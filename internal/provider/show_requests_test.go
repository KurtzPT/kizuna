package provider

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"kizuna/internal/anidb"
	"kizuna/internal/config"
	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata, without comparing: "+
	"review the update as a diff, a changed ask, early, ahead or later line is a behaviour change")

// fieldWorld: every source has some show fields and lacks others, so each
// order reaches a fallback somewhere. AniDB 100 is TheTVDB 500's and
// TheMovieDb 77's season 1, 200 their season 2, 300 TheMovieDb movie 900
// (Plex's p900).
func fieldWorld(path string, q map[string][]string) (int, string) {
	switch path {
	case "api.anidb.net/httpapi":
		switch strings.Join(q["aid"], "") {
		case "200":
			return 200, `<anime id="200"><type>TV Series</type><titles><title type="main" xml:lang="x-jat">Kizuna Two</title></titles>
<episodes><episode><epno type="1">1</epno><length>24</length><title xml:lang="en">Second Start</title></episode></episodes></anime>`
		case "300":
			return 200, movieXML(300, "Kizuna Film", "", 100, "2022-08-06")
		}
		return 200, castXML
	case "api4.thetvdb.com/v4/series/500/extended":
		return 200, tvdbSeriesRecord(false)
	case "api4.thetvdb.com/v4/series/500/artworks": // every kind: the default art orders stop here, before Plex
		return 200, `{"data":{"artworks":[{"type":2,"image":"/poster.jpg"},{"type":3,"image":"/bg.jpg"},{"type":23,"image":"/logo.png"}]}}`
	case "api4.thetvdb.com/v4/series/500/translations/eng":
		return 200, `{"data":{"name":"TVDB Name"}}` // no overview
	case "api.themoviedb.org/3/tv/77":
		return 200, `{"name":"TMDB Name","overview":"From TheMovieDb.","genres":[{"name":"TMDB Genre"}],
			"seasons":[{"season_number":1},{"season_number":2}]}`
	case "api.myanimelist.net/v2/anime/5":
		return 200, `{"synopsis":"From MAL.","genres":[{"name":"MAL Genre"}],"studios":[{"name":"MAL Studio"}],
			"start_date":"2009-04","rating":"pg_13","main_picture":{"large":"https://mal/p.jpg"}}`
	case "cdn.animenewsnetwork.com/encyclopedia/api.xml":
		if strings.Join(q["anime"], "") == "7" {
			return 200, `<ann><anime id="7"><info type="Main title" lang="EN">ANN Title</info>
<info type="Plot Summary">From ANN.</info><info type="Genres">action</info><info type="Objectionable content">TA</info>
<info type="Vintage">2009-04-01</info><staff><task>Director</task><person>ANN Director</person></staff>
<cast lang="JA"><role>Spike</role><person>Kouichi Yamadera</person></cast>
<credit><task>Animation Production</task><company>ANN Studio</company></credit></anime></ann>`
		}
	case plexItemPath:
		return 200, plexList(`{"title":"Plex Name","summary":"From Plex.","studio":"Plex Studio","contentRating":"TV-14",
			"originallyAvailableAt":"2009-04-01","Genre":[{"tag":"Plex Genre"}],"Director":[{"tag":"Plex Show Director"}],
			"Role":[{"tag":"Koichi Yamadera","role":"Spike Spiegel","thumb":"https://plex/yamadera.jpg","order":1},
				{"tag":"Plex Actor","role":"Hero","order":2}]}`)
	}
	return plexService(testSeries)(path, q)
}

// fieldKeys: which of the keys that make a source usable a case has.
type fieldKeys struct{ mal, plex, tmdb bool }

// fieldCase is one configuration of the show fields' source lists.
type fieldCase struct {
	name string
	keys fieldKeys
	set  func(c *config.Config)
	fail []string // paths (host and path) that answer 500
}

