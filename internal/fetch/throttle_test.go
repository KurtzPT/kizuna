package fetch

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// releases runs each ticket's wait in its own goroutine and returns the
// names in the order they were let through, with the times.
func releases(names []string, tickets []*ticket) ([]string, []time.Time) {
	var mu sync.Mutex
	var order []string
	var at []time.Time
	var wg sync.WaitGroup
	for i, t := range tickets {
		wg.Go(func() {
			t.wait()
			mu.Lock()
			order = append(order, names[i])
			at = append(at, time.Now())
			mu.Unlock()
		})
	}
	wg.Wait()
	return order, at
}

// A foreground request queued after background ones goes first; then the
// background ones go in arrival order.
func TestThrottleForegroundFirst(t *testing.T) {
	l := newHostLimit(30 * time.Millisecond)
	l.enqueue(false).wait() // the slot is now taken: the rest queue up
	tickets := []*ticket{l.enqueue(true), l.enqueue(true), l.enqueue(false)}
	order, _ := releases([]string{"bg1", "bg2", "fg"}, tickets)
	if got := strings.Join(order, ","); got != "fg,bg1,bg2" {
		t.Errorf("order %s, want fg,bg1,bg2", got)
	}
}

// A foreground caller joining a queued background download moves it ahead
// of the other background ones.
func TestThrottlePromote(t *testing.T) {
	l := newHostLimit(30 * time.Millisecond)
	l.enqueue(false).wait()
	bg1, bg2 := l.enqueue(true), l.enqueue(true)
	bg2.promote()
	order, _ := releases([]string{"bg1", "bg2"}, []*ticket{bg1, bg2})
	if got := strings.Join(order, ","); got != "bg2,bg1" {
		t.Errorf("order %s, want bg2,bg1", got)
	}
}

// The gap holds between every two requests let through, whatever mix of
// foreground, background and promoted requests arrives, and also while
// requests keep arriving during the waits.
func TestThrottleSpacingHolds(t *testing.T) {
	const gap = 25 * time.Millisecond
	l := newHostLimit(gap)
	var mu sync.Mutex
	var at []time.Time
	var wg sync.WaitGroup
	for i := range 12 {
		tk := l.enqueue(i%3 != 0)
		wg.Go(func() {
			tk.wait()
			mu.Lock()
			at = append(at, time.Now())
			mu.Unlock()
		})
		if i%4 == 1 {
			tk.promote()
		}
		if i%5 == 0 {
			time.Sleep(gap / 2) // some arrive while others wait
		}
	}
	wg.Wait()
	for i := 1; i < len(at); i++ {
		if d := at[i].Sub(at[i-1]); d < gap {
			t.Errorf("requests %d and %d let through %v apart, want at least %v", i-1, i, d, gap)
		}
	}
}

// Through the client: background downloads queued on a throttled host
// don't hold up a foreground download that arrives after them.
func TestBackgroundRequestsYield(t *testing.T) {
	c := New(t.TempDir())
	c.Throttle("api.example", 200*time.Millisecond) // wide enough to queue all four before the second slot
	var mu sync.Mutex
	var hits []string
	c.SetTransport(roundTrip(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}, Request: r}, nil
	}))
	queued := func(n int) {
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
			l := c.limits["api.example"]
			l.mu.Lock()
			got := len(l.fg) + len(l.bg)
			l.mu.Unlock()
			if got >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("never %d requests queued", n)
			}
		}
	}
	get := func(path string, background bool) {
		if _, err := c.Get(context.Background(), Request{URL: "http://api.example" + path, Background: background}); err != nil {
			t.Error(err)
		}
	}
	get("/first", false) // takes the slot: the rest must wait
	var wg sync.WaitGroup
	for i, p := range []string{"/bg1", "/bg2", "/bg3"} {
		wg.Go(func() { get(p, true) })
		queued(i + 1)
	}
	wg.Go(func() { get("/fg", false) })
	wg.Wait()
	if got := strings.Join(hits, ","); got != "/first,/fg,/bg1,/bg2,/bg3" {
		t.Errorf("order %s, want the foreground request right after the first", got)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
