package sources

import (
	"context"
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
	// onReady: the requests' ready for GetJSON (made once too).
	onReady func(fetch.Request) (fetch.Request, error)
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

// getJSON is f.GetJSON, with no download while the key's rejection lasts.
// Its twin is sharedJSON: the Quiet, Done and ready they set must stay
// the same.
func (k *keyCheck) getJSON(ctx context.Context, f *fetch.Client, r fetch.Request, out any) error {
	r.Quiet, r.Done = true, k.onDone // logged in done: a rejection once per pause
	return f.GetJSON(ctx, r, out, k.onReady)
}

// sharedJSON is k.getJSON for a value its callers share and only read
// (fetch.GetShared), kept in m. Its twin is getJSON: see there.
func sharedJSON[T any](ctx context.Context, k *keyCheck, f *fetch.Client, m *fetch.Memo[string, *T], r fetch.Request) (*T, error) {
	r.Quiet, r.Done = true, k.onDone
	return fetch.GetShared(ctx, f, m, r, k.onReady)
}

// sharedFor: how long a decoded file shared by the reads of a build, and of
// the builds right after it, is kept (fetch.Memo's life), so a library scan
// doesn't keep them all. For TheMovieDb's season lists and season files
// and for TheTVDB's episode list pages.
const sharedFor = time.Minute

// ready is GetJSON's ready: the rejection, while its pause lasts, fails the
// download before it is sent.
func (k *keyCheck) ready(r fetch.Request) (fetch.Request, error) { return r, k.blocked() }

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
	k.onDone, k.onReady = k.done, k.ready
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