// showFieldLists: the source lists of the show fields.
func showFieldLists(c *config.Config) []*[]string {
	return []*[]string{&c.TitleSources, &c.SummarySources, &c.GenreSources, &c.CastSources, &c.StudioSources,
		&c.CrewSources, &c.RatingSources, &c.DateSources}
}

func eachFieldList(f func(l []string) []string) func(c *config.Config) {
	return func(c *config.Config) {
		for _, l := range showFieldLists(c) {
			*l = f(slices.Clone(*l))
		}
	}
}

func withoutSrc(l []string, src string) []string {
	return slices.DeleteFunc(l, func(s string) bool { return s == src })
}

var allKeys = fieldKeys{true, true, true}

// tvdbFails: TheTVDB's show values (its episode list still answers);
// plexFails: Plex's show.
var (
	tvdbFails = []string{"api4.thetvdb.com/v4/series/500/extended", "api4.thetvdb.com/v4/series/500/translations/eng",
		"api4.thetvdb.com/v4/series/500/artworks"}
	plexFails = []string{plexItemPath}
)

// fieldCases: the grid. Each list's interesting shapes (the default,
// none, a single source, plex first and last, a source without its key,
// each field its own order), with CAST_PHOTOS_FROM_PLEX off and on.
func fieldCases() []fieldCase {
	single := func(src string) func(c *config.Config) {
		return eachFieldList(func([]string) []string { return []string{src} })
	}
	photos := func(set func(c *config.Config)) func(c *config.Config) {
		return func(c *config.Config) {
			if set != nil {
				set(c)
			}
			c.CastPhotosFromPlex = true
		}
	}
	none := eachFieldList(func([]string) []string { return nil })
	plexFirst := eachFieldList(func(l []string) []string { return append([]string{"plex"}, withoutSrc(l, "plex")...) })
	plexLast := eachFieldList(func(l []string) []string { return append(withoutSrc(l, "plex"), "plex") })
	reversed := eachFieldList(func(l []string) []string { slices.Reverse(l); return l })
	keyless := eachFieldList(func(l []string) []string {
		return append([]string{"mal", "tmdb", "plex"}, withoutSrc(withoutSrc(withoutSrc(l, "mal"), "tmdb"), "plex")...)
	})
	mixed := func(c *config.Config) {
		c.TitleSources = []string{"tmdb", "plex", "anidb"}
		c.SummarySources = []string{"plex", "mal", "anidb"}
		c.GenreSources = []string{"mal", "ann"}
		c.CastSources = []string{"ann", "plex"}
		c.StudioSources = []string{"plex"}
		c.CrewSources = []string{"tvdb", "plex"}
		c.RatingSources = []string{"mal", "plex", "tmdb"}
		c.DateSources = []string{"ann", "tvdb"}
	}
	return []fieldCase{
		{"default", allKeys, nil, nil},
		{"default, cast photos", allKeys, photos(nil), nil},
		{"default, no mal or plex key", fieldKeys{tmdb: true}, nil, nil},
		{"none", allKeys, none, nil},
		{"none, cast photos", allKeys, photos(none), nil},
		{"cast none, cast photos", allKeys, photos(func(c *config.Config) { c.CastSources = nil }), nil},
		{"anidb only", allKeys, single("anidb"), nil},
		{"anidb only, cast photos", allKeys, photos(single("anidb")), nil},
		{"tvdb only", allKeys, single("tvdb"), nil},
		{"tmdb only", allKeys, single("tmdb"), nil},
		{"mal only", allKeys, single("mal"), nil},
		{"plex only", allKeys, single("plex"), nil},
		{"ann only", allKeys, single("ann"), nil},
		{"plex first", allKeys, plexFirst, nil},
		{"plex first, cast photos", allKeys, photos(plexFirst), nil},
		{"plex last", allKeys, plexLast, nil},
		{"reversed", allKeys, reversed, nil},
		{"keyless first", fieldKeys{}, keyless, nil},
		{"keyless first, cast photos", fieldKeys{}, photos(keyless), nil},
		{"mixed", allKeys, mixed, nil},
		{"mixed, no plex key", fieldKeys{mal: true, tmdb: true}, mixed, nil},
		// A site named in one text list only.
		{"title tvdb, rest none", allKeys, func(c *config.Config) { none(c); c.TitleSources = []string{"tvdb"} }, nil},
		{"summary tmdb, rest none", allKeys, func(c *config.Config) { none(c); c.SummarySources = []string{"tmdb"} }, nil},
		// A failing source makes the show incomplete only when a value read
		// needed its answer.
		{"tvdb fails", allKeys, nil, tvdbFails},
		{"plex fails", allKeys, nil, plexFails},
		{"plex fails, plex first", allKeys, plexFirst, plexFails},
		{"plex fails, cast photos", allKeys, photos(nil), plexFails},
		// No source has a rating for the listed country: the second pass.
		{"rating of another country", allKeys, func(c *config.Config) {
			c.RatingSources, c.RatingCountries = []string{"anidb", "tvdb", "plex"}, []string{"FR"}
		}, nil},
	}
}

