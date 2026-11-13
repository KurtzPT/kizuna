// Package provider implements the Plex custom metadata provider: matching
// shows to AniDB/TVDB ids, building show models and answering Plex.
//
// Three provider URLs in Plex:
//
//	/anidb  one AniDB entry per show. Season 1 = all episodes, absolute
//	        numbers. Season 0 = specials/OP/ED/trailers.
//	/tvdb   one TVDB series per show. TVDB seasons, absolute episode numbers.
//	        An entry living only in the series' specials (a movie, an OVA) is
//	        its own show (mapping.Entry.SpecialOnly).
//	/auto   per show: an [anidb-N] tag or GUID gives the /anidb rules, a
//	        [tvdb-N] one the /tvdb rules, anything else AUTO_DEFAULT's.
//
// Only matching depends on the mode: a show key ("anidb-1", "tvdb-2") says
// how its show is built, whichever URL Plex asks.
package provider

import (
	"context"
	"errors"
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
	Auto  bool   // rules per show (see rules)
	id    string // Identifier, built once (it is in every item's GUIDs)
}

// Identifier is the Plex provider id and GUID scheme of the mode.
func (m Mode) Identifier() string { return m.id }

// GUID is the GUID of one of the mode's items: typ is "show", "season" or
// "episode", key the item's rating key. Plex stores these and guidRe reads
// them back, so every GUID is made here.
func (m Mode) GUID(typ, key string) string { return m.id + "://" + typ + "/" + key }

// withID sets a mode's Identifier. Every Mode is one of Modes, made by it.
func withID(m Mode) Mode {
	m.id = "tv.plex.agents.custom." + config.Name + "." + m.Path
	return m
}

var Modes = map[string]Mode{
	"anidb": withID(Mode{Path: "anidb", Title: "Kizuna (AniDB)"}),
	"tvdb":  withID(Mode{Path: "tvdb", Title: "Kizuna (TVDB)", TVDB: true}),
	"auto":  withID(Mode{Path: "auto", Title: "Kizuna", Auto: true}),
}

// Provider holds all sources plus two small caches.
type Provider struct {
	cfg          config.Config
	offLogged    everyTenMinutes // requests for a mode that is off (server.go)
	tagOffLogged everyTenMinutes // /auto id tags for a mode that is off (Provider.warn)
	tagsLogged   everyTenMinutes // titles with more than one id tag (Provider.warn)
	nameLogged   everyTenMinutes // titles matched by name inside a TheTVDB id (Provider.warn)
	titles       *anidb.Titles
	anidb        *sources.AniDB
	lists        *mapping.Lists
	tvdb         *sources.TVDB
	tmdb         *sources.TMDB
	mal          *sources.MAL
	ann          *sources.ANN
	plex         *sources.Plex // Plex's metadata service; nil = off

	access   Access            // ALLOW_FROM / ALLOWED_HOSTS
	reports  *report.Store     // match reports; nil = off
	onExtras func(guid string) // called when Plex asks for a show's extras; nil = nothing to do (OnExtrasRequest)

	loaded   chan struct{} // closed once the first list load is over (see Loading)
	stopping chan struct{} // closed by Stop
	stopOnce sync.Once

	mu       sync.Mutex
	shows    map[string]cachedShow  // root -> built model
	failed   map[string]failedBuild // root -> last build's failure, for failTTL
	builds   map[string]*build      // root -> build in progress (dedupes Plex's request bursts)
	prunedAt time.Time              // last sweep of expired shows
	running  int                    // builds not over yet, their report queued or not (see WaitBuilds)
	ended    chan struct{}          // a build ended (buffer of 1, for WaitBuilds)

	buildFn func(ctx context.Context, root string) (*Show, error) // nil = buildShow; tests swap it
}

type cachedShow struct {
	show *Show
	at   time.Time
}

// failedBuild is a build that failed for another reason than its time: one
// Plex refresh asks for a show several times, and each would download (and
// warn) again.
type failedBuild struct {
	err   error
	build string // its report's build id
	at    time.Time
}

// failTTL: how long a failed build answers for its show, like an incomplete
// one (showTTL).
const failTTL = 2 * time.Minute

type build struct {
	done chan struct{}
	show *Show
	err  error

	rec     *report.Recorder // its match report; nil = reports off
	matched bool             // rec has a match request; under p.mu
}

// Sources are the metadata sources a Provider asks besides AniDB. A struct
// and not one argument each: a new source changes no call site, and tests
// set only the ones they need. A nil source is off, except ANN, which is
// asked for any entry with an ANN id: a build of one needs it set.
type Sources struct {
	TVDB *sources.TVDB
	TMDB *sources.TMDB
	MAL  *sources.MAL
	ANN  *sources.ANN
	Plex *sources.Plex
}

