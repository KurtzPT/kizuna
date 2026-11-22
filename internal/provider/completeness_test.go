package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"kizuna/internal/anidb"
	"kizuna/internal/config"
	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/sources"
)

// sourceRequest is one kind of request a show build makes of a source in
// fieldWorld.
type sourceRequest struct {
	name string
	src  string // the source's name in the lists
	is   func(path string, q map[string][]string) bool
	// nothingThere: the answers that are the source saying it has nothing
	// there, a rule of their own (tried where the value is needed: the
	// source first).
	nothingThere []sourceFailure
	more         []sourceFailure // failures with a rule of their own
	// own: made only by the setups with the same own, and there the only
	// kind that fails. "": every other setup's.
	own string
	// failsOnly: its one way to fail, instead of the usual ones.
	failsOnly *sourceFailure
}

// Setups whose build makes a request no other build makes
// (completeSetup.own).
const (
	ownLookup     = "lookup"      // the mapping has no TheMovieDb id for TheTVDB 500
	ownLater      = "later entry" // AniDB 200, the series' second entry, isn't cached yet
	ownSecondPage = "second page" // TheTVDB's episode list has two pages
	ownAgain      = "asked again" // a build after one whose TheTVDB text failed
)

// sourceFailure is how a request fails: code 0 is no answer at all (a
// network error), else the answer's status and body. cancel: the build's
// time runs out inside the answer (its context is cancelled).
type sourceFailure struct {
	name   string
	code   int
	body   string
	cancel bool
}

var (
	noAnswer  = sourceFailure{name: "no answer"}
	failed    = sourceFailure{name: "500", code: 500, body: `{}`}
	notThere  = sourceFailure{name: "404", code: 404, body: `{}`}
	outOfTime = sourceFailure{name: "out of time", code: 500, body: `{}`, cancel: true}
	by404     = []sourceFailure{notThere}
)

func pathIn(paths ...string) func(string, map[string][]string) bool {
	return func(path string, _ map[string][]string) bool { return slices.Contains(paths, path) }
}

func pathFrom(prefix string) func(string, map[string][]string) bool {
	return func(path string, _ map[string][]string) bool { return strings.HasPrefix(path, prefix) }
}

// tmdbShow: TheMovieDb's show record, with its details appended (cast,
// ratings) or without (its text and season list).
func tmdbShow(details bool) func(string, map[string][]string) bool {
	return func(path string, q map[string][]string) bool {
		return path == "api.themoviedb.org/3/tv/77" &&
			strings.Contains(strings.Join(q["append_to_response"], ","), "aggregate_credits") == details
	}
}

// tmdbFind: TheMovieDb's lookup of TheTVDB 500, asked when the mapping
// has no TheMovieDb id for it.
const tmdbFind = "api.themoviedb.org/3/find/500"

var (
	tvdbText    = pathFrom("api4.thetvdb.com/v4/series/500/translations/")
	anidbSecond = func(path string, q map[string][]string) bool {
		return path == "api.anidb.net/httpapi" && strings.Join(q["aid"], "") == "200"
	}
)

