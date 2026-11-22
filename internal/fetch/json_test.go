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

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
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

// A NotFound answer is remembered for NotFoundTTL: the builds after it get
// it back as a cache hit, with no request and no ready (no login), until
// its time is up. A 200 stored later forgets it.
func TestNotFoundRemembered(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{status: http.StatusNotFound, body: `{"Error": "Resource not found"}`}
	c.SetTransport(net)
	r := Request{URL: "https://api.example/a?api_key=secret", Key: "a.json", TTL: time.Hour,
		NotFoundTTL: 24 * time.Hour, NotFound: []int{http.StatusNotFound}}
	readies := 0
	ready := func(r Request) (Request, error) { readies++; return r, nil }
	ctx, events := traced()
	for build := 1; build <= 3; build++ {
		var out struct{ A int }
		if err := c.GetJSON(ctx, r, &out, ready); !IsNotFound(err) {
			t.Fatalf("build %d: %v, want the 404", build, err)
		}
		if n := net.sent.Load(); n != 1 {
			t.Errorf("build %d: %d requests sent, want 1", build, n)
		}
	}
	if readies != 1 {
		t.Errorf("ready ran %d times, want 1 (not for a remembered answer)", readies)
	}
	ev := events()
	if got := kinds(ev); !slices.Equal(got, []Kind{KindFail, KindCache, KindCache}) {
		t.Errorf("kinds %v, want fail, then cache hits", got)
	}
	if err := ev[2].Err; !IsNotFound(err) || strings.Contains(err.Error(), "secret") {
		t.Errorf("remembered error %v, want the 404 with its URL redacted", err)
	}
	if _, err := c.Get(context.Background(), r); !IsNotFound(err) || net.sent.Load() != 1 {
		t.Errorf("Get: %v, %d sent; want the remembered 404 and no request", err, net.sent.Load())
	}

	// Its time is up: asked again, and remembered again.
	backdate(t, c.notFoundPath(r.Key), r.NotFoundTTL)
	var out struct{ A int }
	if err := c.GetJSON(context.Background(), r, &out, nil); !IsNotFound(err) || net.sent.Load() != 2 {
		t.Errorf("after NotFoundTTL: %v, %d sent; want the 404 asked again", err, net.sent.Load())
	}
	if err := c.GetJSON(context.Background(), r, &out, nil); !IsNotFound(err) || net.sent.Load() != 2 {
		t.Errorf("then: %v, %d sent; want it remembered again", err, net.sent.Load())
	}

	// The source has it now: the 200 is stored and the 404 forgotten.
	backdate(t, c.notFoundPath(r.Key), r.NotFoundTTL)
	net.status, net.body = http.StatusOK, `{"A": 1}`
	if err := c.GetJSON(context.Background(), r, &out, nil); err != nil || out.A != 1 {
		t.Fatalf("200: %+v %v", out, err)
	}
	if _, err := os.Stat(c.notFoundPath(r.Key)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the 404 is still remembered after a 200 (stat: %v)", err)
	}
}

// A caller that found nothing in the cache, but reaches the download only
// once another caller's download of the same key has stored its answer (a
// copy, or a remembered 404) and ended, gets that answer: one request, not
// two. The second caller's ready holds it in that gap, as a slow goroutine
// would (a movie's text and details, read side by side in one build).
func TestDownloadEndedWhileLooking(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"copy", http.StatusOK, `{"A": 1}`},
		{"not found", http.StatusNotFound, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(t.TempDir())
			net := &bodyNet{status: tc.status, body: tc.body}
			c.SetTransport(net)
			r := Request{URL: "https://api.example/a", Key: "a.json", TTL: time.Hour,
				NotFoundTTL: 24 * time.Hour, NotFound: []int{http.StatusNotFound}}
			inReady, goOn := make(chan struct{}), make(chan struct{})
			var once sync.Once
			hold := func(req Request) (Request, error) {
				once.Do(func() { close(inReady); <-goOn })
				return req, nil
			}
			second := make(chan error, 1)
			var a, b struct{ A int }
			ctx, events := traced()
			go func() { second <- c.GetJSON(ctx, r, &b, hold) }()
			<-inReady
			firstErr := c.GetJSON(context.Background(), r, &a, nil) // its download has ended when it returns
			close(goOn)
			secondErr := <-second
			if n := net.sent.Load(); n != 1 {
				t.Errorf("%d requests sent, want 1", n)
			}
			if got := kinds(events()); !slices.Equal(got, []Kind{KindCache}) {
				t.Errorf("second caller's reads %v, want one cache hit", got)
			}
			if tc.status == http.StatusOK {
				if firstErr != nil || secondErr != nil || a.A != 1 || b.A != 1 {
					t.Errorf("first %+v %v, second %+v %v; want both the copy", a, firstErr, b, secondErr)
				}
			} else if !IsNotFound(firstErr) || !IsNotFound(secondErr) {
				t.Errorf("first %v, second %v; want both the 404", firstErr, secondErr)
			}
		})
	}
}