func New(cfg config.Config, titles *anidb.Titles, ad *sources.AniDB, lists *mapping.Lists, src Sources) *Provider {
	access, bad := ParseAccess(cfg.AllowFrom, cfg.AllowedHosts)
	for _, b := range bad {
		slog.Warn("ignoring a bad access entry", "entry", b,
			"allowed", "ALLOW_FROM: private, any, 192.168.1.10, 192.168.1.0/24 · ALLOWED_HOSTS: kizuna.example.com, .example.com, any")
	}
	for _, e := range access.everyone() {
		slog.Warn("ALLOW_FROM=" + e + ": every address may use Kizuna; keep its port off the internet")
	}
	loaded := make(chan struct{})
	close(loaded)
	return &Provider{cfg: cfg, titles: titles, anidb: ad, lists: lists,
		tvdb: src.TVDB, tmdb: src.TMDB, mal: src.MAL, ann: src.ANN, plex: src.Plex,
		tagsLogged: everyTenMinutes{max: 1000}, nameLogged: everyTenMinutes{max: 1000}, access: access, loaded: loaded, stopping: make(chan struct{}), shows: map[string]cachedShow{}, failed: map[string]failedBuild{}, builds: map[string]*build{},
		ended: make(chan struct{}, 1)}
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

// debugOn: debug lines are logged. A hot path asks before slog.Debug, whose
// arguments allocate even when the line is dropped.
func debugOn(ctx context.Context) bool { return slog.Default().Enabled(ctx, slog.LevelDebug) }

// Stop marks the provider as shutting down: requests still waiting for the
// first list load answer 503 "stopping" at once instead of at the end of
// their time, so the server's drain doesn't wait for them, and AniDB's
// background fetches stop. Call it once the shutdown starts; later calls
// do nothing.
func (p *Provider) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopping)
		p.anidb.Stop()
	})
}

