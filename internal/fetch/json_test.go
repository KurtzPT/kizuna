package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A 200 answer that isn't JSON (a captive portal's page) is neither cached
// nor decoded; with an older copy cached, that one answers.
func TestGetJSONRejectsNonJSON(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{body: `<html>Log in to the hotel wifi</html>`}
	c.SetTransport(net)
	r := Request{URL: "https://api.example/x", Key: "x.json", TTL: time.Hour}
	var out struct{ A int }
	if err := c.GetJSON(context.Background(), r, &out, nil); !errors.Is(err, errNotJSON) {
		t.Fatalf("err %v, want not JSON", err)
	}
	if _, _, ok := c.Cached(r.Key); ok {
		t.Fatal("the HTML page was cached")
	}

	net.body = `{"A": 1}`
	if err := c.GetJSON(context.Background(), r, &out, nil); err != nil || out.A != 1 {
		t.Fatalf("JSON answer: %+v, %v", out, err)
	}
	expire(t, c, r.Key)
	net.body = `<html>again</html>`
	out.A = 0
	if err := c.GetJSON(context.Background(), r, &out, nil); err != nil || out.A != 1 {
		t.Errorf("non-JSON with a stale copy: %+v, %v; want the stale copy", out, err)
	}
}

// The request's own Validate still runs, before the answer is decoded.
func TestGetJSONKeepsOwnValidate(t *testing.T) {
	c := New(t.TempDir())
	c.SetTransport(&bodyNet{body: `{"error": "banned"}`})
	banned := errors.New("banned")
	r := Request{URL: "https://api.example/y", Key: "y.json", TTL: time.Hour,
		Validate: func(data []byte) error {
			if strings.Contains(string(data), "banned") {
				return banned
			}
			return nil
		}}
	var out map[string]string
	if err := c.GetJSON(context.Background(), r, &out, nil); !errors.Is(err, banned) {
		t.Errorf("err %v, want the request's own Validate error", err)
	}
}

// JSON of another shape (an error object where a list belongs) is rejected
// like a page that isn't JSON: not cached, and an older copy answers.
func TestGetJSONRejectsWrongShape(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{body: `{"status": "failure", "message": "rate limited"}`}
	c.SetTransport(net)
	r := Request{URL: "https://api.example/list", Key: "list.json", TTL: time.Hour}
	var out []int
	var ute *json.UnmarshalTypeError
	if err := c.GetJSON(context.Background(), r, &out, nil); !errors.As(err, &ute) {
		t.Fatalf("err %v, want a type error", err)
	}
	if _, _, ok := c.Cached(r.Key); ok {
		t.Fatal("the error object was cached")
	}

	net.body = `[1, 2]`
	if err := c.GetJSON(context.Background(), r, &out, nil); err != nil || len(out) != 2 {
		t.Fatalf("list answer: %v, %v", out, err)
	}
	expire(t, c, r.Key)
	net.body = `{"status": "failure"}`
	out = nil
	if err := c.GetJSON(context.Background(), r, &out, nil); err != nil || len(out) != 2 {
		t.Errorf("error object with a stale copy: %v, %v; want the stale copy", out, err)
	}
}

// decodes counts how often a counted is decoded.
var decodes atomic.Int32

type counted struct{ A int }

func (c *counted) UnmarshalJSON(data []byte) error {
	decodes.Add(1)
	var v struct{ A int }
	err := json.Unmarshal(data, &v)
	c.A = v.A
	return err
}

// A downloaded answer is decoded once, by the check that lets it be cached;
// a cached one once, as it is read.
func TestGetJSONDecodesOnce(t *testing.T) {
	c := New(t.TempDir())
	c.SetTransport(&bodyNet{body: `{"A": 7}`})
	r := Request{URL: "https://api.example/z", Key: "z.json", TTL: time.Hour}
	for _, from := range []string{"network", "cache"} {
		decodes.Store(0)
		var out counted
		if err := c.GetJSON(context.Background(), r, &out, nil); err != nil || out.A != 7 || decodes.Load() != 1 {
			t.Errorf("from the %s: %+v, %v, decoded %d times; want A 7, once", from, out, err, decodes.Load())
		}
	}
}