// Only the listed statuses are remembered, and only with NotFoundTTL, Key
// and TTL.
func TestNotFoundOnlyListed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		r      Request
	}{
		{"status not listed", http.StatusInternalServerError, Request{Key: "b.json", TTL: time.Hour, NotFoundTTL: time.Hour, NotFound: []int{404}}},
		{"no NotFoundTTL", http.StatusNotFound, Request{Key: "b.json", TTL: time.Hour, NotFound: []int{404}}},
		{"no TTL", http.StatusNotFound, Request{Key: "b.json", NotFoundTTL: time.Hour, NotFound: []int{404}}},
		{"no Key", http.StatusNotFound, Request{TTL: time.Hour, NotFoundTTL: time.Hour, NotFound: []int{404}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(t.TempDir())
			net := &bodyNet{status: tc.status}
			c.SetTransport(net)
			r := tc.r
			r.URL, r.Quiet = "https://api.example/b", true
			for range 2 {
				_, _ = c.Get(context.Background(), r)
			}
			if n := net.sent.Load(); n != 2 {
				t.Errorf("%d requests sent, want 2 (nothing remembered)", n)
			}
		})
	}
}

// An expired copy still answers when the source now says "nothing there"
// (a deleted record): the remembered 404 never hides it. The source is
// asked again at each build, as for any failed download.
func TestNotFoundStaleCopyAnswers(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{body: `{"A": 1}`}
	c.SetTransport(net)
	r := Request{URL: "https://api.example/c", Key: "c.json", TTL: time.Hour, NotFoundTTL: 24 * time.Hour, NotFound: []int{404}}
	var out struct{ A int }
	if err := c.GetJSON(context.Background(), r, &out, nil); err != nil {
		t.Fatal(err)
	}
	expire(t, c, r.Key)
	net.status = http.StatusNotFound
	for build := 1; build <= 3; build++ {
		out.A = 0
		if err := c.GetJSON(context.Background(), r, &out, nil); err != nil || out.A != 1 {
			t.Errorf("build %d: %+v %v, want the stale copy", build, out, err)
		}
		if n := net.sent.Load(); n != int32(1+build) {
			t.Errorf("build %d: %d requests sent, want %d", build, n, 1+build)
		}
	}
}

// TestNotFoundStaleCopyAnswers through Get, the other read function: the
// owner's rule holds for both.
func TestNotFoundStaleCopyAnswersGet(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{body: `{"A": 1}`}
	c.SetTransport(net)
	r := Request{URL: "https://api.example/c", Key: "c.json", TTL: time.Hour, NotFoundTTL: 24 * time.Hour, NotFound: []int{404}}
	if _, err := c.Get(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	expire(t, c, r.Key)
	net.status = http.StatusNotFound
	for build := 1; build <= 3; build++ {
		data, err := c.Get(context.Background(), r)
		if err != nil || string(data) != `{"A": 1}` {
			t.Errorf("build %d: %q %v, want the stale copy", build, data, err)
		}
		if n := net.sent.Load(); n != int32(1+build) {
			t.Errorf("build %d: %d requests sent, want %d", build, n, 1+build)
		}
	}
}

// A fresh copy answers even with a remembered "nothing there" beside it (a
// file left behind, say by a crash before a 200 removed it): the copy is
// read first, and the .notfound file is not even looked at. Both read
// functions.
func TestFreshCopyBesideNotFound(t *testing.T) {
	const msg = "remembered not found"
	for _, name := range []string{"Get", "GetJSON"} {
		t.Run(name, func(t *testing.T) {
			c := New(t.TempDir())
			net := &bodyNet{status: http.StatusNotFound}
			c.SetTransport(net)
			r := Request{URL: "https://api.example/f", Key: "f.json", TTL: time.Hour,
				NotFoundTTL: 24 * time.Hour, NotFound: []int{http.StatusNotFound}}
			if err := WriteFile(c.path(r.Key), []byte(`{"A": 1}`)); err != nil {
				t.Fatal(err)
			}
			if err := WriteFile(c.notFoundPath(r.Key), []byte("404 https://api.example/f")); err != nil {
				t.Fatal(err)
			}
			logs := &lockedBuffer{}
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(old) })
			ctx, events := traced()
			if name == "Get" {
				data, err := c.Get(ctx, r)
				if err != nil || string(data) != `{"A": 1}` {
					t.Errorf("Get: %q %v, want the fresh copy", data, err)
				}
			} else {
				var out struct{ A int }
				if err := c.GetJSON(ctx, r, &out, nil); err != nil || out.A != 1 {
					t.Errorf("GetJSON: %+v %v, want the fresh copy", out, err)
				}
			}
			if n := net.sent.Load(); n != 0 {
				t.Errorf("%d requests sent, want none", n)
			}
			ev := events()
			if len(ev) != 1 || ev[0].Kind != KindCache || ev[0].Err != nil {
				t.Errorf("events %+v, want one cache hit without an error", ev)
			}
			if n := logs.count(msg); n != 0 {
				t.Errorf("%d %q lines, want none (the .notfound not consulted):\n%s", n, msg, logs)
			}
		})
	}
}