var sourceRequests = []sourceRequest{
	// AniDB's 404 is no answer about the anime, unlike its error about it
	// (blocked 24 h) and a ban.
	{name: "anidb", src: "anidb", is: pathIn("api.anidb.net/httpapi"), more: []sourceFailure{notThere,
		{name: "error answer", code: 200, body: "<error>Anime not found</error>"}, {name: "503 ban", code: 503, body: "Banned"}}},
	// Fetched in the background for the next build: its answer is held
	// until the build is over, and fails only in its own cells.
	{name: "anidb later entry", src: "anidb", is: anidbSecond, own: ownLater},
	{name: "tvdb login", src: "tvdb", is: pathIn("api4.thetvdb.com/v4/login"),
		more: []sourceFailure{{name: "401 key rejected", code: 401, body: `{}`}}},
	{name: "tvdb series record", src: "tvdb", is: pathIn("api4.thetvdb.com/v4/series/500/extended"), nothingThere: by404},
	{name: "tvdb text", src: "tvdb", is: tvdbText, nothingThere: []sourceFailure{notThere,
		{name: "400 no English text", code: 400, body: `{"status":"failure","message":"Translation not found"}`}}},
	{name: "tvdb text again", src: "tvdb", is: tvdbText, own: ownAgain, failsOnly: &outOfTime},
	{name: "tvdb art", src: "tvdb", is: pathIn("api4.thetvdb.com/v4/series/500/artworks"), nothingThere: by404},
	{name: "tvdb episode list", src: "tvdb", is: pathFrom(tvdbEpisodes), nothingThere: by404},
	// A 404 there is past the last page.
	{name: "tvdb episode list's second page", src: "tvdb", is: func(path string, q map[string][]string) bool {
		return path == tvdbEpisodes && strings.Join(q["page"], "") == "1"
	}, nothingThere: by404, own: ownSecondPage},
	{name: "tvdb crew records", src: "tvdb", is: pathFrom("api4.thetvdb.com/v4/episodes/"), nothingThere: by404},
	{name: "tmdb details", src: "tmdb", is: func(path string, q map[string][]string) bool {
		return tmdbShow(true)(path, q) || path == "api.themoviedb.org/3/movie/900"
	}, nothingThere: by404},
	{name: "tmdb text", src: "tmdb", is: tmdbShow(false), nothingThere: by404},
	{name: "tmdb art", src: "tmdb", is: pathIn("api.themoviedb.org/3/tv/77/images", "api.themoviedb.org/3/movie/900/images"),
		nothingThere: by404},
	{name: "tmdb season", src: "tmdb", is: pathFrom("api.themoviedb.org/3/tv/77/season/"), nothingThere: by404},
	// Its real "not found" is an empty list.
	{name: "tmdb lookup", src: "tmdb", is: pathIn(tmdbFind), own: ownLookup,
		nothingThere: []sourceFailure{notThere, {name: "empty list", code: 200, body: `{"tv_results":[]}`}}},
	{name: "plex match", src: "plex", is: pathFrom(plexHost + "/library/metadata/matches/"), nothingThere: by404},
	{name: "plex show", src: "plex", is: pathIn(plexItemPath, plexHost+"/library/metadata/p900"), nothingThere: by404},
	{name: "plex seasons", src: "plex", is: pathIn(plexItemPath + "/children"), nothingThere: by404},
	{name: "plex episode lists", src: "plex", is: func(path string, _ map[string][]string) bool {
		return strings.HasPrefix(path, plexItemPath+"s") && strings.HasSuffix(path, "/children")
	}, nothingThere: by404},
	{name: "mal", src: "mal", is: pathFrom("api.myanimelist.net/"), nothingThere: by404},
	{name: "ann", src: "ann", is: pathFrom("cdn.animenewsnetwork.com/"), nothingThere: by404},
}

func srcFirst(src string, l []string) []string { return append([]string{src}, withoutSrc(l, src)...) }

// firstIn puts src first in every source list that names it, so each of
// its values is needed.
func firstIn(src string) func(c *config.Config) {
	return func(c *config.Config) {
		lists := append(showFieldLists(c), &c.PosterSources, &c.BackgroundSources, &c.LogoSources,
			&c.EpisodeFillSources, &c.EpisodeCrewSources, &c.EpisodeTitles)
		for _, l := range lists {
			if slices.Contains(*l, src) {
				*l = srcFirst(src, *l)
			}
		}
	}
}

// completeSetup is one setup of the matrix: its settings, and the requests
// that fail in it.
type completeSetup struct {
	name   string
	src    string // only this source's requests fail; "" every source's
	kind   string // only this kind of request fails (a sourceRequest's name); "" every kind
	set    func(c *config.Config)
	needed bool     // the values are needed: "nothing there" answers are tried
	roots  []string // nil: fieldRoots
	// noAniDB: the roots built without ANIDB_CLIENT too. The others only
	// with it: there a source first would lose only the episode lists an
	// /anidb show without AniDB's episodes doesn't read.
	noAniDB []string
	own     string // see sourceRequest.own
	// newEntry: AniDB's title list doesn't have the series' first entry
	// yet (a new one, which AniDB has before the list's next refresh).
	newEntry bool
}

