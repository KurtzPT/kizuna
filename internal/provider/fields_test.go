package provider

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"kizuna/internal/config"
	"kizuna/internal/mapping"
	"kizuna/internal/sources"
)

func TestPickFallsBack(t *testing.T) {
	vals := map[string]string{"anidb": "", "tvdb": "from tvdb", "tmdb": "from tmdb"}
	var asked []string
	got, _ := pick([]string{"anidb", "tvdb", "tmdb"}, func(src string) string { asked = append(asked, src); return vals[src] })
	if got != "from tvdb" {
		t.Fatalf("got %q, want tvdb value", got)
	}
	if !slices.Equal(asked, []string{"anidb", "tvdb"}) { // tmdb never fetched
		t.Fatalf("asked %v", asked)
	}
}

func TestImagesAllAndFirst(t *testing.T) {
	art := map[string][]string{"tmdb": nil, "tvdb": {"a", "b"}, "anidb": {"c", "a"}}
	series := map[string][]string{"tvdb": {"d", "a"}}
	get := func(src string) ([]string, []string) { return art[src], series[src] }
	order := []string{"tmdb", "tvdb", "anidb"}
	if got, counts := images(order, true, true, get); !slices.Equal(got, []string{"a", "b", "d", "c"}) || counts != "tvdb:3,anidb:1" {
		t.Errorf("all: %v %q", got, counts)
	}
	if got, counts := images(order, false, false, get); !slices.Equal(got, []string{"a", "b", "d"}) || counts != "" {
		t.Errorf("first: %v %q", got, counts)
	}
}

func TestSeriesGuard(t *testing.T) {
	series := func() (sources.Details, error) {
		return sources.Details{Genres: []string{"series"}, Date: "2004-01-01"}, nil
	}
	ss := &showSources{tvdb: 1, tmdb: 2}
	ss.tvdbDetails.f, ss.tmdbDetails.f = series, series
	ss.mal.f = func() (sources.MALAnime, error) {
		return sources.MALAnime{Details: sources.Details{Genres: []string{"mal"}}}, nil
	}
	order := []string{"tvdb", "tmdb", "mal"}
	has := func(d sources.Details) bool { return len(d.Genres) > 0 }

	if got := pd(ss.pickDetails(order, has)).Genres[0]; got != "mal" { // sequel entry: skip series data
		t.Errorf("not first entry: got %q, want mal", got)
	}
	ss.movie = true // a TMDB movie is its own entry
	if got := pd(ss.pickDetails([]string{"tvdb", "tmdb"}, has)).Genres; len(got) != 1 {
		t.Errorf("movie: got %v, want tmdb genres", got)
	}
	ss.movie, ss.series = false, true
	if got := pd(ss.pickDetails(order, has)).Genres[0]; got != "series" {
		t.Errorf("first entry: got %q, want series", got)
	}
}

func pd(d sources.Details, _ string) sources.Details { return d }

// Without AniDB details, the fallbacks fill will reach are fetched at once,
// not one after another: each fetch here only returns once the other one has
// started. With details, AniDB has the fields: no fallback is fetched.
func TestWarmSkipsAniDBWithoutDetails(t *testing.T) {
	cfg := config.Config{SummarySources: []string{"anidb", "tvdb"}, GenreSources: []string{"anidb", "tmdb"},
		DateSources: []string{"anidb", "tvdb", "tmdb"}}
	newSS := func(a *sources.AniDBAnime) (*showSources, func() []string) {
		ss := &showSources{p: &Provider{cfg: cfg}, a: a, series: true, show: seriesTarget}
		var mu sync.Mutex
		var asked []string
		started := map[string]chan struct{}{"tvdb": make(chan struct{}), "tmdb": make(chan struct{})}
		both := func(src, other string) {
			mu.Lock()
			asked = append(asked, src)
			mu.Unlock()
			close(started[src])
			select {
			case <-started[other]:
			case <-time.After(20 * time.Second): // only when the fetches run one after another
				t.Errorf("%s: fetched alone, not alongside %s", src, other)
			}
		}
		ss.tvdbText.f = func() (text, error) { both("tvdb", "tmdb"); return text{summary: "From TheTVDB."}, nil }
		ss.tmdbDetails.f = func() (sources.Details, error) {
			both("tmdb", "tvdb")
			return sources.Details{Genres: []string{"TMDB"}, Date: "2004-01-01"}, nil
		}
		return ss, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(asked) }
	}

	ss, _ := newSS(nil)
	s := &Show{}
	ss.fill(s)
	if s.Summary != "From TheTVDB." || !slices.Equal(s.Genres, []string{"TMDB"}) || s.Date != "2004-01-01" {
		t.Errorf("summary %q genres %v date %q, want the fallbacks'", s.Summary, s.Genres, s.Date)
	}

	ss, asked := newSS(&sources.AniDBAnime{Description: "From AniDB.", StartDate: "2003-01-01",
		Genres: []string{"AniDB"}})
	s = &Show{}
	ss.fill(s)
	if got := asked(); len(got) != 0 || s.Summary != "From AniDB." {
		t.Errorf("with AniDB details: fetched %v (summary %q), want nothing", got, s.Summary)
	}
}