// fieldRoots: two seasons and a movie in /anidb (neither season starts its
// series: StartsShow is false with SetForTest), and the series in /tvdb.
var fieldRoots = []string{"anidb-100", "anidb-200", "anidb-300", "tvdb-500"}

// fieldProvider is a provider for c over fieldWorld. taken gives the
// requests made since it was last called, sorted: each once, not how often
// (two fetches at once of one 404, which isn't cached, make one or two).
func fieldProvider(t *testing.T, c fieldCase) (p *Provider, taken func() []string) {
	var mu sync.Mutex
	asked := map[string]bool{}
	net := &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		mu.Lock()
		asked[path+"?"+url.Values(q).Encode()] = true
		mu.Unlock()
		if slices.Contains(c.fail, path) {
			return 500, `{}`
		}
		return fieldWorld(path, q)
	}}
	p = newTestProvider(t, net)
	p.titles.SetForTest(map[int][]anidb.Title{100: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test"}},
		200: {{Type: "main", Lang: "x-jat", Text: "Kizuna Two"}}, 300: {{Type: "main", Lang: "x-jat", Text: "Kizuna Film"}}})
	p.lists.SetForTest(map[int]mapping.Entry{
		100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}, TMDB: mapping.Placement{ID: 77, Season: 1, Mapped: true},
			MAL: 5, ANN: 7},
		200: {AID: 200, TVDB: mapping.Placement{ID: 500, Season: 2, Mapped: true}, TMDB: mapping.Placement{ID: 77, Season: 2, Mapped: true},
			MAL: 6, ANN: 8},
		300: {AID: 300, TMDBMovie: 900, IMDB: "tt900", MAL: 5, ANN: 7},
	})
	f := fetch.New(t.TempDir())
	f.SetTransport(net)
	if c.keys.mal {
		p.mal = sources.NewMAL(f, "client")
	}
	if !c.keys.tmdb {
		p.tmdb = sources.NewTMDB(f, "")
	}
	f.ThrottleAll(0)
	if c.keys.plex {
		withPlex(t, p, net)
	}
	if c.set != nil {
		c.set(&p.cfg)
	}
	return p, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Sorted(maps.Keys(asked))
		clear(asked)
		return out
	}
}

func writeAsked(b *strings.Builder, label string, asked []string) {
	for _, r := range asked {
		fmt.Fprintf(b, "%s %s\n", label, r)
	}
}