var tvdbRoot = []string{"tvdb-500"}

var completeSetups = []completeSetup{
	// One /anidb root without ANIDB_CLIENT is enough: all of them follow
	// the one rule (complete without AniDB's details).
	{name: "default", noAniDB: []string{"anidb-300", "tvdb-500"}},
	{name: "tvdb first", src: "tvdb", set: firstIn("tvdb"), needed: true},
	{name: "tmdb first", src: "tmdb", set: firstIn("tmdb"), needed: true},
	{name: "plex first", src: "plex", set: firstIn("plex"), needed: true},
	{name: "mal first", src: "mal", set: firstIn("mal"), needed: true},
	{name: "ann first", src: "ann", set: firstIn("ann"), needed: true},
	// A source first in one list only of those that read one fetch: no
	// other list's read can count the fetch's failure for it.
	{name: "plex first in episode titles only", kind: "plex episode lists", needed: true,
		roots: []string{"anidb-100", "tvdb-500"}, set: func(c *config.Config) {
			c.EpisodeTitles = srcFirst("plex", c.EpisodeTitles)
			c.EpisodeCrewSources = withoutSrc(c.EpisodeCrewSources, "plex")
			c.EpisodeFillSources = withoutSrc(c.EpisodeFillSources, "plex")
		}},
	{name: "tmdb first in posters only", kind: "tmdb season", needed: true,
		roots: []string{"anidb-100", "tvdb-500"}, set: func(c *config.Config) {
			c.PosterSources = srcFirst("tmdb", c.PosterSources)
			c.EpisodeTitles = withoutSrc(c.EpisodeTitles, "tmdb")
			c.EpisodeCrewSources = withoutSrc(c.EpisodeCrewSources, "tmdb")
		}},
	// /tvdb only: an /anidb entry here doesn't start its series, which
	// keeps TheTVDB's genres out.
	{name: "tvdb first in genres only", kind: "tvdb series record", needed: true, roots: tvdbRoot,
		set: func(c *config.Config) {
			c.GenreSources = srcFirst("tvdb", c.GenreSources)
			c.PosterSources = withoutSrc(c.PosterSources, "tvdb") // its season posters read the record too
		}},
	// Plex's show is fetched at the start for its cast photos: its failure
	// counts only when the cast needed it.
	{name: "default, cast photos", src: "plex", set: func(c *config.Config) { c.CastPhotosFromPlex = true }},
	{name: "default, TheMovieDb id looked up", src: "tmdb", roots: tvdbRoot, noAniDB: tvdbRoot, own: ownLookup},
	{name: "tmdb first, TheMovieDb id looked up", src: "tmdb", set: firstIn("tmdb"), needed: true, roots: tvdbRoot,
		own: ownLookup},
	// buildTVDB asks AniDB for such an entry, unlike buildAniDB: on purpose
	// since 94d8a4f, so its cells say "asked true".
	{name: "default, first entry not in AniDB's title list", src: "anidb", roots: tvdbRoot, noAniDB: tvdbRoot, newEntry: true},
	{name: "default, later entry not cached", src: "anidb", roots: tvdbRoot, own: ownLater},
	{name: "default, episode list of two pages", src: "tvdb", needed: true, roots: tvdbRoot, own: ownSecondPage},
	// The earlier build is complete: that text is fetched ahead and no
	// field needs it. It is the one request asked again, so nothing else
	// meets the cancelled context.
	{name: "default, built before with TheTVDB's text failing", src: "tvdb", roots: []string{"anidb-100"}, own: ownAgain},
}

// completeWorld is a build of root under a setup, with ANIDB_CLIENT set or
// not.
type completeWorld struct {
	setup *completeSetup
	root  string
	anidb bool
}

func (w completeWorld) String() string {
	anidb := "ANIDB_CLIENT set"
	if !w.anidb {
		anidb = "no ANIDB_CLIENT"
	}
	return fmt.Sprintf("%s / %s / %s", w.setup.name, w.root, anidb)
}