// At debug level, each use of a remembered "nothing there" logs one line
// with the key, the status and when it is asked again, never the URL, so
// never its api key.
// At the normal level nothing is logged.
func TestNotFoundRememberedLogged(t *testing.T) {
	const msg = "remembered not found"
	setup := func(t *testing.T) (*Client, Request) {
		c := New(t.TempDir())
		c.SetTransport(&bodyNet{status: http.StatusNotFound})
		r := Request{URL: "https://api.example/d?api_key=secret", Key: "tmdb/d.json", TTL: time.Hour,
			NotFoundTTL: 24 * time.Hour, NotFound: []int{http.StatusNotFound}}
		if _, err := c.Get(context.Background(), r); !IsNotFound(err) {
			t.Fatalf("first request: %v, want the 404", err)
		}
		// Two hours old: at least 2 h have gone, so at most 22 h are left.
		old := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(c.notFoundPath(r.Key), old, old); err != nil {
			t.Fatal(err)
		}
		return c, r
	}

	t.Run("debug", func(t *testing.T) {
		c, r := setup(t)
		logs := &lockedBuffer{}
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(old) })
		var out struct{ A int }
		if err := c.GetJSON(context.Background(), r, &out, nil); !IsNotFound(err) {
			t.Fatalf("GetJSON: %v, want the remembered 404", err)
		}
		if _, err := c.Get(context.Background(), r); !IsNotFound(err) {
			t.Fatalf("Get: %v, want the remembered 404", err)
		}
		if n := logs.count(msg); n != 2 {
			t.Fatalf("%d %q lines, want 2 (one per use):\n%s", n, msg, logs)
		}
		var line string
		for l := range strings.Lines(logs.String()) {
			if strings.Contains(l, `msg="`+msg+`"`) {
				line = l
				break
			}
		}
		for _, want := range []string{"level=DEBUG", "key=tmdb/d.json", "status=404"} {
			if !strings.Contains(line, want) {
				t.Errorf("line %q lacks %q", line, want)
			}
		}
		if strings.Contains(logs.String(), "secret") || strings.Contains(line, "url=") {
			t.Errorf("line %q names the URL", line)
		}
		_, again, _ := strings.Cut(line, "asked_again_in=")
		again, _, _ = strings.Cut(again, " ")
		d, err := time.ParseDuration(strings.TrimSpace(again))
		if err != nil || d <= 0 || d > 22*time.Hour {
			t.Errorf("asked_again_in %q (%v), want a time left of at most 22h", again, err)
		}
	})

	t.Run("normal level", func(t *testing.T) {
		c, r := setup(t)
		logs := captureLogs(t)
		var out struct{ A int }
		if err := c.GetJSON(context.Background(), r, &out, nil); !IsNotFound(err) {
			t.Fatalf("GetJSON: %v, want the remembered 404", err)
		}
		if n := logs.count(msg); n != 0 { // counted: another test's late goroutine may log something else
			t.Errorf("%d %q lines at the normal level, want none:\n%s", n, msg, logs)
		}
	})
}

// backdate makes the file at path older than age, by an hour: no clock or
// file system rounding leaves it younger.
func backdate(t *testing.T, path string, age time.Duration) {
	t.Helper()
	old := time.Now().Add(-age - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}
