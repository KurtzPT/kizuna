package plex

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Plex sets an extra's title back to the name it reads from the file ("OP1")
// on every metadata refresh of the show, a moment after it asks the provider
// for the show's extras. So each such request is followed by two looks at the
// show in Plex: one soon, for the usual refresh, and one later, for a refresh
// that was slow to save (artwork to download).
var passes = [...]time.Duration{15 * time.Second, 2 * time.Minute}

const (
	callTimeout = 15 * time.Second // one request to Plex
	maxPending  = 10000            // shows waiting for their looks: more are left out, so requests can't fill memory
	maxBody     = 8 << 20          // an answer from Plex (a show's extras are a few KB)
	maxLeftover = 64 << 10         // read from an answer nobody needs, to use the connection again
	maxFailures = 3                // calls in a row without an answer from Plex before the pause
	pause       = time.Minute      // how long nothing is asked of a Plex that doesn't answer
	warnEvery   = time.Minute      // one warning of a kind per this long
)

// A Renamer gives the opening and ending extras of shows their readable
// titles. Queue is called for a show when Plex refreshes it, and Run does
// the work.
type Renamer struct {
	base, token string
	client      *http.Client
	now         func() time.Time                     // time.Now; tests set the time by hand
	after       func(time.Duration) <-chan time.Time // time.After; tests end the wait themselves
	wake        chan struct{}                        // tells Run that Queue added a show

	mu          sync.Mutex
	waiting     map[string]*waiting    // by show GUID
	queues      [len(passes)]list.List // of *waiting, per pass: the delays are fixed, so each is in due order
	failures    int                    // calls in a row that Plex didn't answer
	pausedUntil time.Time              // Queue does nothing before this time
	lastWarn    time.Time              // the last failure warning
	lastFull    time.Time              // the last "too many shows" warning
}

// waiting is a show with looks still to come.
type waiting struct {
	guid  string
	looks [len(passes)]*list.Element // its place in each pass's queue; nil = that look is running or over
	due   [len(passes)]time.Time
	shows []show // what the first look found, so the next one needn't ask; only Run's goroutine uses it
}

// show is the show in one library.
type show struct{ key, title string }

// New returns a Renamer for the Plex server at base ("http://plex:32400")
// with an account token. Nothing happens until Run is called.
func New(base, token string) *Renamer {
	return &Renamer{base: strings.TrimRight(base, "/"), token: token,
		client: &http.Client{
			Timeout: callTimeout,
			// No proxy (HTTP_PROXY) and no redirects: the token must reach
			// the server at PLEX_URL and nothing else.
			Transport: &http.Transport{Proxy: nil, MaxIdleConns: 2, MaxIdleConnsPerHost: 2,
				IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now: time.Now, after: time.After, wake: make(chan struct{}, 1),
		waiting: map[string]*waiting{}}
}

// CheckURL says what is wrong with a PLEX_URL (nil = nothing). The messages
// never repeat the value: it may hold a password.
func CheckURL(base string) error {
	u, err := url.Parse(base)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "":
		return errors.New("it must look like http://192.168.1.10:32400")
	case u.User != nil:
		return errors.New("it must not hold a user name or password (the token goes in PLEX_TOKEN): use the form http://192.168.1.10:32400")
	case u.Path != "" && u.Path != "/":
		return errors.New("it must be the server's address alone, without a path after the port: use the form http://192.168.1.10:32400")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.HasSuffix(base, "#"):
		return errors.New("it must be the server's address alone, without a ? or # part: use the form http://192.168.1.10:32400")
	}
	return nil
}

// Queue plans the looks at the show with this GUID
// ("tv.plex.agents.custom.kizuna.anidb://show/anidb-23"), counted from now.
// Looks planned for the show before are replaced: Plex has just reset its
// titles again, so only looks after this moment are of use. Queue never
// waits for Plex: it is called while answering a request.
func (r *Renamer) Queue(guid string) {
	r.mu.Lock()
	now := r.now()
	if now.Before(r.pausedUntil) {
		r.mu.Unlock()
		return
	}
	w := r.waiting[guid]
	if w == nil {
		if len(r.waiting) >= maxPending {
			warn := due(&r.lastFull, now)
			r.mu.Unlock()
			if warn {
				slog.Warn("too many shows wait for their opening and ending titles: the others keep Plex's titles until their next metadata refresh; more of them in the next minute aren't logged",
					"waiting", maxPending)
			}
			return
		}
		w = &waiting{guid: guid}
		r.waiting[guid] = w
	}
	for i, d := range passes {
		w.due[i] = now.Add(d)
		if w.looks[i] != nil {
			r.queues[i].MoveToBack(w.looks[i])
		} else {
			w.looks[i] = r.queues[i].PushBack(w)
		}
	}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default: // Run already knows
	}
}