func completeWorlds() []completeWorld {
	var out []completeWorld
	for i := range completeSetups {
		l := &completeSetups[i]
		roots := l.roots
		if roots == nil {
			roots = fieldRoots
		}
		for _, root := range roots {
			out = append(out, completeWorld{l, root, true})
			if slices.Contains(l.noAniDB, root) {
				out = append(out, completeWorld{l, root, false})
			}
		}
	}
	return out
}

func (w completeWorld) failures(req *sourceRequest) []sourceFailure {
	switch {
	case req.own != w.setup.own,
		w.setup.src != "" && req.src != w.setup.src,
		w.setup.kind != "" && req.name != w.setup.kind:
		return nil
	case req.failsOnly != nil:
		return []sourceFailure{*req.failsOnly}
	}
	out := []sourceFailure{noAnswer, failed}
	if w.setup.needed {
		out = append(out, req.nothingThere...)
	}
	return append(out, req.more...)
}

// completeBuild is what a build of a world gave: the kinds of requests it
// asked (sourceRequests' indexes) and its outcome lines.
type completeBuild struct {
	asked   []bool
	outcome string
}

const nothingFails = -1

// buildComplete builds w's show with sourceRequests[req] failing as fail.
func buildComplete(t *testing.T, w completeWorld, req int, fail sourceFailure) completeBuild {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var armed atomic.Bool
	asked := make([]atomic.Bool, len(sourceRequests))
	var mu sync.Mutex
	var unknown []string
	// ask notes which kinds of request path is, and says whether it fails.
	ask := func(path string, q map[string][]string) bool {
		if !armed.Load() {
			return false
		}
		known := false
		for i := range sourceRequests {
			if sourceRequests[i].is(path, q) {
				asked[i].Store(true)
				known = true
			}
		}
		if !known && req == nothingFails {
			mu.Lock()
			unknown = append(unknown, path)
			mu.Unlock()
		}
		return req != nothingFails && sourceRequests[req].is(path, q)
	}
	buildOver := make(chan struct{})
	net := &fakeNet{
		answer: func(path string, q map[string][]string) (int, string) {
			switch fails := fail.code != 0 && ask(path, q); {
			case fails:
				if fail.cancel {
					cancel()
				}
				return fail.code, fail.body
			case w.setup.own == ownLater && anidbSecond(path, q):
				<-buildOver // the build sees it still loading
			case w.setup.own == ownAgain && !armed.Load() && tvdbText(path, q):
				return 500, `{}`
			case w.setup.own == ownSecondPage && path == tvdbEpisodes:
				if strings.Join(q["page"], "") == "1" {
					return 200, `{"data":{"episodes":[{"id":15,"seasonNumber":2,"number":2,"name":"TVDB Four"}]},"links":{"next":null}}`
				}
				code, body := fieldWorld(path, q)
				return code, strings.Replace(body, `"next":null`, `"next":"https://api4.thetvdb.com/v4/series/500/episodes/official/eng?page=1"`, 1)
			}
			return fieldWorld(path, q)
		},
		drop: func(path string, q map[string][]string) bool { return fail.code == 0 && ask(path, q) },
	}
	p := fieldProviderOn(t, allKeys, w.setup.set, net)
	if w.setup.own == ownLookup {
		entries := map[int]mapping.Entry{}
		for _, aid := range []int{100, 200, 300} {
			entries[aid] = p.lists.ByAID(aid)
		}
		e := entries[100] // the series' first entry: the one buildTVDB reads
		e.TMDB = mapping.Placement{}
		entries[100] = e
		p.lists.SetForTest(entries)
	}
	if w.setup.newEntry {
		p.titles.SetForTest(map[int][]anidb.Title{200: p.titles.Of(200), 300: p.titles.Of(300)})
	}
	if !w.anidb {
		dir := t.TempDir()
		f := fetch.New(dir)
		f.SetTransport(net)
		p.anidb = sources.NewAniDB(f, dir, "", "1")
	}
	if strings.HasPrefix(w.root, "tvdb-") && p.anidb.Enabled() && w.setup.own != ownLater {
		// The series' second entry cached, as after an earlier build: else
		// the show is incomplete while it loads in the background.
		if _, err := p.anidb.Anime(ctx, 200); err != nil {
			t.Error(err)
		}
	}
	if w.setup.own == ownAgain {
		if _, err := p.buildShow(ctx, w.root); err != nil {
			t.Error(err)
		}
		p.anidb.WaitIdle()
		p.tvdb.WaitIdle()
	}
	armed.Store(true)
	s, err := p.buildShow(ctx, w.root)
	close(buildOver)
	p.anidb.WaitIdle()
	p.tvdb.WaitIdle()
	if len(unknown) > 0 {
		t.Errorf("%s: requests no sourceRequests entry matches, so no cell fails them: %q", w, unknown)
	}
	var b strings.Builder
	switch {
	case noShow(err):
		fmt.Fprintf(&b, "error, no such show: %v\n", err)
	case err != nil:
		fmt.Fprintf(&b, "error, not available now: %v\n", err)
	}
	if s != nil {
		fmt.Fprintf(&b, "complete %v\n", s.Complete)
	}
	out := completeBuild{asked: make([]bool, len(asked)), outcome: b.String()}
	for i := range asked {
		out.asked[i] = asked[i].Load()
	}
	return out
}

