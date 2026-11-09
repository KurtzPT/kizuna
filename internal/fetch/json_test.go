package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// bodyNet answers every request with its body.
type bodyNet struct{ body string }

func (b *bodyNet) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(b.body)), Header: http.Header{}, Request: r}, nil
}

// A 200 answer that isn't JSON (a captive portal's page) is neither cached
// nor decoded; with an older copy cached, that one answers.
func TestGetJSONRejectsNonJSON(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{body: `<html>Log in to the hotel wifi</html>`}
	c.SetTransport(net)
	r := Request{URL: "https://api.example/x", Key: "x.json", TTL: time.Hour}
	var out struct{ A int }
	if err := c.GetJSON(context.Background(), r, &out); !errors.Is(err, errNotJSON) {
		t.Fatalf("err %v, want not JSON", err)
	}
	if _, _, ok := c.Cached(r.Key); ok {
		t.Fatal("the HTML page was cached")
	}

	net.body = `{"A": 1}`
	if err := c.GetJSON(context.Background(), r, &out); err != nil || out.A != 1 {
		t.Fatalf("JSON answer: %+v, %v", out, err)
	}
	expire(t, c, r.Key)
	net.body = `<html>again</html>`
	out.A = 0
	if err := c.GetJSON(context.Background(), r, &out); err != nil || out.A != 1 {
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
	if err := c.GetJSON(context.Background(), r, &out); !errors.Is(err, banned) {
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
	if err := c.GetJSON(context.Background(), r, &out); !errors.As(err, &ute) {
		t.Fatalf("err %v, want a type error", err)
	}
	if _, _, ok := c.Cached(r.Key); ok {
		t.Fatal("the error object was cached")
	}

	net.body = `[1, 2]`
	if err := c.GetJSON(context.Background(), r, &out); err != nil || len(out) != 2 {
		t.Fatalf("list answer: %v, %v", out, err)
	}
	expire(t, c, r.Key)
	net.body = `{"status": "failure"}`
	out = nil
	if err := c.GetJSON(context.Background(), r, &out); err != nil || len(out) != 2 {
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
		if err := c.GetJSON(context.Background(), r, &out); err != nil || out.A != 7 || decodes.Load() != 1 {
			t.Errorf("from the %s: %+v, %v, decoded %d times; want A 7, once", from, out, err, decodes.Load())
		}
	}
}

// A caller that joins another's download gets out filled too: the check
// decoded the answer for the first caller only.
func TestGetJSONJoined(t *testing.T) {
	c := New(t.TempDir())
	var sent atomic.Int32
	c.SetTransport(roundTrip(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		return okTransport(`{"A": 3}`)(r)
	}))
	const host = "api.example"
	c.Throttle(host, time.Hour)
	if _, err := c.Get(context.Background(), Request{URL: "https://" + host + "/first"}); err != nil { // takes the hour's slot
		t.Fatal(err)
	}
	l := c.limits[host]
	waitQueue := func(done func() bool) { // until done says the queue is as wanted
		l.mu.Lock()
		for !done() {
			changed := l.changed
			l.mu.Unlock()
			<-changed
			l.mu.Lock()
		}
		l.mu.Unlock()
	}
	r := Request{URL: "https://" + host + "/j", Key: "j.json", TTL: time.Hour}
	first, second := make(chan error), make(chan error)
	var a, b struct{ A int }
	go func() {
		bg := r
		bg.Background = true
		first <- c.GetJSON(context.Background(), bg, &a)
	}()
	waitQueue(func() bool { return len(l.bg) == 1 })
	go func() { second <- c.GetJSON(context.Background(), r, &b) }()
	waitQueue(func() bool { return len(l.fg) == 1 }) // joined: the download moved up
	c.Throttle(host, 0)
	if err := <-first; err != nil || a.A != 3 {
		t.Errorf("first caller: %+v, %v", a, err)
	}
	if err := <-second; err != nil || b.A != 3 {
		t.Errorf("joined caller: %+v, %v", b, err)
	}
	if sent.Load() != 2 {
		t.Errorf("%d requests sent, want 2 (one each for /first and the shared download)", sent.Load())
	}
}