// WaitBuilds waits until no show build is running, or until ctx is done
// (then false). A build outlives requests that give up on it, and queues
// its match report when it ends: the shutdown waits for it, within its
// drain, before the reports' last flush. A build started meanwhile (by a
// request still being drained) is waited for too.
func (p *Provider) WaitBuilds(ctx context.Context) bool {
	for {
		p.mu.Lock()
		n := p.running
		p.mu.Unlock()
		if n == 0 {
			return true
		}
		select {
		case <-p.ended: // maybe an earlier build's: counted again above
		case <-ctx.Done():
			return false
		}
	}
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
// and cached in memory per showTTL. There is no size limit: only a build
// adds a model, and the end of a build sweeps out the expired ones
// (pruneShows, at most every 5 minutes), so memory holds what Plex asked
// for in about the hour before the latest build.
func (p *Provider) show(ctx context.Context, root string) (*Show, error) {
	p.mu.Lock()
	if c, ok := p.shows[root]; ok {
		if ttl := showTTL(c.show); time.Since(c.at) < ttl {
			p.mu.Unlock()
			age := time.Since(c.at).Round(time.Second)
			if debugOn(ctx) { // its args would be boxed for nothing
				slog.Debug("show from memory", "root", root, "age", age, "ttl", ttl)
			}
			if rq := report.RequestFrom(ctx); rq != nil {
				rq.Build, rq.Answer = c.show.build, report.Answer{Kind: report.AnswerMemory, Age: int64(age / time.Second)}
			}
			return c.show, nil
		}
	}
	if f, ok := p.failed[root]; ok && time.Since(f.at) < failTTL {
		p.mu.Unlock()
		age := time.Since(f.at).Round(time.Second)
		if debugOn(ctx) {
			slog.Debug("show build failed lately", "root", root, "age", age, "err", f.err)
		}
		if rq := report.RequestFrom(ctx); rq != nil {
			rq.Build, rq.Answer = f.build, report.Answer{Kind: report.AnswerFailed, Age: int64(age / time.Second), After: int64(failTTL / time.Second)}
		}
		return nil, f.err
	}
	b, running := p.builds[root]
	if !running {
		b = p.newBuild(root)
		if p.reports != nil {
			b.rec = report.NewRecorder(root, buildMode(ctx, root))
		}
	}
	if b.rec != nil && !b.matched {
		// The match request that started the build, or the first to join
		// one a metadata request started: its report shows how it matched.
		if m := report.MatchFrom(ctx); m != nil {
			b.rec.SetMatch(m)
			b.matched = true
		}
	}
	p.mu.Unlock()

	if !running {
		// The build belongs to every caller waiting on it, not to the first
		// one: Plex dropping that request must not fail the others.
		go p.runBuild(context.WithoutCancel(ctx), buildTime(ctx), root, b)
	}
	rq := report.RequestFrom(ctx) // nil with the reports off
	if rq != nil {
		rq.Answer = report.Answer{Kind: report.AnswerNewBuild}
		if running {
			rq.Answer.Kind = report.AnswerJoined
		}
	}
	select {
	case <-b.done:
	case <-ctx.Done():
		select {
		case <-b.done: // over as the request gave up: it has the answer
		default:
			if rq != nil {
				rq.Answer.Kind = report.AnswerTimedOut
			}
			return nil, ctx.Err() // the build goes on (see writeUnavailable)
		}
	}
	switch {
	case rq == nil:
	case b.show != nil:
		rq.Build = b.show.build
	default:
		rq.Build, rq.Answer = b.rec.ID(), report.Answer{Kind: report.AnswerUnavailable}
	}
	return b.show, b.err
}

// newBuild registers a build of root, counted until runBuild ends it;
// caller holds p.mu.
func (p *Provider) newBuild(root string) *build {
	b := &build{done: make(chan struct{})}
	p.builds[root] = b
	p.running++
	return b
}

// errBuildTimeout: the build ran out of its own time (buildTime) before its
// sources answered. Unlike a request that gives up while the build goes on,
// nothing is being built any more; it isn't kept as a failure, so the next
// request builds again.
var errBuildTimeout = errors.New("the show's sources didn't answer in time")

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
	rec := b.rec
	if rec != nil {
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
		delete(p.failed, root)
		switch {
		case b.err == nil:
			p.shows[root] = cachedShow{b.show, time.Now()}
		case !errors.Is(b.err, errBuildTimeout): // out of time: the next request tries again
			p.failed[root] = failedBuild{b.err, rec.ID(), time.Now()}
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
		// Counted down only now, its report queued: WaitBuilds returns
		// before the shutdown's last flush.
		p.mu.Lock()
		p.running--
		p.mu.Unlock()
		select {
		case p.ended <- struct{}{}:
		default: // a wake-up is already waiting
		}
	}()
	if p.buildFn != nil {
		b.show, b.err = p.buildFn(ctx, root)
	} else {
		b.show, b.err = p.buildShow(ctx, root)
	}
	if b.err != nil && ctx.Err() != nil {
		b.err = fmt.Errorf("%w: %v", errBuildTimeout, b.err) // the handlers answer 503
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

// pruneShows drops expired models and failures, at most every 5 minutes;
// caller holds p.mu.
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
	for root, f := range p.failed {
		if time.Since(f.at) >= failTTL {
			delete(p.failed, root)
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

// noShowError is a build error for a show that doesn't exist: the handlers
// answer 404 and "no match", which Plex keeps. Any other build error is a
// source that can't answer now (TheTVDB down, a key not set): 503 with the
// reason, so Plex keeps what it has and asks again.
type noShowError struct{ error }

func (e noShowError) Unwrap() error { return e.error }

// noShow: err says the show doesn't exist (see noShowError).
func noShow(err error) bool {
	var e noShowError
	return errors.As(err, &e)
}

// buildShow builds a show and sorts its failures: a show that doesn't exist
// is a noShowError, see there.
func (p *Provider) buildShow(ctx context.Context, root string) (*Show, error) {
	site, id, ok := parseRoot(root)
	if !ok {
		return nil, noShowError{fmt.Errorf("bad key %q", root)}
	}
	var s *Show
	var err error
	switch {
	case site == "anidb":
		// Its one failure: neither the title list nor AniDB knows the id.
		// Without the title list (it failed to load) that says nothing.
		if s, err = p.buildAniDB(ctx, id); errors.Is(err, errUnknownAID) && p.titles.Ready() {
			err = noShowError{err}
		}
	default:
		if off := p.modeOff(Modes["tvdb"]); off != "" {
			// A tvdb-N key asked through /anidb or /auto, with TheTVDB not set
			// up: like /tvdb itself (withMode), a 503 with the reason.
			return nil, fmt.Errorf("%s needs TheTVDB, which is off: %s", root, off)
		}
		// TheTVDB answered without episodes: the show doesn't exist. Any
		// other failure is TheTVDB not answering now.
		if s, err = p.buildTVDB(ctx, id); errors.Is(err, errNoEpisodes) {
			err = noShowError{err}
		}
	}
	if s != nil && ctx.Err() != nil {
		// Out of time: some source may have been skipped without saying so.
		s.Complete = false
		slog.Info("show build ran out of time; using what came back, rebuilt in 2 min", "root", root)
	}
	if s != nil {
		s.setImages()
	}
	return s, err
}