// buildFieldCase builds root under c, with a match report when reports,
// and describes what was asked and what the show got.
func buildFieldCase(t *testing.T, c fieldCase, root string, reports bool) string {
	p, taken := fieldProvider(t, c)
	ctx := context.Background()
	var rec *report.Recorder
	if reports {
		rec = report.NewRecorder(root, strings.SplitN(root, "-", 2)[0])
		ctx = report.WithRecorder(ctx, rec)
	}
	s, err := p.buildShow(ctx, root)
	p.anidb.WaitIdle()
	p.tvdb.WaitIdle()
	var b strings.Builder
	fmt.Fprintf(&b, "== %s / %s / reports %v\n", c.name, root, reports)
	if err != nil {
		fmt.Fprintf(&b, "error %v\n", err)
	}
	writeAsked(&b, "ask", taken())
	if s != nil {
		var cast []string
		for _, r := range s.Roles {
			cast = append(cast, r.Name+"|"+r.Role+"|"+r.Photo)
		}
		fmt.Fprintf(&b, "title %q\nsummary %q\ngenres %q\ncast %q\nstudio %q\ndirectors %q\nwriters %q\nrating %q\ndate %q\n",
			s.Title, s.Summary, s.Genres, cast, s.Studio, s.Directors, s.Writers, s.ContentRating, s.Date)
		fmt.Fprintf(&b, "posters %q\nbackgrounds %q\nlogos %q\n", s.Art.Posters, s.Art.Backgrounds, s.Art.Logos)
		if !strings.HasPrefix(root, "tvdb-") { // a /tvdb build may still wait for AniDB 200 in the background
			fmt.Fprintf(&b, "complete %v\n", s.Complete)
		}
	}
	for _, fd := range rec.Facts().Fields {
		var offered []string
		for _, src := range fd.Asked {
			offered = append(offered, src+"="+fd.Offered[src])
		}
		fmt.Fprintf(&b, "field %s order %q asked %q used %q offered %q\n", fd.Name, fd.Order, fd.Asked, fd.Used, offered)
	}
	return b.String()
}

// showPrefetch is one show of the prefetch grid: a build's steps for its
// show fields (model.go), with first set as StartsShow would.
type showPrefetch struct {
	name  string
	aid   int
	first firstEntry
	tvdb  bool // a /tvdb build: warmEarly before AniDB's details
}

var prefetchShows = []showPrefetch{
	{"anidb-100, starts both series", 100, firstEntry{true, true}, false},
	{"anidb-100, starts neither", 100, firstEntry{}, false},
	{"anidb-200, starts TheMovieDb's only", 200, firstEntry{tmdb: true}, false},
	{"anidb-300, a movie", 300, firstEntry{tmdb: true}, false},
	{"tvdb-500", 100, firstEntry{true, true}, true},
}

// prefetchFieldCase runs sh's show fields under c and says what was fetched
// ahead (warmEarly, then warm) and what only fill and fillSeasons fetched.
func prefetchFieldCase(t *testing.T, c fieldCase, sh showPrefetch) string {
	p, taken := fieldProvider(t, c)
	ctx := context.Background()
	var b strings.Builder
	fmt.Fprintf(&b, "== %s / %s\n", c.name, sh.name)
	entry := p.lists.ByAID(sh.aid)
	s := &Show{Root: sh.name, Episodes: map[int][]Ep{1: nil, 2: nil}}
	anime := func() *sources.AniDBAnime {
		a, err := p.anidb.Anime(ctx, sh.aid)
		if err != nil {
			t.Error(err)
		}
		taken() // AniDB's own request: not a show field's
		return a
	}
	var ss *showSources
	if sh.tvdb {
		ss = p.newSources(ctx, sh.aid, entry, entry.TVDB.ID, entry.TMDB.ID, false, sh.first)
		ss.tvdbTargets(plexSeasons(s))
		ss.warmEarly()
		ss.earlyWG.Wait()
		writeAsked(&b, "early", taken())
		ss.setAniDB(ctx, anime(), entry)
	} else {
		a := anime()
		tmdb, movie := entry.TMDB.ID, entry.Movie()
		if movie {
			tmdb = entry.TMDBMovie
		}
		ss = p.newSources(ctx, sh.aid, entry, entry.TVDB.ID, tmdb, movie, sh.first)
		ss.setAniDB(ctx, a, entry)
		ss.anidbTargets(entry, plexSeasons(s))
	}
	ss.warm() // fill warms again: what it would fetch ahead is in already
	writeAsked(&b, "ahead", taken())
	ss.fill(s)
	ss.fillSeasons(s)
	writeAsked(&b, "later", taken())
	fmt.Fprintf(&b, "title %q summary %q studio %q rating %q date %q failed %q\n",
		s.Title, s.Summary, s.Studio, s.ContentRating, s.Date, ss.failed())
	return b.String()
}

