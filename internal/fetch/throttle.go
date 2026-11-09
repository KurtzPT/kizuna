package fetch

import (
	"slices"
	"sync"
	"time"
)

// hostLimit spaces the requests to one host at least gap apart. Waiting
// requests go out one at a time, foreground ones first, each in arrival
// order: a background request (an AniDB prefetch) only takes a slot when
// no foreground request (one Plex is waiting for) is queued. The gap is
// measured from the moment the previous request was let through, whatever
// the order, so priority never shortens it.
type hostLimit struct {
	mu      sync.Mutex
	gap     time.Duration
	last    time.Time     // when the last request was let through
	fg, bg  []*ticket     // waiting requests, in arrival order
	changed chan struct{} // closed (and replaced) when last or the queues change

	// released, if set, gets each let-through request and time, in order,
	// under mu: tests check the order and spacing there, not after wait
	// returns (a delayed goroutine would make them look wrong).
	released func(*ticket, time.Time)
}

// ticket is one request's place in a hostLimit's queue.
type ticket struct {
	l          *hostLimit
	background bool
}

func newHostLimit(gap time.Duration) *hostLimit {
	return &hostLimit{gap: gap, changed: make(chan struct{})}
}

// enqueue queues a request; it waits for its turn in ticket.wait.
func (l *hostLimit) enqueue(background bool) *ticket {
	t := &ticket{l: l, background: background}
	l.mu.Lock()
	if background {
		l.bg = append(l.bg, t)
	} else {
		l.fg = append(l.fg, t)
	}
	l.signal()
	l.mu.Unlock()
	return t
}

// signal wakes every waiter to look at the queue again. Needs l.mu.
func (l *hostLimit) signal() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// head is the request that goes next. Needs l.mu.
func (l *hostLimit) head() *ticket {
	if len(l.fg) > 0 {
		return l.fg[0]
	}
	if len(l.bg) > 0 {
		return l.bg[0]
	}
	return nil
}

// promote moves a queued background request to the end of the foreground
// queue: a foreground caller joined its download. A nil ticket (a host
// without a limit) or one already let through is left alone.
func (t *ticket) promote() {
	if t == nil {
		return
	}
	l := t.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if !t.background {
		return
	}
	if i := slices.Index(l.bg, t); i >= 0 {
		l.bg = slices.Delete(l.bg, i, i+1)
		l.fg = append(l.fg, t)
		t.background = false
		l.signal()
	}
}

// wait blocks until it is t's turn and at least gap has passed since the
// last request was let through, and returns how long it waited. A nil
// ticket doesn't wait.
func (t *ticket) wait() time.Duration {
	if t == nil {
		return 0
	}
	l := t.l
	start := time.Now()
	l.mu.Lock()
	for {
		now := time.Now()
		at := l.last.Add(l.gap)
		first := l.head() == t
		if first && !now.Before(at) {
			if t.background {
				l.bg = l.bg[1:]
			} else {
				l.fg = l.fg[1:]
			}
			l.last = now
			if l.released != nil {
				l.released(t, now)
			}
			l.signal()
			l.mu.Unlock()
			return time.Since(start)
		}
		changed := l.changed
		l.mu.Unlock()
		if first {
			timer := time.NewTimer(at.Sub(now))
			select {
			case <-timer.C:
			case <-changed: // a foreground request may have gone ahead
			}
			timer.Stop()
		} else {
			<-changed
		}
		l.mu.Lock()
	}
}