// Run does the looks Queue plans, one at a time (a library refresh must not
// flood Plex), until ctx ends. A call to Plex still running then is cut.
func (r *Renamer) Run(ctx context.Context) {
	for {
		wait, more := r.work(ctx)
		var timer <-chan time.Time
		if more {
			timer = r.after(wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake: // the new show's first look may come before the one waited for
		case <-timer:
		}
	}
}

// work does every look that is due. It returns how long until the next one;
// more is false when no show waits.
func (r *Renamer) work(ctx context.Context) (wait time.Duration, more bool) {
	for ctx.Err() == nil {
		w, wait := r.take()
		if w == nil {
			return wait, wait > 0
		}
		r.look(ctx, w)
	}
	return 0, false
}

// take removes the look that is due from its queue (the longest due first).
// Without one, w is nil and wait is the time until the next look (0 = no
// show waits).
func (r *Renamer) take() (w *waiting, wait time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pass := 0
	for i := range r.queues {
		if e := r.queues[i].Front(); e != nil {
			if first := e.Value.(*waiting); w == nil || first.due[i].Before(w.due[pass]) {
				w, pass = first, i
			}
		}
	}
	if w == nil {
		return nil, 0
	}
	if wait := w.due[pass].Sub(r.now()); wait > 0 {
		return nil, wait
	}
	r.queues[pass].Remove(w.looks[pass])
	w.looks[pass] = nil
	return w, 0
}

// look sets the titles of one show, and decides what a failure means.
func (r *Renamer) look(ctx context.Context, w *waiting) {
	shows, err := r.rename(ctx, w.guid, w.shows)
	if ctx.Err() != nil {
		return // stopping: the call was cut, nothing is wrong
	}
	r.mu.Lock()
	now := r.now()
	if r.waiting[w.guid] == w { // not dropped meanwhile
		w.shows = shows
		if w.looks == [len(passes)]*list.Element{} { // this was its last look
			delete(r.waiting, w.guid)
		}
	}
	// Plex doesn't answer: every look would wait for its timeout, one show
	// after the other, for nothing. Give Plex a moment.
	dropped, paused := 0, r.failures >= maxFailures
	if paused {
		dropped = len(r.waiting)
		clear(r.waiting)
		for i := range r.queues {
			r.queues[i].Init()
		}
		r.failures, r.pausedUntil = 0, now.Add(pause)
	}
	warn := err != nil && !paused && due(&r.lastWarn, now)
	r.mu.Unlock()
	switch {
	case paused:
		slog.Warn("Plex doesn't answer (check PLEX_URL): no opening and ending titles are set for the next minute, and the shows that waited get theirs on their next metadata refresh",
			"failures", maxFailures, "shows_dropped", dropped, "err", err)
	case warn:
		// At most once a minute: with a wrong token, a library refresh would
		// log one per show.
		slog.Warn("can't set the opening and ending titles in Plex (check PLEX_URL and PLEX_TOKEN); more failures in the next minute aren't logged",
			"show", w.guid, "err", err)
	}
}

// due says whether a warning last logged at *last may be logged again, and
// notes it when so.
func due(last *time.Time, now time.Time) bool {
	if !last.IsZero() && now.Sub(*last) < warnEvery {
		return false
	}
	*last = now
	return true
}

type metadata struct {
	RatingKey string `json:"ratingKey"`
	Title     string `json:"title"`
	Subtype   string `json:"subtype"`
	Media     []struct {
		Part []struct {
			File string `json:"file"`
		} `json:"Part"`
	} `json:"Media"`
}

type answer struct {
	MediaContainer struct {
		Metadata []metadata `json:"Metadata"`
	} `json:"MediaContainer"`
}

// rename sets the title of every opening and ending extra of the shows with
// this GUID (one per library that holds the show) that doesn't have it yet.
// known is what an earlier call returned: with it Plex isn't asked for the
// shows again. One failure doesn't end the work, unless Plex didn't answer
// at all: the errors come back joined.
func (r *Renamer) rename(ctx context.Context, guid string, known []show) ([]show, error) {
	shows := known
	if len(shows) == 0 {
		found, err := r.get(ctx, "/library/all?type=2&guid="+url.QueryEscape(guid))
		if err != nil {
			return nil, err
		}
		for _, m := range found {
			shows = append(shows, show{key: m.RatingKey, title: m.Title})
		}
		if len(shows) == 0 {
			// Not a fault: the show was removed, or its library is being built.
			slog.Debug("plex: no show with this GUID, no titles to set", "show", guid)
			return nil, nil
		}
	}
	var errs []error
	for _, s := range shows {
		extras, err := r.get(ctx, "/library/metadata/"+url.PathEscape(s.key)+"/extras")
		if len(known) > 0 && statusOf(err) == http.StatusNotFound {
			return r.rename(ctx, guid, nil) // the show was removed and added again: it has a new key
		}
		if err != nil {
			errs = append(errs, err)
			if noAnswer(err) {
				break
			}
			continue
		}
		for _, t := range titles(extras) {
			err := r.put(ctx, "/library/metadata/"+url.PathEscape(t.key)+"?title.value="+url.QueryEscape(t.to))
			if err != nil {
				errs = append(errs, err)
				if noAnswer(err) {
					return shows, errors.Join(errs...)
				}
				continue
			}
			slog.Info("plex: extra renamed", "show", s.title, "from", t.from, "to", t.to)
		}
	}
	return shows, errors.Join(errs...)
}

// retitle is an extra (by rating key) with the title it has and the one it gets.
type retitle struct {
	key, from, to string
	credit        Credit
}

// titles lists the extras that need a new title, in Plex's order: the
// openings and endings whose file name says which one they are. When a
// creditless file and a regular one would get the same title, the creditless
// one says so.
func titles(extras []metadata) []retitle {
	var all []retitle
	regular := map[string]bool{} // titles a file without NC has
	for _, e := range extras {
		if (e.Subtype != "opening" && e.Subtype != "ending") || len(e.Media) == 0 || len(e.Media[0].Part) == 0 {
			continue
		}
		if c, ok := CreditOf(stem(e.Media[0].Part[0].File)); ok {
			all = append(all, retitle{key: e.RatingKey, from: e.Title, credit: c})
			if !c.Creditless {
				regular[c.Title] = true
			}
		}
	}
	var out []retitle
	for _, t := range all {
		t.to = t.credit.Title
		if t.credit.Creditless && regular[t.to] {
			t.to += " (creditless)"
		}
		if t.to != t.from {
			out = append(out, t)
		}
	}
	return out
}

func (r *Renamer) get(ctx context.Context, path string) ([]metadata, error) {
	var a answer
	if err := r.do(ctx, http.MethodGet, path, &a); err != nil {
		return nil, err
	}
	return a.MediaContainer.Metadata, nil
}

func (r *Renamer) put(ctx context.Context, path string) error {
	return r.do(ctx, http.MethodPut, path, nil)
}

// do sends one request and reads the answer into a (nil = not needed). The
// token travels in a header, and errors name the path without its query, so
// no error or log line carries the token, a title or a GUID.
func (r *Renamer) do(ctx context.Context, method, path string, a *answer) error {
	call := method + " " + pathOnly(path)
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, nil)
	if err != nil {
		return fmt.Errorf("plex: %s: bad address", call)
	}
	req.Header.Set("X-Plex-Token", r.token)
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok { // without the URL
			err = ue.Err
		}
		if ctx.Err() == nil { // not when Kizuna is stopping
			r.count(false)
		}
		return &callError{call: call, err: err}
	}
	defer func() {
		// A connection is used again only when its answer was read to the end.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxLeftover))
		resp.Body.Close()
	}()
	r.count(true)
	if resp.StatusCode != http.StatusOK { // a redirect too: it isn't followed
		return &callError{call: call, status: resp.StatusCode}
	}
	if a == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(a); err != nil {
		return fmt.Errorf("plex: %s: not the answer Plex gives: %w", call, err)
	}
	return nil
}

