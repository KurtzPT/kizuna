// Package provider implements the Plex custom metadata provider: matching
// shows to AniDB/TVDB ids, building show models and answering Plex.
//
// Three provider URLs in Plex:
//
//	/anidb  one AniDB entry per show. Season 1 = all episodes, absolute
//	        numbers. Season 0 = specials/OP/ED/trailers.
//	/tvdb   one TVDB series per show. TVDB seasons, absolute episode numbers.
//	/auto   per show: an [anidb-N] tag or GUID gives the /anidb rules, a
//	        [tvdb-N] one the /tvdb rules, anything else AUTO_DEFAULT's.
//
// Only matching depends on the mode: a show key ("anidb-1", "tvdb-2") says
// how its show is built, whichever URL Plex asks.
package provider

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"kizuna/internal/anidb"
	"kizuna/internal/config"
	"kizuna/internal/mapping"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// Mode is one provider URL.
type Mode struct {
	Path  string // URL path segment
	Title string // name shown in Plex
	TVDB  bool
	Auto  bool // rules per show (see rules)
}

// Identifier is the Plex provider id and GUID scheme of the mode.
func (m Mode) Identifier() string {
	return "tv.plex.agents.custom." + config.Name + "." + m.Path
}

var Modes = map[string]Mode{
	"anidb": {Path: "anidb", Title: "Kizuna (AniDB)"},
	"tvdb":  {Path: "tvdb", Title: "Kizuna (TVDB)", TVDB: true},
	"auto":  {Path: "auto", Title: "Kizuna", Auto: true},
}

// Provider holds all sources plus two small caches.
type Provider struct {
	cfg       config.Config
	offLogged everyTenMinutes // requests for a mode that is off (server.go)
	titles    *anidb.Titles
	anidb     *sources.AniDB
	lists     *mapping.Lists
	tvdb      *sources.TVDB
	tmdb      *sources.TMDB
	mal       *sources.MAL
	ann       *sources.ANN

	access  Access        // ALLOW_FROM / ALLOWED_HOSTS
	reports *report.Store // match reports; nil = off

	loaded chan struct{} // closed once the first list load is over (see Loading)

	mu       sync.Mutex
	shows    map[string]cachedShow // root -> built model
	builds   map[string]*build     // root -> build in progress (dedupes Plex's request bursts)
	prunedAt time.Time             // last sweep of expired shows

	buildFn func(ctx context.Context, root string) (*Show, error) // nil = buildShow; tests swap it
}

type cachedShow struct {
	show *Show
	at   time.Time
}

type build struct {
	done chan struct{}
	show *Show
	err  error
}

func New(cfg config.Config, titles *anidb.Titles, ad *sources.AniDB, lists *mapping.Lists,
	tvdb *sources.TVDB, tmdb *sources.TMDB, mal *sources.MAL, ann *sources.ANN) *Provider {
	access, bad := ParseAccess(cfg.AllowFrom, cfg.AllowedHosts)
	for _, b := range bad {
		slog.Warn("ignoring a bad access entry", "entry", b,
			"allowed", "ALLOW_FROM: private, any, 192.168.1.10, 192.168.1.0/24 · ALLOWED_HOSTS: kizuna.example.com, .example.com, any")
	}
	if access.AnyClient {
		slog.Warn("ALLOW_FROM=any: every address may use Kizuna; keep its port off the internet")
	}
	loaded := make(chan struct{})
	close(loaded)
	return &Provider{cfg: cfg, titles: titles, anidb: ad, lists: lists, tvdb: tvdb, tmdb: tmdb, mal: mal, ann: ann,
		access: access, loaded: loaded, shows: map[string]cachedShow{}, builds: map[string]*build{}}
}

// Loading marks the lists as still loading for the first time: /health
// answers "starting", and matches and metadata requests wait for the load
// (see waitLoaded). Call it before serving; call done when the first load
// is over, whether or not the lists loaded.
func (p *Provider) Loading() (done func()) {
	loaded := make(chan struct{})
	p.loaded = loaded
	return sync.OnceFunc(func() { close(loaded) })
}

// showTTL: how long a built model is served from memory (shorter when
// something was missing, see Show.Complete, so it is retried soon).
func showTTL(s *Show) time.Duration {
	if s.Complete {
		return time.Hour
	}
	return 2 * time.Minute
}

// show returns the model for a root rating key, built at most once at a time
// and cached in memory per showTTL. There is no size limit: expired models
// are swept out every few minutes, so memory follows what Plex asked for in
// the last hour.
func (p *Provider) show(ctx context.Context, root string) (*Show, error) {
	p.mu.Lock()
	if c, ok := p.shows[root]; ok {
		if ttl := showTTL(c.show); time.Since(c.at) < ttl {
			p.mu.Unlock()
			age := time.Since(c.at).Round(time.Second)
			slog.Debug("show from memory", "root", root, "age", age, "ttl", ttl)
			if rq := report.RequestFrom(ctx); rq != nil {
				rq.Build, rq.Answer = c.show.build, "memory, "+age.String()
			}
			return c.show, nil
		}
	}
	b, running := p.builds[root]
	if !running {
		b = &build{done: make(chan struct{})}
		p.builds[root] = b
	}
	p.mu.Unlock()

	if !running {
		// The build belongs to every caller waiting on it, not to the first
		// one: Plex dropping that request must not fail the others.
		go p.runBuild(context.WithoutCancel(ctx), buildTime(ctx), root, b)
	}
	rq := report.RequestFrom(ctx) // nil with the reports off
	if rq != nil {
		rq.Answer = "new build"
		if running {
			rq.Answer = "joined a build"
		}
	}
	select {
	case <-b.done:
		switch {
		case rq == nil:
		case b.show != nil:
			rq.Build = b.show.build
		default:
			rq.Answer = "not available"
		}
		return b.show, b.err
	case <-ctx.Done():
		if rq != nil {
			rq.Answer = "timed out"
		}
		return nil, ctx.Err()
	}
}

