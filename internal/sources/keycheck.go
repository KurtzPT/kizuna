package sources

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"kizuna/internal/fetch"
)

// rejectedKeyPause: how long a source's rejected key (HTTP 401, 403 unless
// only401, or the answer a keyCheck's rejects names) is remembered. A wrong
// key won't fix itself.
const rejectedKeyPause = 10 * time.Minute

// keyCheck remembers that a source rejected its key, so a wrong key costs
// one request and one log line per pause, not one per call of a library
// scan. Meanwhile only the cache answers (fresh, or stale as for any failed
// download).
type keyCheck struct {
	source, setting string      // for the log: "TMDB", "TMDB_API_KEY"
	onDone          func(error) // the requests' Done (made once: no allocation per call)
	// rejects, if set, also counts this answer as a rejected key (one the
	// source gives with another code than 401/403).
	rejects func(*fetch.StatusError) bool
	// only401: a 403 is an ordinary failed request, not a rejected key
	// (Plex's service: Cloudflare in front of it answers some with a 403).
	only401 bool

	mu    sync.Mutex
	until time.Time
	err   error
}

// getJSON is f.GetJSON, unless the key was rejected lately.
func (k *keyCheck) getJSON(ctx context.Context, f *fetch.Client, r fetch.Request, out any) error {
	if err := k.blocked(); err != nil {
		start := time.Now()
		if data, ok := f.Fresh(ctx, r.Key, r.TTL); ok {
			fetch.Observe(ctx, r.Key, fetch.KindCache, start, nil)
			return json.Unmarshal(data, out)
		}
		if data, ok := f.Stale(ctx, r.Key, start, err); ok {
			return json.Unmarshal(data, out)
		}
		return err
	}
	r.Quiet, r.Done = true, k.onDone // logged in done: a rejection once per pause
	return f.GetJSON(ctx, r, out)
}

// blocked returns the rejection while its pause lasts.
func (k *keyCheck) blocked() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if time.Now().Before(k.until) {
		return k.err
	}
	return nil
}

func newKeyCheck(source, setting string) *keyCheck {
	k := &keyCheck{source: source, setting: setting}
	k.onDone = k.done
	return k
}

// done looks at a request's outcome (Done: also when a stale copy answered).
func (k *keyCheck) done(err error) {
	var se *fetch.StatusError
	switch {
	case err == nil || fetch.IsNotFound(err): // 404: the source just has no such entry
	case errors.As(err, &se) && (se.Code == http.StatusUnauthorized || (se.Code == http.StatusForbidden && !k.only401) ||
		(k.rejects != nil && k.rejects(se))):
		k.mu.Lock()
		first := !time.Now().Before(k.until)
		k.err, k.until = err, time.Now().Add(rejectedKeyPause)
		k.mu.Unlock()
		if first {
			slog.Error(k.source+" rejected the request: check "+k.setting+"; "+k.source+" is skipped until the next try",
				"retry_in", rejectedKeyPause, "err", err)
		}
	default:
		slog.Warn("source request failed", "source", k.source, "err", err) // its URL is in err, redacted
	}
}