// AniDB's details (its cast built from the characters) are built once per
// show, not once per field that asks.
func TestAniDBDetailsBuiltOnce(t *testing.T) {
	a := &sources.AniDBAnime{Characters: []sources.AniDBCharacter{{Name: "Frieren", Seiyuu: "Atsumi Tanezaki"}}}
	ss := &showSources{a: a}
	if cast := ss.details("anidb").Cast; len(cast) != 1 || cast[0].Role != "Frieren" {
		t.Fatalf("cast %+v", cast)
	}
	if n := testing.AllocsPerRun(10, func() { ss.details("anidb") }); n != 0 {
		t.Errorf("%.0f allocations per later call, want 0", n)
	}
	if d := (&showSources{}).details("anidb"); len(d.Cast) != 0 { // no AniDB details
		t.Errorf("without AniDB: %+v", d)
	}
}

func TestANNFallback(t *testing.T) {
	cfg := config.Config{TitleSources: []string{"ann"}, SummarySources: []string{"anidb", "ann"},
		GenreSources: []string{"anidb", "ann"}, StudioSources: []string{"anidb"}}
	ann := sources.ANNAnime{ID: 13, Title: "Cowboy Bebop", Summary: "Bounty hunters.",
		Details: sources.Details{Genres: []string{"Drama"}}}
	ss := &showSources{p: &Provider{cfg: cfg}}
	ss.ann.f = func() (sources.ANNAnime, error) { return ann, nil }
	s := &Show{}
	ss.fill(s)
	// ANN's text is used unchanged (its credit is in the README and code only).
	if s.Summary != "Bounty hunters." || s.Title != "Cowboy Bebop" || len(s.Genres) != 1 {
		t.Fatalf("summary %q title %q genres %v", s.Summary, s.Title, s.Genres)
	}
}

// seasonTexts: TVDB 500 and TMDB 77 with a series overview and per-season
// ones. TVDB season 4 and TMDB season 1 have none.
func seasonTexts(path string, _ map[string][]string) (int, string) {
	switch path {
	case "api4.thetvdb.com/v4/login":
		return 200, `{"data":{"token":"t"}}`
	case "api4.thetvdb.com/v4/series/500/translations/eng":
		return 200, `{"data":{"name":"Series","overview":"TVDB series."}}`
	case "api4.thetvdb.com/v4/series/500/extended":
		return 200, `{"data":{"seasons":[{"id":503,"number":3,"type":{"type":"official"}},
			{"id":504,"number":4,"type":{"type":"official"}},{"id":603,"number":3,"type":{"type":"dvd"}}]}}`
	case "api4.thetvdb.com/v4/seasons/503/translations/eng":
		return 200, `{"data":{"name":"Season 3","overview":"TVDB season 3."}}`
	case "api4.thetvdb.com/v4/seasons/504/translations/eng":
		return 200, `{"data":{"name":"Season 4","overview":""}}`
	case "api.themoviedb.org/3/tv/77":
		return 200, `{"name":"Series","overview":"TMDB series.","seasons":[{"season_number":1},{"season_number":2}]}`
	case "api.themoviedb.org/3/tv/77/season/1":
		return 200, `{"overview":"","episodes":[]}`
	case "api.themoviedb.org/3/tv/77/season/2":
		return 200, `{"overview":"TMDB season 2.","episodes":[]}`
	}
	return 0, ""
}