// goldenEnv sets every setting a build reads, so the machine's environment
// doesn't change what it asks.
func goldenEnv(t *testing.T) {
	defaultLists(t)
	for _, name := range []string{"TITLE_LANGS", "TITLE_OFFICIAL_ONLY", "ORIGINAL_TITLE_LANGS", "EPISODE_LANGS", "MIN_SCORE",
		"AUTO_DEFAULT", "MATCH_REPORTS", "PLEX_URL", "PLEX_TLS_NAME", "LOG_LEVEL", "TVDB_PIN", "TVDB_LEGACY_API_KEY"} {
		t.Setenv(name, "")
	}
	t.Setenv("DATA_DIR", t.TempDir())
}

// runGolden runs n cases, a few at once (they mostly wait on the cache's
// files), and compares what each says with its block of testdata/name.
// -update rewrites the file without comparing: review the update as a
// diff, since a changed ask, early, ahead or later line is a behaviour
// change (a source asked, or no longer asked, for a value).
func runGolden(t *testing.T, name string, n int, run func(i int) string) {
	out := make([]string, n)
	next := make(chan int)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			for i := range next {
				out[i] = run(i)
			}
		})
	}
	for i := range n {
		next <- i
	}
	close(next)
	wg.Wait()
	golden := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(strings.Join(out, "")), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	data, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{} // each case's lines, by its first one
	head := ""
	for line := range strings.SplitAfterSeq(string(data), "\n") {
		if strings.HasPrefix(line, "== ") {
			head = line
		}
		want[head] += line
	}
	if len(want) != len(out) {
		t.Errorf("%d cases, %s has %d", len(out), golden, len(want))
	}
	bad := 0
	for _, block := range out {
		head, _, _ := strings.Cut(block, "\n")
		if w := want[head+"\n"]; w != block {
			if bad++; bad <= 5 {
				t.Errorf("differs from %s:\ngot:\n%swant:\n%s", golden, block, w)
			}
		}
	}
}

// What a show build asks of every source, and what its fields get, over a
// grid of source lists, keys and modes, with the match report off and on.
// A difference is a request to a source no list names, a request lost, or
// a field from another source.
func TestShowRequestsGolden(t *testing.T) {
	goldenEnv(t)
	type job struct {
		c       fieldCase
		root    string
		reports bool
	}
	var jobs []job
	for _, c := range fieldCases() {
		for _, root := range fieldRoots {
			for _, reports := range []bool{false, true} {
				jobs = append(jobs, job{c, root, reports})
			}
		}
	}
	runGolden(t, "show_requests.golden", len(jobs), func(i int) string {
		return buildFieldCase(t, jobs[i].c, jobs[i].root, jobs[i].reports)
	})
}

// What the show fields fetch ahead (warmEarly, warm) and what only fill
// reaches, over the same grid, with the first entries StartsShow can't
// give the builds above. A difference is a prefetch lost or added: one
// added is a request to a source no list names for the value, or to Plex,
// MAL or ANN before the sources ahead of them left a gap.
func TestShowPrefetchGolden(t *testing.T) {
	goldenEnv(t)
	cases := fieldCases()
	runGolden(t, "show_prefetch.golden", len(cases)*len(prefetchShows), func(i int) string {
		return prefetchFieldCase(t, cases[i/len(prefetchShows)], prefetchShows[i%len(prefetchShows)])
	})
}
