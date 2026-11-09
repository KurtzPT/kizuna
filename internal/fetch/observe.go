package fetch

import (
	"context"
	"strings"
	"time"
)

// Kind is how a read was answered. The values are the match report's
// trace kinds (report.SpanKind), which it converts to directly.
type Kind string

const (
	KindNet    Kind = "net"    // downloaded
	KindCache  Kind = "cache"  // a cached copy
	KindStale  Kind = "stale"  // the download failed (or was not allowed) and an old cached copy answered
	KindFail   Kind = "fail"   // nothing: the read failed
	KindMemory Kind = "memory" // the source's own in-memory copy; no file was read
)

// Event is one read, for an observer (the per-show match report's trace).
type Event struct {
	Key   string // cache key, or the redacted URL when uncached
	Start time.Time
	Took  time.Duration
	Kind  Kind
	Err   error // why it failed, or why an old copy was used
}

type observerKey struct{}

// WithObserver returns a context whose reads are reported to f. f may be
// called from several goroutines at once.
func WithObserver(ctx context.Context, f func(Event)) context.Context {
	return context.WithValue(ctx, observerKey{}, f)
}

// Observing reports whether ctx has an observer, for callers that would
// build an event's key only for it.
func Observing(ctx context.Context) bool { return ctx.Value(observerKey{}) != nil }

// Observe reports a read that didn't go through Get (an answer from a
// source's own memory, or a cache file it read itself) to ctx's observer.
func Observe(ctx context.Context, key string, kind Kind, start time.Time, err error) {
	f, _ := ctx.Value(observerKey{}).(func(Event))
	if f == nil {
		return
	}
	f(Event{Key: key, Start: start, Took: time.Since(start), Kind: kind, Err: err})
}

// CachedCtx is Cached, reporting the read to ctx's observer when the file
// was there.
func (c *Client) CachedCtx(ctx context.Context, key string) ([]byte, time.Duration, bool) {
	start := time.Now()
	data, age, ok := c.Cached(key)
	if ok && Observing(ctx) {
		// A copy: handing key itself to the observer would put every
		// caller's key on the heap, observed or not.
		Observe(ctx, strings.Clone(key), KindCache, start, nil)
	}
	return data, age, ok
}

func observe(ctx context.Context, r Request, start time.Time, kind Kind, err error) {
	if ctx.Value(observerKey{}) == nil {
		return
	}
	key := r.Key
	if key == "" {
		key = Redact(r.URL)
	}
	Observe(ctx, key, kind, start, err)
}