// In /anidb, an entry after the series' first gets the summary of its own
// TheTVDB (else TheMovieDb) season, never the series' overview: without
// one the order goes on to MAL and ANN. The first entry keeps the series'.
func TestLaterEntrySummary(t *testing.T) {
	tvdb := func(season int) mapping.Placement { return mapping.Placement{ID: 500, Season: season, Mapped: true} }
	tmdb := func(season int, mapped bool) mapping.Placement {
		return mapping.Placement{ID: 77, Season: season, Mapped: mapped}
	}
	noText := &sources.AniDBAnime{}
	cases := []struct {
		name   string
		a      *sources.AniDBAnime
		entry  mapping.Entry
		series bool
		want   string
	}{
		{"no AniDB details", nil, mapping.Entry{TVDB: tvdb(3)}, false, "TVDB season 3."},
		{"empty AniDB description", noText, mapping.Entry{TVDB: tvdb(3)}, false, "TVDB season 3."},
		{"AniDB first", &sources.AniDBAnime{Description: "AniDB."}, mapping.Entry{TVDB: tvdb(3)}, false, "AniDB."},
		{"absolute", nil, mapping.Entry{TVDB: tvdb(-1)}, false, "MAL."},
		{"specials", nil, mapping.Entry{TVDB: tvdb(0)}, false, "MAL."},
		{"not mapped", nil, mapping.Entry{TVDB: mapping.Placement{ID: 500, Season: 3}}, false, "MAL."},
		{"empty TVDB season", nil, mapping.Entry{TVDB: tvdb(4), TMDB: tmdb(2, true)}, false, "TMDB season 2."},
		{"no TVDB id", nil, mapping.Entry{TMDB: tmdb(2, true)}, false, "TMDB season 2."},
		{"TMDB season not mapped", nil, mapping.Entry{TVDB: tvdb(4), TMDB: tmdb(2, false)}, false, "MAL."},
		{"empty TMDB season", nil, mapping.Entry{TVDB: tvdb(4), TMDB: tmdb(1, true)}, false, "MAL."},
		{"first entry", noText, mapping.Entry{TVDB: tvdb(1), TMDB: tmdb(1, true)}, true, "TVDB series."},
		{"first entry, TMDB", noText, mapping.Entry{TMDB: tmdb(1, true)}, true, "TMDB series."},
	}
	for _, c := range cases {
		net := &fakeNet{answer: seasonTexts}
		p := newTestProvider(t, net)
		p.cfg.TitleSources = []string{"anidb"} // only the summary may ask for the series' text
		ss := p.newSources(context.Background(), 100, c.a, c.entry, c.entry.TVDB.ID, c.entry.TMDB.ID, false, c.series)
		ss.mal.f = func() (sources.MALAnime, error) { return sources.MALAnime{Synopsis: "MAL."}, nil }
		s := &Show{}
		ss.fill(s)
		if s.Summary != c.want {
			t.Errorf("%s: summary %q, want %q", c.name, s.Summary, c.want)
		}
		// TMDB's series text is in the show file, which a later entry reads
		// for its season list: the summary above shows it isn't used.
		if !c.series && net.count("api4.thetvdb.com/v4/series/500/translations/eng") > 0 {
			t.Errorf("%s: a later entry fetched the series overview", c.name)
		}
	}
}

func TestUniqueCast(t *testing.T) {
	got := uniqueCast([]sources.Person{
		{Name: "Atsumi Tanezaki", Role: "Frieren", Photo: "a.jpg"},
		{Name: "Yoji Ueda", Role: "Eisen"},
		{Name: "Yoji Ueda ", Role: "eisen", Photo: "u.jpg"}, // same person and character
		{Name: "Yoji Ueda", Role: "Narrator"},               // another character: kept
	})
	want := []sources.Person{
		{Name: "Atsumi Tanezaki", Role: "Frieren", Photo: "a.jpg"},
		{Name: "Yoji Ueda", Role: "Eisen", Photo: "u.jpg"},
		{Name: "Yoji Ueda", Role: "Narrator"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