// count notes whether Plex answered a call (with any status).
func (r *Renamer) count(answered bool) {
	r.mu.Lock()
	if answered {
		r.failures = 0
	} else {
		r.failures++
	}
	r.mu.Unlock()
}

// A callError is a call Plex didn't answer (err), or answered with a status
// other than 200.
type callError struct {
	call   string // "GET /library/all"
	status int
	err    error
}

func (e *callError) Error() string {
	switch {
	case e.status == http.StatusUnauthorized || e.status == http.StatusForbidden:
		return fmt.Sprintf("plex: %s: status %d: Plex refused the token (check PLEX_TOKEN)", e.call, e.status)
	case e.status != 0:
		return fmt.Sprintf("plex: %s: status %d", e.call, e.status)
	}
	return fmt.Sprintf("plex: %s: %v", e.call, e.err)
}

func (e *callError) Unwrap() error { return e.err }

// statusOf is the status Plex answered with when err is such a failure, else 0.
func statusOf(err error) int {
	if ce, ok := errors.AsType[*callError](err); ok {
		return ce.status
	}
	return 0
}

// noAnswer says whether err is a call Plex didn't answer.
func noAnswer(err error) bool {
	ce, ok := errors.AsType[*callError](err)
	return ok && ce.status == 0
}

func pathOnly(path string) string {
	p, _, _ := strings.Cut(path, "?")
	return p
}