// A caller that joins another's download gets out filled too: the check
// decoded the answer for the first caller only.
func TestGetJSONJoined(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{body: `{"A": 3}`}
	c.SetTransport(net)
	const host = "api.example"
	c.Throttle(host, time.Hour)
	if _, err := c.Get(context.Background(), Request{URL: "https://" + host + "/first"}); err != nil { // takes the hour's slot
		t.Fatal(err)
	}
	l := c.limits[host]
	r := Request{URL: "https://" + host + "/j", Key: "j.json", TTL: time.Hour}
	first, second := make(chan error), make(chan error)
	var a, b struct{ A int }
	go func() {
		bg := r
		bg.Background = true
		first <- c.GetJSON(context.Background(), bg, &a, nil)
	}()
	l.waitQueue(func() bool { return len(l.bg) == 1 })
	go func() { second <- c.GetJSON(context.Background(), r, &b, nil) }()
	l.waitQueue(func() bool { return len(l.fg) == 1 }) // joined: the download moved up
	c.Throttle(host, 0)
	if err := <-first; err != nil || a.A != 3 {
		t.Errorf("first caller: %+v, %v", a, err)
	}
	if err := <-second; err != nil || b.A != 3 {
		t.Errorf("joined caller: %+v, %v", b, err)
	}
	if n := net.sent.Load(); n != 2 {
		t.Errorf("%d requests sent, want 2 (one each for /first and the shared download)", n)
	}
}

// brokenCopy caches body (JSON cut short) under key, fresh, and returns it.
func brokenCopy(t *testing.T, c *Client, key string) string {
	t.Helper()
	const body = `{"A": 1, "B":`
	if err := WriteFile(c.path(key), []byte(body)); err != nil {
		t.Fatal(err)
	}
	return body
}

// cachedBody returns key's cached bytes.
func cachedBody(t *testing.T, c *Client, key string) string {
	t.Helper()
	data, _, ok := c.Cached(key)
	if !ok {
		t.Fatalf("%s: no cached copy", key)
	}
	return string(data)
}

// traced returns a context that records its reads' kinds and errors, and a
// function that gives the ones so far.
func traced() (context.Context, func() []Event) {
	var mu sync.Mutex
	var events []Event
	ctx := WithObserver(context.Background(), func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})
	return ctx, func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(events)
	}
}

// kinds lists events' kinds.
func kinds(events []Event) []Kind {
	var k []Kind
	for _, e := range events {
		k = append(k, e.Kind)
	}
	return k
}

// lockedBuffer is a log handler's output, safe to read while a download's
// goroutine logs.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// count says how many times msg was logged.
func (l *lockedBuffer) count(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), "msg=\""+msg+"\"")
}

// captureLogs sends the default logger to a buffer for the test.
func captureLogs(t *testing.T) *lockedBuffer {
	logs := &lockedBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return logs
}

const (
	brokenFresh = "cached copy doesn't decode; downloading it again"
	brokenStale = "stale cached copy doesn't decode; not used"
)

// A fresh cached copy that doesn't decode is no answer: it is downloaded
// again, once, and the good answer replaces it. The trace shows the
// download only, and the broken copy is logged once.
func TestGetJSONBrokenCopyDownloadedAgain(t *testing.T) {
	logs := captureLogs(t)
	c := New(t.TempDir())
	net := &bodyNet{body: `{"A": 2}`}
	c.SetTransport(net)
	r := Request{URL: "https://api.example/b", Key: "b.json", TTL: time.Hour}
	brokenCopy(t, c, r.Key)
	var out struct{ A int }
	ctx, events := traced()
	if err := c.GetJSON(ctx, r, &out, nil); err != nil || out.A != 2 {
		t.Fatalf("got %+v, %v; want the downloaded answer", out, err)
	}
	if got := kinds(events()); !slices.Equal(got, []Kind{KindNet}) {
		t.Errorf("trace %v, want [net]", got)
	}
	if n := logs.count(brokenFresh); n != 1 {
		t.Errorf("broken copy logged %d times, want 1", n)
	}
	if n := net.sent.Load(); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
	if got := cachedBody(t, c, r.Key); got != net.body {
		t.Errorf("cached %q, want the new answer", got)
	}
	out.A = 0
	if err := c.GetJSON(context.Background(), r, &out, nil); err != nil || out.A != 2 || net.sent.Load() != 1 {
		t.Errorf("next call: %+v, %v, %d requests; want the new copy, from the cache", out, err, net.sent.Load())
	}
}

