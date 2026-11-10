package fetch

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordReleases makes l report each let-through request's name (from
// names) under its lock, so the order is exact whatever the goroutines do.
func recordReleases(l *hostLimit, names map[*ticket]string) func() []string {
	var order []string
	l.mu.Lock()
	l.released = func(t *ticket, _ time.Time) { order = append(order, names[t]) }
	l.mu.Unlock()
	return func() []string {
		l.mu.Lock()
		defer l.mu.Unlock()
		return order
	}
}

// open drops l's gap: requests queued behind an hour-long gap (so none
// could slip through while the test queued them) now go, one at a time, in
// the order the queue gives.
func open(l *hostLimit) { l.setGap(0) }

// releases opens l, waits for every ticket in its own goroutine and returns
// the names in the order they were let through.
func releases(l *hostLimit, names []string, tickets []*ticket) []string {
	byTicket := map[*ticket]string{}
	for i, t := range tickets {
		byTicket[t] = names[i]
	}
	order := recordReleases(l, byTicket)
	open(l)
	var wg sync.WaitGroup
	for _, t := range tickets {
		wg.Go(func() { send(t) })
	}
	wg.Wait()
	return order()
}

// send waits for t's turn and sends its request at once.
func send(t *ticket) {
	t.wait()
	t.sent()
}

// A foreground request queued after background ones goes first; then the
// background ones go in arrival order.
func TestThrottleForegroundFirst(t *testing.T) {
	l := newHostLimit(time.Hour)
	send(l.enqueue(false)) // the slot is now taken: the rest queue up
	tickets := []*ticket{l.enqueue(true), l.enqueue(true), l.enqueue(false)}
	order := releases(l, []string{"bg1", "bg2", "fg"}, tickets)
	if got := strings.Join(order, ","); got != "fg,bg1,bg2" {
		t.Errorf("order %s, want fg,bg1,bg2", got)
	}
}

// A foreground caller joining a queued background download moves it ahead
// of the other background ones.
func TestThrottlePromote(t *testing.T) {
	l := newHostLimit(time.Hour)
	send(l.enqueue(false))
	bg1, bg2 := l.enqueue(true), l.enqueue(true)
	bg2.promote()
	order := releases(l, []string{"bg1", "bg2"}, []*ticket{bg1, bg2})
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
	var at []time.Time                                               // let-through times, recorded under the limit's lock
	l.released = func(_ *ticket, t time.Time) { at = append(at, t) } // so their order and spacing are exact
	var wg sync.WaitGroup
	for i := range 12 {
		tk := l.enqueue(i%3 != 0)
		wg.Go(func() { send(tk) })
		if i%4 == 1 {
			tk.promote()
		}
		if i%5 == 0 {
			time.Sleep(gap / 2) // some arrive while others wait
		}
	}
	wg.Wait()
	if len(at) != 12 {
		t.Fatalf("%d requests let through, want 12", len(at))
	}
	for i := 1; i < len(at); i++ {
		if d := at[i].Sub(at[i-1]); d < gap {
			t.Errorf("requests %d and %d let through %v apart, want at least %v", i-1, i, d, gap)
		}
	}
}

// The next request isn't let through while the one let through before it
// hasn't gone out (its Before is slow), and the gap counts from when it
// went out. Only a lower bound is checked; the pause only gives a wrong
// release its chance.
func TestThrottleGapFromSent(t *testing.T) {
	const gap = 30 * time.Millisecond
	l := newHostLimit(gap)
	var second time.Time
	a, b := l.enqueue(false), l.enqueue(false)
	l.released = func(tk *ticket, at time.Time) {
		if tk == b {
			second = at
		}
	}
	a.wait()
	done := make(chan struct{})
	go func() { send(b); close(done) }()
	time.Sleep(3 * gap) // a's Before takes its time
	sentAt := time.Now()
	a.sent()
	<-done
	if d := second.Sub(sentAt); d < gap {
		t.Errorf("second request let through %v after the first went out, want at least %v", d, gap)
	}
}

// ThrottleFrom carries an earlier run's last request over: the first
// request waits until gap after it. An earlier time than the one known is
// ignored.
func TestThrottleFrom(t *testing.T) {
	const gap = 50 * time.Millisecond
	c := New(t.TempDir())
	last := time.Now()
	c.ThrottleFrom("api.example", gap, last)
	c.ThrottleFrom("api.example", gap, last.Add(-time.Hour))
	l := c.limits["api.example"]
	var at time.Time
	l.released = func(_ *ticket, t time.Time) { at = t }
	send(l.enqueue(false))
	if at.Before(last.Add(gap)) {
		t.Errorf("first request let through %v after the last one, want at least %v", at.Sub(last), gap)
	}
}

// ThrottleAll changes every throttled host's gap.
func TestThrottleAll(t *testing.T) {
	c := New(t.TempDir())
	c.Throttle("a.example", time.Hour)
	c.Throttle("b.example", time.Minute)
	c.ThrottleAll(0)
	for _, h := range []string{"a.example", "b.example"} {
		l := c.limits[h]
		l.mu.Lock()
		gap := l.gap
		l.mu.Unlock()
		if gap != 0 {
			t.Errorf("%s: gap %v, want 0", h, gap)
		}
	}
}

// Through the client: background downloads queued on a throttled host
// don't hold up a foreground download that arrives after them.
func TestBackgroundRequestsYield(t *testing.T) {
	c := New(t.TempDir())
	c.Throttle("api.example", time.Hour) // nothing slips through while the test queues; open lets them go
	c.SetTransport(&bodyNet{body: "ok"})
	l := c.limits["api.example"]
	names := map[*ticket]string{}
	queue := func(path string, background bool, wg *sync.WaitGroup) {
		n := l.queued()
		wg.Go(func() {
			if _, err := c.Get(context.Background(), Request{URL: "http://api.example" + path, Key: path, Background: background}); err != nil {
				t.Error(err)
			}
		})
		l.waitQueue(func() bool { return len(l.fg)+len(l.bg) > n })
		c.mu.Lock()
		names[c.inflight[path].ticket] = path
		c.mu.Unlock()
	}
	var wg sync.WaitGroup
	if _, err := c.Get(context.Background(), Request{URL: "http://api.example/first"}); err != nil { // takes the slot
		t.Fatal(err)
	}
	for _, p := range []string{"/bg1", "/bg2", "/bg3"} {
		queue(p, true, &wg)
	}
	queue("/fg", false, &wg)
	order := recordReleases(l, names)
	open(l)
	wg.Wait()
	if got := strings.Join(order(), ","); got != "/fg,/bg1,/bg2,/bg3" {
		t.Errorf("order %s, want the foreground request first", got)
	}
}

// queued is how many requests wait in l's queues.
func (l *hostLimit) queued() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.fg) + len(l.bg)
}

// waitQueue blocks until done, called with l.mu held, says l's queues are as
// wanted (every change to them signals).
func (l *hostLimit) waitQueue(done func() bool) {
	l.mu.Lock()
	for !done() {
		changed := l.changed
		l.mu.Unlock()
		<-changed
		l.mu.Lock()
	}
	l.mu.Unlock()
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