// Whether a show build that met a failing source is complete (kept an
// hour) or incomplete (rebuilt in 2 minutes). Each kind of request of each
// source fails in turn, without an answer and with a 500 (and where it is
// the source saying it has nothing there a 404, TheTVDB's 400 for no
// English text and TheMovieDb's empty lookup, AniDB's error and ban
// answers, TheTVDB rejecting the key), under the default lists and with
// that source first (in every list, or in one of those reading a shared
// fetch), in /anidb and /tvdb, with and without ANIDB_CLIENT. Also: a
// later /tvdb entry still loading, a second page of TheTVDB's episode list
// failing, and the build running out of time.
//
// The rules are the owner's: a failure counts when a value read needed
// its answer; "nothing there" (a 404, AniDB's error about the anime,
// TheTVDB's error answer for a crew record) and a value fetched ahead that
// no field needed don't. A difference is a failure swallowed (complete:
// kept an hour without the data) or one counted that nothing needed
// (rebuilt and logged every 2 minutes). Some lines follow the code where
// no rule says: a 404 as "nothing there" for TheTVDB's series record, art
// and episode list, and for MAL and ANN; TheTVDB's crew records answering
// 500 (complete); TheMovieDb's lookup answering 404 (complete); /anidb
// without ANIDB_CLIENT.
//
// A request a world's build doesn't make can't fail it: such a cell says
// "asked false" with that build's outcome, without building again. Hence
// a build making a request no sourceRequests entry matches fails the test.
func TestShowCompleteGolden(t *testing.T) {
	goldenEnv(t)
	worlds := completeWorlds()
	healthy := make([]completeBuild, len(worlds))
	fewAtOnce(len(worlds), func(i int) { healthy[i] = buildComplete(t, worlds[i], nothingFails, sourceFailure{}) })
	type cell struct {
		w, req int
		fail   sourceFailure
	}
	var cells []cell
	for w, world := range worlds {
		cells = append(cells, cell{w: w, req: nothingFails})
		for i := range sourceRequests {
			for _, fail := range world.failures(&sourceRequests[i]) {
				cells = append(cells, cell{w, i, fail})
			}
		}
	}
	runGolden(t, "show_complete.golden", len(cells), func(i int) string {
		c := cells[i]
		if c.req == nothingFails {
			return fmt.Sprintf("== nothing fails / %s\n%s", worlds[c.w], healthy[c.w].outcome)
		}
		head := fmt.Sprintf("== %s %s / %s\n", sourceRequests[c.req].name, c.fail.name, worlds[c.w])
		if !healthy[c.w].asked[c.req] {
			return head + "asked false\n" + healthy[c.w].outcome
		}
		return head + "asked true\n" + buildComplete(t, worlds[c.w], c.req, c.fail).outcome
	})
}
