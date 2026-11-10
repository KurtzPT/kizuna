package plex

import (
	"context"
	"encoding/json"
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
var passes = []time.Duration{15 * time.Second, 2 * time.Minute}

const (
	callTimeout = 15 * time.Second // one request to Plex
	maxPending  = 10000            // shows waiting for their looks: more are dropped, so requests can't fill memory
	maxBody     = 8 << 20          // an answer from Plex (a show's extras are a few KB)
)

// A Renamer gives the opening and ending extras of shows their readable
// titles. Queue is called for a show when Plex refreshes it.
type Renamer struct {
	base, token string
	client      *http.Client
	after       func(time.Duration, func()) // time.AfterFunc; a test runs the function itself

	mu       sync.Mutex
	pending  map[string]bool // show GUIDs with looks still to come
	stopped  bool
	lastWarn time.Time
	one      sync.Mutex // one show at a time: a library refresh must not flood Plex
}

// New returns a Renamer for the Plex server at base ("http://plex:32400")
// with an account token. transport is for tests (nil = the default).
func New(base, token string, transport http.RoundTripper) *Renamer {
	return &Renamer{base: strings.TrimRight(base, "/"), token: token,
		client:  &http.Client{Timeout: callTimeout, Transport: transport},
		after:   func(d time.Duration, f func()) { time.AfterFunc(d, f) },
		pending: map[string]bool{}}
}

// CheckURL says what is wrong with a PLEX_URL ("" = nothing).
func CheckURL(base string) string {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "it must look like http://192.168.1.10:32400"
	}
	return ""
}

// Stop ends the looks still to come.
func (r *Renamer) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
}

// Queue plans the looks at the show with this GUID
// ("tv.plex.agents.custom.kizuna.anidb://show/anidb-23"). A show that
// already waits for its looks isn't planned twice.
func (r *Renamer) Queue(guid string) {
	r.mu.Lock()
	if r.stopped || r.pending[guid] || len(r.pending) >= maxPending {
		r.mu.Unlock()
		return
	}
	r.pending[guid] = true
	r.mu.Unlock()
	for i, d := range passes {
		last := i == len(passes)-1
		r.after(d, func() {
			r.look(guid)
			if last {
				r.mu.Lock()
				delete(r.pending, guid)
				r.mu.Unlock()
			}
		})
	}
}

func (r *Renamer) look(guid string) {
	r.one.Lock()
	defer r.one.Unlock()
	r.mu.Lock()
	stopped := r.stopped
	r.mu.Unlock()
	if stopped {
		return
	}
	if err := r.Rename(context.Background(), guid); err != nil {
		r.warn(guid, err)
	}
}

// warn logs a failure at most once a minute: with Plex down, a library
// refresh would log one per show.
func (r *Renamer) warn(guid string, err error) {
	r.mu.Lock()
	due := time.Since(r.lastWarn) >= time.Minute
	if due {
		r.lastWarn = time.Now()
	}
	r.mu.Unlock()
	if due {
		slog.Warn("can't set the opening and ending titles in Plex (check PLEX_URL and PLEX_TOKEN); more failures in the next minute aren't logged",
			"show", guid, "err", err)
	}
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

// Rename sets the title of every opening and ending extra of the shows with
// this GUID (one per library that holds the show) that doesn't have it yet.
func (r *Renamer) Rename(ctx context.Context, guid string) error {
	shows, err := r.get(ctx, "/library/all?type=2&guid="+url.QueryEscape(guid))
	if err != nil {
		return err
	}
	for _, show := range shows {
		extras, err := r.get(ctx, "/library/metadata/"+url.PathEscape(show.RatingKey)+"/extras")
		if err != nil {
			return err
		}
		for _, n := range titles(extras) {
			if err := r.put(ctx, "/library/metadata/"+url.PathEscape(n.key)+"?title.value="+url.QueryEscape(n.title)); err != nil {
				return err
			}
			slog.Info("plex: extra renamed", "show", show.Title, "from", n.now, "to", n.title)
		}
	}
	return nil
}

// renamed is an extra (by rating key) with the title it has and the one it gets.
type renamed struct {
	key, now, title string
	c               Credit
}

// titles lists the extras that need a new title, in Plex's order: the
// openings and endings whose file name says which one they are. When a
// creditless file and a regular one would get the same title, the creditless
// one says so.
func titles(extras []metadata) []renamed {
	var all []renamed
	count := map[string]int{}
	for _, e := range extras {
		if (e.Subtype != "opening" && e.Subtype != "ending") || len(e.Media) == 0 || len(e.Media[0].Part) == 0 {
			continue
		}
		if c, ok := CreditOf(stem(e.Media[0].Part[0].File)); ok {
			all = append(all, renamed{key: e.RatingKey, now: e.Title, c: c})
			count[c.Title]++
		}
	}
	regular := map[string]bool{} // titles a file without NC has
	for _, n := range all {
		if !n.c.Creditless {
			regular[n.c.Title] = true
		}
	}
	var out []renamed
	for _, n := range all {
		n.title = n.c.Title
		if n.c.Creditless && count[n.title] > 1 && regular[n.title] {
			n.title += " (creditless)"
		}
		if n.title != n.now {
			out = append(out, n)
		}
	}
	return out
}

func (r *Renamer) get(ctx context.Context, path string) ([]metadata, error) {
	body, err := r.do(ctx, http.MethodGet, path)
	if err != nil {
		return nil, err
	}
	var a answer
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("plex: %s: %w", pathOnly(path), err)
	}
	return a.MediaContainer.Metadata, nil
}

func (r *Renamer) put(ctx context.Context, path string) error {
	_, err := r.do(ctx, http.MethodPut, path)
	return err
}

// do sends one request. The token travels in a header, so no URL in an error
// or a log carries it.
func (r *Renamer) do(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, nil)
	if err != nil {
		return nil, fmt.Errorf("plex: %s: bad address", pathOnly(path))
	}
	req.Header.Set("X-Plex-Token", r.token)
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		if ue, ok := err.(*url.Error); ok { // without the URL: its query holds titles and GUIDs
			err = ue.Err
		}
		return nil, fmt.Errorf("plex: %s %s: %w", method, pathOnly(path), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("plex: %s %s: %w", method, pathOnly(path), err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("plex: %s %s: status %d", method, pathOnly(path), resp.StatusCode)
	}
	return body, nil
}

func pathOnly(path string) string {
	p, _, _ := strings.Cut(path, "?")
	return p
}