// With the source failing, a broken copy, fresh or stale, doesn't answer:
// the download's error does, and the file stays for the next try. The trace
// shows the failed read only (no cache hit, no stale answer), and the broken
// copy is logged once.
func TestGetJSONBrokenCopySourceDown(t *testing.T) {
	for _, stale := range []bool{false, true} {
		logs := captureLogs(t)
		c := New(t.TempDir())
		net := &bodyNet{status: http.StatusServiceUnavailable, body: `{}`}
		c.SetTransport(net)
		r := Request{URL: "https://api.example/d", Key: "d.json", TTL: time.Hour}
		body := brokenCopy(t, c, r.Key)
		if stale {
			expire(t, c, r.Key)
		}
		var out struct{ A int }
		var se *StatusError
		ctx, events := traced()
		if err := c.GetJSON(ctx, r, &out, nil); !errors.As(err, &se) || se.Code != http.StatusServiceUnavailable {
			t.Errorf("stale %v: err %v, want the 503", stale, err)
		}
		if ev := events(); !slices.Equal(kinds(ev), []Kind{KindFail}) || !errors.As(ev[0].Err, &se) {
			t.Errorf("stale %v: trace %v, want one fail with the 503", stale, ev)
		}
		want := map[string]int{brokenFresh: 1, brokenStale: 0}
		if stale {
			want = map[string]int{brokenFresh: 0, brokenStale: 1}
		}
		for msg, n := range want {
			if got := logs.count(msg); got != n {
				t.Errorf("stale %v: %q logged %d times, want %d", stale, msg, got, n)
			}
		}
		if out.A != 0 {
			t.Errorf("stale %v: out %+v, want nothing of the broken copy", stale, out)
		}
		if n := net.sent.Load(); n != 1 {
			t.Errorf("stale %v: %d requests, want 1", stale, n)
		}
		if got := cachedBody(t, c, r.Key); got != body {
			t.Errorf("stale %v: cached %q, want the broken copy kept", stale, got)
		}
	}
}

// A source that answers broken JSON again is asked once more per call, not
// in a loop, and its answer replaces nothing.
func TestGetJSONBrokenAnswerTwice(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{body: `<html>busy</html>`}
	c.SetTransport(net)
	r := Request{URL: "https://api.example/t", Key: "t.json", TTL: time.Hour}
	body := brokenCopy(t, c, r.Key)
	var out struct{ A int }
	for call := int32(1); call <= 2; call++ {
		if err := c.GetJSON(context.Background(), r, &out, nil); !errors.Is(err, errNotJSON) {
			t.Errorf("call %d: err %v, want not JSON", call, err)
		}
		if n := net.sent.Load(); n != call {
			t.Errorf("call %d: %d requests in all, want %d", call, n, call)
		}
	}
	if got := cachedBody(t, c, r.Key); got != body {
		t.Errorf("cached %q, want the first copy kept", got)
	}
}

// ready runs only when the cache has no fresh answer. Its error fails the
// call with nothing sent: a stale copy answers unless the request it returns
// has NoStale, and a broken fresh copy never does.
func TestGetJSONReady(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{body: `{"A": 9}`}
	c.SetTransport(net)
	down := errors.New("login failed")
	readies := 0
	fail := func(noStale bool) func(Request) (Request, error) {
		return func(r Request) (Request, error) {
			readies++
			r.NoStale = noStale
			return r, down
		}
	}
	ctx := context.Background()
	r := Request{URL: "https://api.example/r", Key: "r.json", TTL: time.Hour}
	if err := WriteFile(c.path(r.Key), []byte(`{"A": 1}`)); err != nil {
		t.Fatal(err)
	}
	var out struct{ A int }
	if err := c.GetJSON(ctx, r, &out, fail(false)); err != nil || out.A != 1 || readies != 0 {
		t.Errorf("fresh copy: %+v, %v, ready ran %d times; want the copy, no ready", out, err, readies)
	}
	expire(t, c, r.Key)
	if err := c.GetJSON(ctx, r, &out, fail(false)); err != nil || out.A != 1 {
		t.Errorf("stale copy: %+v, %v; want the stale copy", out, err)
	}
	if err := c.GetJSON(ctx, r, &out, fail(true)); !errors.Is(err, down) {
		t.Errorf("stale copy, NoStale: err %v, want ready's error", err)
	}
	brokenCopy(t, c, r.Key)
	if err := c.GetJSON(ctx, r, &out, fail(false)); !errors.Is(err, down) {
		t.Errorf("broken copy: err %v, want ready's error", err)
	}
	if n := net.sent.Load(); n != 0 {
		t.Errorf("%d requests, want none", n)
	}
	ok := func(r Request) (Request, error) {
		r.URL = "https://api.example/elsewhere" // what ready returns is what is sent
		return r, nil
	}
	if err := c.GetJSON(ctx, r, &out, ok); err != nil || out.A != 9 || net.sent.Load() != 1 {
		t.Errorf("broken copy, ready ok: %+v, %v, %d requests; want the download", out, err, net.sent.Load())
	}
}

