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
	"sync"
	"time"

	"kizuna/internal/anidb"
	"kizuna/internal/config"
	"kizuna/internal/mapping"
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
	"anidb": {Path: "anidb", Title: "Kizuna – AniDB (absolute)"},
	"tvdb":  {Path: "tvdb", Title: "Kizuna – TVDB seasons (absolute)", TVDB: true},
	"auto":  {Path: "auto", Title: "Kizuna – AniDB or TVDB per folder tag", Auto: true},
}

// Provider holds all sources plus two small caches.
type Provider struct {
	cfg    config.Config
	titles *anidb.Titles
	anidb  *sources.AniDB
	lists  *mapping.Lists
	tvdb   *sources.TVDB
	tmdb   *sources.TMDB
	mal    *sources.MAL
	ann    *sources.ANN

	access Access // ALLOW_FROM / ALLOWED_HOSTS

	mu       sync.Mutex
	shows    map[string]cachedShow // root -> built model
	builds   map[string]*build     // root -> build in progress (dedupes Plex's request bursts)
	prunedAt time.Time             // last sweep of expired shows
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
	return &Provider{cfg: cfg, titles: titles, anidb: ad, lists: lists, tvdb: tvdb, tmdb: tmdb, mal: mal, ann: ann,
		access: access, shows: map[string]cachedShow{}, builds: map[string]*build{}}
}

// showTTL: how long a built model is served from memory (shorter when AniDB
// details were missing, so they are retried soon).
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
			slog.Debug("show from memory", "root", root, "age", time.Since(c.at).Round(time.Second), "ttl", ttl)
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
		go p.runBuild(context.WithoutCancel(ctx), root, b)
	}
	select {
	case <-b.done:
		return b.show, b.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// runBuild builds one show for everyone waiting on b and caches it.
func (p *Provider) runBuild(ctx context.Context, root string, b *build) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	start := time.Now()
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
		close(b.done)
	}()
	b.show, b.err = p.buildShow(ctx, root)
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

var rootRe = regexp.MustCompile(`^(anidb|tvdb)-(\d+)$`)

func (p *Provider) buildShow(ctx context.Context, root string) (*Show, error) {
	m := rootRe.FindStringSubmatch(root)
	if m == nil {
		return nil, fmt.Errorf("bad key %q", root)
	}
	id, _ := strconv.Atoi(m[2])
	var s *Show
	var err error
	if m[1] == "anidb" {
		s, err = p.buildAniDB(ctx, id)
	} else {
		s, err = p.buildTVDB(ctx, id)
	}
	return s, err
}