// buildMargin: a build stops this long before the request that started it
// gives up, so that request still answers with what came back.
const buildMargin = 2 * time.Second

// minBuildTime: a request with little time left (it waited for the first
// list load) still starts a build worth caching; it answers 503 itself and
// the next request gets the show.
const minBuildTime = requestTimeout / 2

// buildTime is how long a build started by a request with ctx may run.
func buildTime(ctx context.Context) time.Duration {
	left := requestTimeout
	if d, ok := ctx.Deadline(); ok {
		left = time.Until(d)
	}
	return max(left-buildMargin, minBuildTime)
}

// runBuild builds one show for everyone waiting on b and caches it.
func (p *Provider) runBuild(ctx context.Context, budget time.Duration, root string, b *build) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	start := time.Now()
	var rec *report.Recorder
	if p.reports != nil {
		rec = report.NewRecorder(root, buildMode(ctx, root))
		rec.SetMatch(report.MatchFrom(ctx)) // the match request that started the build, if any
		ctx = observeFetches(report.WithRecorder(ctx, rec), rec)
	}
	defer func() {
		cancel()
		if r := recover(); r != nil { // never leave waiters (and later requests) hanging on a dead build
			b.show, b.err = nil, fmt.Errorf("build panicked: %v", r)
			slog.Error("show build panicked", "root", root, "err", r, "stack", string(debug.Stack()))
		}
		p.mu.Lock()
		delete(p.builds, root)
		if b.err == nil {
			p.shows[root] = cachedShow{b.show, time.Now()}
		}
		p.pruneShows()
		p.mu.Unlock()
		// Queue the report before waking the waiters: a request answered by
		// this build is then never written (or flushed at shutdown) without it.
		if rec != nil {
			safeReport(root, func() {
				p.finishReport(rec, root, b.show, b.err, time.Since(start))
				p.reports.AddBuild(p.reportKey(root), rec)
			})
		}
		close(b.done)
	}()
	if p.buildFn != nil {
		b.show, b.err = p.buildFn(ctx, root)
	} else {
		b.show, b.err = p.buildShow(ctx, root)
	}
	if b.err != nil && ctx.Err() != nil {
		b.err = fmt.Errorf("%w: %v", ctx.Err(), b.err) // out of time: the handlers answer 503
	}
	if b.show != nil {
		b.show.build = rec.ID()
	}
	took := time.Since(start).Round(time.Millisecond)
	if b.err != nil {
		slog.Warn("show build failed", "root", root, "took", took, "err", b.err)
		return
	}
	s, eps := b.show, 0
	for _, n := range s.Seasons() {
		eps += len(s.Episodes[n])
	}
	slog.Info("show built", "root", root, "title", s.Title, "seasons", len(s.Episodes), "episodes", eps,
		"complete", s.Complete, "took", took)
}

// pruneShows drops expired models, at most every 5 minutes; caller holds p.mu.
func (p *Provider) pruneShows() {
	if time.Since(p.prunedAt) < 5*time.Minute {
		return
	}
	p.prunedAt = time.Now()
	for root, c := range p.shows {
		if time.Since(c.at) >= showTTL(c.show) {
			delete(p.shows, root)
		}
	}
}

// rootPattern is a show key ("anidb-1", "tvdb-2"). The id has no leading
// zero, so "anidb-01" isn't built and cached apart from "anidb-1", and at
// most 9 digits: it fits an int (AniDB and TheTVDB ids are far shorter).
const rootPattern = `(?:anidb|tvdb)-[1-9]\d{0,8}`

var rootRe = regexp.MustCompile(`^` + rootPattern + `$`)

// parseRoot splits a show key ("anidb-1", "tvdb-2") into its site and id.
func parseRoot(root string) (site string, id int, ok bool) {
	if !rootRe.MatchString(root) {
		return "", 0, false
	}
	site, n, _ := strings.Cut(root, "-")
	id, _ = strconv.Atoi(n)
	return site, id, true
}

func (p *Provider) buildShow(ctx context.Context, root string) (*Show, error) {
	site, id, ok := parseRoot(root)
	if !ok {
		return nil, fmt.Errorf("bad key %q", root)
	}
	var s *Show
	var err error
	if site == "anidb" {
		s, err = p.buildAniDB(ctx, id)
	} else {
		s, err = p.buildTVDB(ctx, id)
	}
	if s != nil && ctx.Err() != nil {
		// Out of time: some source may have been skipped without saying so.
		s.Complete = false
		slog.Info("show build ran out of time; using what came back, rebuilt in 2 min", "root", root)
	}
	return s, err
}