// out is replaced, not merged into, whichever way the answer comes: a field
// only the first value had is gone after the second, and a copy that
// doesn't decode leaves nothing of itself.
func TestGetJSONReplacesOut(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{status: http.StatusServiceUnavailable}
	c.SetTransport(net)
	ctx := context.Background()
	cache := func(key, body string, stale bool) Request {
		t.Helper()
		if err := WriteFile(c.path(key), []byte(body)); err != nil {
			t.Fatal(err)
		}
		if stale {
			expire(t, c, key)
		}
		return Request{URL: "https://api.example/" + key, Key: key, TTL: time.Hour}
	}
	type pair struct{ A, B int }
	for _, from := range []string{"cache", "stale", "network"} {
		out := pair{A: 1, B: 2}
		r := cache(from+".json", `{"A": 3}`, from == "stale")
		if from == "network" {
			r.Key = ""
			net.status, net.body = 0, `{"A": 3}`
		}
		if err := c.GetJSON(ctx, r, &out, nil); err != nil || out != (pair{A: 3}) {
			t.Errorf("from the %s: %+v, %v; want {A:3 B:0}", from, out, err)
		}
	}
	net.status = http.StatusServiceUnavailable
	out := pair{A: 1, B: 2}
	r := cache("half.json", `{"A": 5, "B": "x"}`, false) // A decodes, B doesn't
	if err := c.GetJSON(ctx, r, &out, nil); err == nil || out != (pair{}) {
		t.Errorf("half a decode: %+v, %v; want an error and nothing in out", out, err)
	}
}

// A broken fresh copy of a Revalidate key loses its ETag before the new
// download: the request is a full one (no If-None-Match, so no 304 vouches
// for the broken bytes), and the new answer is saved with its own ETag.
func TestGetJSONBrokenCopyRevalidate(t *testing.T) {
	srv := &etagServer{body: `{"A": 1}`, etag: `"a1"`}
	c := New(t.TempDir())
	r := Request{URL: srv.start(t), Key: "lists/list.json", TTL: time.Hour, Revalidate: true}
	ctx := context.Background()
	var out struct{ A int }
	if err := c.GetJSON(ctx, r, &out, nil); err != nil || out.A != 1 {
		t.Fatalf("first get: %+v, %v", out, err)
	}
	brokenCopy(t, c, r.Key) // the ETag file stays as it was
	srv.set(`{"A": 2}`, `"b2"`)
	if err := c.GetJSON(ctx, r, &out, nil); err != nil || out.A != 2 {
		t.Fatalf("broken copy: %+v, %v; want the new answer", out, err)
	}
	if inm := srv.sent(1); inm != "" {
		t.Errorf("If-None-Match %q on the new download, want none", inm)
	}
	if got := cachedBody(t, c, r.Key); got != `{"A": 2}` {
		t.Errorf("cached %q, want the new answer", got)
	}
	if tag, err := os.ReadFile(c.etagPath(r.Key)); err != nil || string(tag) != `"b2"` {
		t.Errorf("etag %q, %v; want the new answer's", tag, err)
	}
}

// An out GetJSON can't decode into (nil, not a pointer) is the caller's
// error: a good fresh copy is not downloaded again, nor rewritten.
func TestGetJSONBadOutNotABrokenCopy(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{body: `{"A": 2}`}
	c.SetTransport(net)
	r := Request{URL: "https://api.example/o", Key: "o.json", TTL: time.Hour}
	if err := WriteFile(c.path(r.Key), []byte(`{"A": 1}`)); err != nil {
		t.Fatal(err)
	}
	var notPointer struct{ A int }
	for name, out := range map[string]any{"nil": nil, "not a pointer": notPointer} {
		var ie *json.InvalidUnmarshalError
		if err := c.GetJSON(context.Background(), r, out, nil); !errors.As(err, &ie) {
			t.Errorf("%s: err %v, want an invalid unmarshal error", name, err)
		}
	}
	if n := net.sent.Load(); n != 0 {
		t.Errorf("%d requests, want none", n)
	}
	if got := cachedBody(t, c, r.Key); got != `{"A": 1}` {
		t.Errorf("cached %q, want the good copy kept", got)
	}
}
