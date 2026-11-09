package provider

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"kizuna/internal/anidb"
	"kizuna/internal/config"
	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/sources"
)

// End-to-end show builds: every source answers from a fake at its real URL,
// so these run the whole path from the mapping to the finished model.

// fakeNet answers requests by host and path ("api4.thetvdb.com/v4/login").
// Unknown URLs get a 404, like a source without that entry.
type fakeNet struct {
	mu     sync.Mutex
	answer func(path string, q map[string][]string) (int, string)
	hits   map[string]int
}

func (f *fakeNet) RoundTrip(r *http.Request) (*http.Response, error) {
	path := r.URL.Hostname() + r.URL.Path
	f.mu.Lock()
	f.hits[path]++
	f.mu.Unlock()
	code, body := http.StatusNotFound, `{}`
	if f.answer != nil {
		if c, b := f.answer(path, r.URL.Query()); c != 0 {
			code, body = c, b
		}
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

func (f *fakeNet) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

// testSeries: AniDB 100 is TVDB 500 (TMDB 77) season 1 with two episodes;
// season 2 has one, the specials one.
const anidbXML = `<anime id="100"><type>TV Series</type><startdate>2009-04-01</startdate><enddate>2009-06-01</enddate>
<titles><title type="main" xml:lang="x-jat">Kizuna Test</title><title type="official" xml:lang="en">Bond Test</title></titles>
<description>From AniDB.</description>
<episodes>
<episode><epno type="1">1</epno><length>24</length><title xml:lang="en">The Start</title></episode>
<episode><epno type="1">2</epno><length>24</length><title xml:lang="en">The Middle</title></episode>
</episodes></anime>`

func testSeries(path string, q map[string][]string) (int, string) {
	switch path {
	case "api.anidb.net/httpapi":
		return 200, anidbXML
	case "api4.thetvdb.com/v4/login":
		return 200, `{"data":{"token":"t"}}`
	case "api4.thetvdb.com/v4/series/500/episodes/official/eng":
		return 200, `{"data":{"episodes":[
			{"id":11,"seasonNumber":1,"number":1,"name":"TVDB One","image":"/e1.jpg"},
			{"id":12,"seasonNumber":1,"number":2,"name":"TVDB Two","image":"/e2.jpg"},
			{"id":13,"seasonNumber":2,"number":1,"name":"TVDB Three"},
			{"id":14,"seasonNumber":0,"number":1,"name":"TVDB Special"}]},"links":{"next":null}}`
	case "api4.thetvdb.com/v4/episodes/11/extended":
		return 200, `{"data":{"characters":[{"personName":"TVDB Writer","peopleType":"Writer"},
			{"personName":"TVDB Director","peopleType":"Director"},{"personName":"A Voice","peopleType":"Actor"}]}}`
	case "api4.thetvdb.com/v4/episodes/12/extended": // no writer: TMDB's is used
		return 200, `{"data":{"characters":[{"personName":"TVDB Director 2","peopleType":"Director"}]}}`
	case "api.themoviedb.org/3/find/500":
		return 200, `{"tv_results":[{"id":77}]}`
	case "api.themoviedb.org/3/tv/77":
		return 200, `{"name":"TMDB Name","seasons":[{"season_number":1},{"season_number":2}]}`
	case "api.themoviedb.org/3/tv/77/season/1":
		return 200, `{"episodes":[{"episode_number":1,"name":"TMDB One","crew":[{"job":"Director","department":"Directing","name":"TMDB Director"}]},
			{"episode_number":2,"name":"TMDB Two","crew":[{"job":"Screenplay","department":"Writing","name":"TMDB Writer"},
				{"job":"Director","department":"Directing","name":"TMDB Director 2"}]}]}`
	case "api.themoviedb.org/3/tv/77/season/2":
		return 200, `{"episodes":[{"episode_number":1,"name":"TMDB Three","crew":[{"job":"Director","department":"Directing","name":"TMDB Director 3"}]}]}`
	case "api.themoviedb.org/3/tv/77/images":
		return 200, `{"posters":[{"file_path":"/p.jpg"}]}`
	}
	return 0, ""
}

// newTestProvider wires every source to net, with the default settings.
func newTestProvider(t *testing.T, net *fakeNet) *Provider {
	t.Helper()
	net.hits = map[string]int{}
	dir := t.TempDir()
	f := fetch.New(dir)
	f.SetTransport(net)
	titles := &anidb.Titles{}
	titles.SetForTest(map[int][]anidb.Title{100: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test"}}})
	lists := &mapping.Lists{}
	lists.SetForTest(map[int]mapping.Entry{100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true},
		TMDB: mapping.Placement{ID: 77}}})
	cfg := config.Load()
	p := New(cfg, titles, sources.NewAniDB(f, dir, "kizunatest", "1"), lists,
		sources.NewTVDB(f, "key", "", ""), sources.NewTMDB(f, "key"), sources.NewMAL(f, ""), sources.NewANN(f))
	// No background fetch outlives the test: it would land in a later
	// test's allocation count (TestReportsOffAllocs).
	t.Cleanup(func() { p.anidb.WaitIdle(); p.tvdb.WaitIdle() })
	return p
}

func TestBuildTVDBShow(t *testing.T) {
	net := &fakeNet{answer: testSeries}
	p := newTestProvider(t, net)
	s, err := p.buildShow(context.Background(), "tvdb-500")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Complete {
		t.Error("everything answered: want a complete show")
	}
	if s.Title != "Kizuna Test" || s.Summary != "From AniDB." {
		t.Errorf("title %q summary %q, want AniDB's", s.Title, s.Summary)
	}
	// TVDB seasons, absolute numbers: S02's first episode is episode 3.
	if ep := s.Episode(2, 3); ep == nil || ep.Title != "TVDB Three" {
		t.Errorf("S02E03: %+v", ep)
	}
	if ep := s.Episode(1, 1); ep == nil || ep.Title != "The Start" || ep.Thumb != "https://artworks.thetvdb.com/e1.jpg" {
		t.Errorf("S01E01: %+v, want AniDB's title and TVDB's thumbnail", ep)
	}
	if len(s.Art.Posters) == 0 || s.Art.Posters[0] != "https://image.tmdb.org/t/p/original/p.jpg" {
		t.Errorf("posters %v, want TMDB's first", s.Art.Posters)
	}
	if !strings.Contains(strings.Join(s.Guids, " "), "tmdb://77") {
		t.Errorf("guids %v", s.Guids)
	}
}

func TestBuildAniDBShow(t *testing.T) {
	net := &fakeNet{answer: testSeries}
	p := newTestProvider(t, net)
	s, err := p.buildShow(context.Background(), "anidb-100")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Complete || s.Placeholders {
		t.Errorf("complete %v placeholders %v, want a full show", s.Complete, s.Placeholders)
	}
	if ep := s.Episode(1, 2); ep == nil || ep.Title != "The Middle" || ep.Thumb != "https://artworks.thetvdb.com/e2.jpg" || ep.Minutes != 24 {
		t.Errorf("S01E02: %+v, want AniDB's title and length, TVDB's thumbnail", ep)
	}
}

// Episode directors and writers: TheTVDB's (v4: one record per episode),
// else TheMovieDb's, per field, in both modes.
func TestEpisodeCrew(t *testing.T) {
	net := &fakeNet{answer: testSeries}
	p := newTestProvider(t, net)
	type want struct {
		season, index int
		directors     string
		writers       string
	}
	cases := map[string][]want{
		"tvdb-500": {
			{1, 1, "TVDB Director", "TVDB Writer"},
			{1, 2, "TVDB Director 2", "TMDB Writer"}, // TVDB has no writer
			{2, 3, "TMDB Director 3", ""},            // no TVDB record (404)
			{0, 1, "", ""},
		},
		"anidb-100": {
			{1, 1, "TVDB Director", "TVDB Writer"},
			{1, 2, "TVDB Director 2", "TMDB Writer"},
		},
	}
	for root, wants := range cases {
		for build := range 2 {
			s, err := p.buildShow(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if !s.Complete {
				t.Errorf("%s: want complete (a missing record is nothing to wait for)", root)
			}
			for _, w := range wants {
				ep := s.Episode(w.season, w.index)
				if ep == nil {
					t.Fatalf("%s S%02dE%02d missing", root, w.season, w.index)
				}
				if d, wr := strings.Join(ep.Directors, ","), strings.Join(ep.Writers, ","); d != w.directors || wr != w.writers {
					t.Errorf("%s build %d S%02dE%02d: directors %q writers %q, want %q %q",
						root, build, w.season, w.index, d, wr, w.directors, w.writers)
				}
			}
		}
	}
	// Cached records are used; a missing one isn't asked again right away.
	for _, path := range []string{"api4.thetvdb.com/v4/episodes/11/extended", "api4.thetvdb.com/v4/episodes/13/extended"} {
		if n := net.count(path); n != 1 {
			t.Errorf("%s asked %d times, want 1", path, n)
		}
	}
	s, _ := p.buildShow(context.Background(), "tvdb-500")
	it := p.episodeItem(Modes["tvdb"], s, s.Episode(1, 2))
	if len(it.Director) != 1 || it.Director[0].Tag != "TVDB Director 2" || len(it.Writer) != 1 || it.Writer[0].Tag != "TMDB Writer" {
		t.Errorf("Plex episode: Director %v Writer %v", it.Director, it.Writer)
	}
}

// Anime-Lists numbers credits C1 = 101, C2 = 102… (endings included), while
// Kizuna shows endings from 151: the mapping must be read by AniDB's C number.
func TestCreditsUseAnimeListsNumbers(t *testing.T) {
	withCredits := strings.Replace(anidbXML, "</episodes>",
		`<episode><epno type="3">C1</epno><title xml:lang="en">Opening</title></episode>
<episode><epno type="3">C2</epno><title xml:lang="en">Ending</title></episode></episodes>`, 1)
	p := newTestProvider(t, &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		if path == "api.anidb.net/httpapi" {
			return 200, withCredits
		}
		return testSeries(path, q)
	}})
	p.lists.SetForTest(map[int]mapping.Entry{100: {AID: 100,
		TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true,
			Episodes: []mapping.EpisodeMap{{AniDBSeason: 0, AniDBEp: 102, Season: 0, Ep: 1}}},
		TMDB: mapping.Placement{ID: 77}}})
	s, err := p.buildShow(context.Background(), "anidb-100")
	if err != nil {
		t.Fatal(err)
	}
	if ep := s.Episode(0, 151); ep == nil || ep.titles["tvdb"] != "TVDB Special" {
		t.Errorf("ending C2 (S00E151): %+v, want paired with TVDB S00E01 through Anime-Lists' 102", ep)
	}
	if ep := s.Episode(0, 101); ep == nil || ep.titles["tvdb"] != "" {
		t.Errorf("opening C1 (S00E101): %+v, want no TVDB pairing", ep)
	}
}

// /anidb pairs TheMovieDb by the mapping's TMDB season, offset and
// exceptions, not at TheTVDB's spot (like Re:Zero's 2nd season: TVDB S2,
// TMDB S1 from episode 27). Titles and crew come from the right TMDB episode.
func TestAniDBPairsTMDBByItsOwnSeasons(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	p.lists.SetForTest(map[int]mapping.Entry{100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 2, Mapped: true},
		TMDB: mapping.Placement{ID: 77, Mapped: true, Season: 1, Offset: 1,
			Episodes: []mapping.EpisodeMap{{AniDBSeason: 1, AniDBEp: 2, Season: 2, Ep: 1}}}}})
	s, err := p.buildShow(context.Background(), "anidb-100")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		index                 int
		tvdb, tmdb, directors string
		writers               string
	}{
		// TVDB S2E1 (no crew record), TMDB S1E2 by the offset
		{1, "TVDB Three", "TMDB Two", "TMDB Director 2", "TMDB Writer"},
		// no TVDB S2E2; TMDB S2E1 by the exception
		{2, "", "TMDB Three", "TMDB Director 3", ""},
	}
	for _, c := range cases {
		ep := s.Episode(1, c.index)
		if ep == nil {
			t.Fatalf("S01E%02d missing", c.index)
		}
		if ep.titles["tvdb"] != c.tvdb || ep.titles["tmdb"] != c.tmdb {
			t.Errorf("S01E%02d titles %v, want tvdb %q tmdb %q", c.index, ep.titles, c.tvdb, c.tmdb)
		}
		if d, w := strings.Join(ep.Directors, ","), strings.Join(ep.Writers, ","); d != c.directors || w != c.writers {
			t.Errorf("S01E%02d directors %q writers %q, want %q %q", c.index, d, w, c.directors, c.writers)
		}
	}
}

// /tvdb pairs TheMovieDb through AniDB, not at TheTVDB's season and number
// (One Piece: TVDB S2E1 is AniDB episode 9, TMDB S2E1 is episode 62). An
// episode without an AniDB counterpart is paired by absolute number, and so
// is one whose AniDB entry isn't cached yet, until the rebuild.
func TestTVDBPairsTMDBThroughAniDB(t *testing.T) {
	second := `<anime id="200"><type>TV Series</type><enddate>2010-01-01</enddate><episodes>
<episode><epno type="1">1</epno><title xml:lang="en">Second Start</title></episode></episodes></anime>`
	net := &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		if path == "api.anidb.net/httpapi" && q["aid"][0] == "200" {
			return 200, second
		}
		return testSeries(path, q)
	}}
	p := newTestProvider(t, net)
	p.lists.SetForTest(map[int]mapping.Entry{
		// AniDB 1 = TVDB S1E1 = TMDB S1E2; AniDB 2 = TVDB S1E2 = TMDB S2E1
		100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true},
			TMDB: mapping.Placement{ID: 77, Mapped: true, Season: 1, Offset: 1,
				Episodes: []mapping.EpisodeMap{{AniDBSeason: 1, AniDBEp: 2, Season: 2, Ep: 1}}}},
		// AniDB 200 ep 1 = TVDB S2E1 = TMDB S1E1, not cached at first
		200: {AID: 200, TVDB: mapping.Placement{ID: 500, Season: 2, Mapped: true},
			TMDB: mapping.Placement{ID: 77, Mapped: true, Season: 1}},
	})
	type want struct {
		season, index        int
		tmdb, directors, wrt string
	}
	check := func(build string, s *Show, wants []want) {
		t.Helper()
		for _, w := range wants {
			ep := s.Episode(w.season, w.index)
			if ep == nil {
				t.Fatalf("%s: S%02dE%02d missing", build, w.season, w.index)
			}
			if ep.titles["tmdb"] != w.tmdb {
				t.Errorf("%s: S%02dE%02d TMDB title %q, want %q", build, w.season, w.index, ep.titles["tmdb"], w.tmdb)
			}
			if d, wr := strings.Join(ep.Directors, ","), strings.Join(ep.Writers, ","); d != w.directors || wr != w.wrt {
				t.Errorf("%s: S%02dE%02d directors %q writers %q, want %q %q", build, w.season, w.index, d, wr, w.directors, w.wrt)
			}
		}
	}
	s, err := p.buildShow(context.Background(), "tvdb-500")
	if err != nil {
		t.Fatal(err)
	}
	if s.Complete {
		t.Error("AniDB 200 not cached: want an incomplete show")
	}
	check("first build", s, []want{
		{1, 1, "TMDB Two", "TVDB Director", "TVDB Writer"},
		{1, 2, "TMDB Three", "TVDB Director 2", ""}, // TMDB S2E1 has no writer
		{2, 3, "TMDB Three", "TMDB Director 3", ""}, // AniDB 200 not cached: absolute 3
		{0, 1, "", "", ""}, // no AniDB counterpart, no TMDB S00E01
	})

	p.anidb.WaitIdle()
	if a, _ := p.anidb.Peek(200); a == nil {
		t.Fatal("AniDB 200 never fetched in the background")
	}
	if s, err = p.buildShow(context.Background(), "tvdb-500"); err != nil {
		t.Fatal(err)
	}
	if !s.Complete {
		t.Error("every AniDB entry cached: want a complete show")
	}
	check("rebuild", s, []want{{2, 3, "TMDB One", "TMDB Director", ""}})
}

// In /tvdb, an episode without an AniDB counterpart is paired with the other
// site by absolute number, not by the list's season and number, which the
// other site may split differently: TVDB S2E1 (absolute 3) is TMDB S1E3
// here, not TMDB S2E1. An absolute number the other site doesn't have pairs
// with nothing.
func TestTVDBUnpairedEpisodeGoesByAbsoluteNumber(t *testing.T) {
	wrong := `{"episodes":[{"episode_number":1,"name":"TMDB S2E1 (wrong)","crew":[{"job":"Director","name":"Wrong Director"}]}]}`
	cases := map[string]struct {
		season1   string // TMDB seasons 1 and 2
		title     string // TMDB title of TVDB S2E1
		directors string
	}{
		"other split": {`{"episodes":[{"episode_number":1,"name":"TMDB One"},{"episode_number":2,"name":"TMDB Two"},
			{"episode_number":3,"name":"TMDB Three (right)","crew":[{"job":"Director","name":"Right Director"}]}]}`,
			"TMDB Three (right)", "Right Director"},
		"no absolute 3 on TMDB": {`{"episodes":[{"episode_number":1,"name":"TMDB One"}]}`, "", ""},
	}
	for name, c := range cases {
		p := newTestProvider(t, &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
			switch path {
			case "api.themoviedb.org/3/tv/77/season/1":
				return 200, c.season1
			case "api.themoviedb.org/3/tv/77/season/2":
				return 200, wrong
			}
			return testSeries(path, q)
		}})
		p.cfg.EpisodeTitles = []string{"tmdb", "tvdb", "anidb"}
		s, err := p.buildShow(context.Background(), "tvdb-500")
		if err != nil {
			t.Fatal(err)
		}
		ep := s.Episode(2, 3)
		if ep == nil {
			t.Fatalf("%s: S02E03 missing", name)
		}
		if ep.titles["tmdb"] != c.title {
			t.Errorf("%s: S02E03 TMDB title %q, want %q", name, ep.titles["tmdb"], c.title)
		}
		if d := strings.Join(ep.Directors, ","); d != c.directors {
			t.Errorf("%s: S02E03 directors %q, want %q", name, d, c.directors)
		}
	}
}

// In /tvdb, a cached AniDB entry whose TMDB id has no TMDB season or
// exception (an id only Fribb has) doesn't pair at TheTVDB's season and
// number: TVDB S2E1 (absolute 3) stays TMDB S1E3, as before it was cached.
func TestTVDBEntryWithoutTMDBSeasonGoesByAbsoluteNumber(t *testing.T) {
	second := `<anime id="200"><type>TV Series</type><enddate>2010-01-01</enddate><episodes>
<episode><epno type="1">1</epno><title xml:lang="en">Second Start</title></episode></episodes></anime>`
	p := newTestProvider(t, &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		switch {
		case path == "api.anidb.net/httpapi" && q["aid"][0] == "200":
			return 200, second
		case path == "api.themoviedb.org/3/tv/77/season/1":
			return 200, `{"episodes":[{"episode_number":1,"name":"TMDB One"},{"episode_number":2,"name":"TMDB Two"},
				{"episode_number":3,"name":"TMDB Three (right)","crew":[{"job":"Director","name":"Right Director"}]}]}`
		case path == "api.themoviedb.org/3/tv/77/season/2":
			return 200, `{"episodes":[{"episode_number":1,"name":"TMDB S2E1 (wrong)","crew":[{"job":"Director","name":"Wrong Director"}]}]}`
		}
		return testSeries(path, q)
	}})
	p.cfg.EpisodeTitles = []string{"tmdb", "tvdb", "anidb"}
	p.lists.SetForTest(map[int]mapping.Entry{
		100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}, TMDB: mapping.Placement{ID: 77}},
		200: {AID: 200, TVDB: mapping.Placement{ID: 500, Season: 2, Mapped: true}, TMDB: mapping.Placement{ID: 77}},
	})
	if _, err := p.anidb.Anime(context.Background(), 200); err != nil {
		t.Fatal(err)
	}
	s, err := p.buildShow(context.Background(), "tvdb-500")
	if err != nil {
		t.Fatal(err)
	}
	ep := s.Episode(2, 3)
	if ep == nil {
		t.Fatal("S02E03 missing")
	}
	if ep.titles["tmdb"] != "TMDB Three (right)" || strings.Join(ep.Directors, ",") != "Right Director" {
		t.Errorf("S02E03: TMDB title %q directors %v, want TMDB S1E3's", ep.titles["tmdb"], ep.Directors)
	}
	if ep.titles["anidb"] != "Second Start" {
		t.Errorf("S02E03: AniDB title %q, want AniDB 200's (the entry is cached)", ep.titles["anidb"])
	}
}

// A partial episode list is used, but the show is rebuilt soon instead of
// being kept for an hour with episodes missing.
func TestPartialEpisodeListIsIncomplete(t *testing.T) {
	cases := map[string]func(path string, q map[string][]string) (int, string){
		"tvdb page 1 fails": func(path string, q map[string][]string) (int, string) {
			if path == "api4.thetvdb.com/v4/series/500/episodes/official/eng" {
				if q["page"][0] == "1" {
					return 500, `{}`
				}
				return 200, `{"data":{"episodes":[{"seasonNumber":1,"number":1,"name":"TVDB One"}]},"links":{"next":"page=1"}}`
			}
			return testSeries(path, q)
		},
		"tmdb season fails": func(path string, q map[string][]string) (int, string) {
			if path == "api.themoviedb.org/3/tv/77/season/2" {
				return 502, `{}`
			}
			return testSeries(path, q)
		},
	}
	for name, answer := range cases {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			p := newTestProvider(t, &fakeNet{answer: answer})
			s, err := p.buildShow(context.Background(), root)
			if err != nil {
				t.Fatalf("%s %s: %v", name, root, err)
			}
			if s.Complete {
				t.Errorf("%s %s: want an incomplete show (rebuilt in 2 min)", name, root)
			}
			if s.Episode(1, 1) == nil {
				t.Errorf("%s %s: what came back must still be used", name, root)
			}
		}
	}
	// A season TMDB lists but doesn't have (404) is nothing to wait for.
	p := newTestProvider(t, &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		if path == "api.themoviedb.org/3/tv/77/season/2" {
			return 404, `{}`
		}
		return testSeries(path, q)
	}})
	if s, err := p.buildShow(context.Background(), "tvdb-500"); err != nil || !s.Complete {
		t.Errorf("missing TMDB season: complete %v err %v, want complete", s != nil && s.Complete, err)
	}
}

// A show-level source that fails (or runs out of the build's time) leaves
// the show incomplete, so it's rebuilt in 2 minutes instead of being kept an
// hour without posters or details. A source without the show (404), or one
// that isn't configured, is nothing to wait for.
func TestFailedShowSourceIsIncomplete(t *testing.T) {
	failing := func(fail string, code int, wait time.Duration) func(string, map[string][]string) (int, string) {
		return func(path string, q map[string][]string) (int, string) {
			if path+"?"+strings.Join(q["append_to_response"], "") == fail {
				time.Sleep(wait)
				return code, `{}`
			}
			return testSeries(path, q)
		}
	}
	cases := []struct {
		name     string
		answer   func(string, map[string][]string) (int, string)
		timeout  time.Duration
		complete bool
	}{
		{"tmdb art 500", failing("api.themoviedb.org/3/tv/77/images?", 500, 0), 0, false},
		{"tmdb details 503", failing("api.themoviedb.org/3/tv/77?aggregate_credits,content_ratings", 503, 0), 0, false},
		{"out of time", failing("api.themoviedb.org/3/tv/77/images?", 200, 1500*time.Millisecond), time.Second, false},
		{"404 and MAL not configured", failing("api.themoviedb.org/3/tv/77/images?", 404, 0), 0, true},
	}
	for _, c := range cases {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			p := newTestProvider(t, &fakeNet{answer: c.answer})
			p.lists.SetForTest(map[int]mapping.Entry{100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true},
				TMDB: mapping.Placement{ID: 77}, MAL: 5}})
			p.cfg.PosterSources = []string{"mal", "tmdb", "tvdb"}
			ctx := context.Background()
			if c.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, c.timeout)
				defer cancel()
			}
			s, err := p.buildShow(ctx, root)
			if err != nil {
				t.Fatalf("%s %s: %v", c.name, root, err)
			}
			if s.Complete != c.complete {
				t.Errorf("%s %s: complete %v, want %v", c.name, root, s.Complete, c.complete)
			}
		}
	}
}

// A rejected TVDB key is tried once, not once per TVDB call. /tvdb shows
// then fail with the reason: TheMovieDb never stands in for TheTVDB's list.
func TestRejectedTVDBKeyIsRemembered(t *testing.T) {
	net := &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		if path == "api4.thetvdb.com/v4/login" {
			return 401, `{}`
		}
		return testSeries(path, q)
	}}
	p := newTestProvider(t, net)
	for range 3 {
		if s, err := p.buildShow(context.Background(), "tvdb-500"); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("show %v, err %v: want the TVDB login error", s != nil, err)
		}
	}
	if n := net.count("api4.thetvdb.com/v4/login"); n != 1 {
		t.Errorf("TVDB login tried %d times, want 1", n)
	}
}

// In /tvdb, an episode whose AniDB entry the mapping puts on another TMDB
// show has no TheMovieDb pairing, even when this show has an episode at its
// absolute number. An episode the mapping can't answer for (its AniDB entry
// isn't cached yet) still goes by absolute number.
func TestTVDBEpisodeOnOtherTMDBShowIsNotPaired(t *testing.T) {
	second := `<anime id="200"><type>TV Series</type><enddate>2010-01-01</enddate><episodes>
<episode><epno type="1">1</epno><title xml:lang="en">Second Start</title></episode></episodes></anime>`
	for name, c := range map[string]struct {
		cached           bool
		title, directors string // TMDB title and directors of TVDB S2E1 (absolute 3)
	}{
		"on another TMDB show": {true, "", ""},
		"not cached yet":       {false, "TMDB Three", "TMDB Director 3"},
	} {
		p := newTestProvider(t, &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
			switch {
			case path == "api.anidb.net/httpapi" && q["aid"][0] == "200":
				if !c.cached {
					return 500, ""
				}
				return 200, second
			case path == "api.themoviedb.org/3/tv/77/season/1":
				return 200, `{"episodes":[{"episode_number":1,"name":"TMDB One"},{"episode_number":2,"name":"TMDB Two"},
					{"episode_number":3,"name":"TMDB Three","crew":[{"job":"Director","name":"TMDB Director 3"}]}]}`
			case path == "api.themoviedb.org/3/tv/77/season/2":
				return 200, `{"episodes":[]}`
			}
			return testSeries(path, q)
		}})
		p.cfg.EpisodeTitles = []string{"tmdb", "tvdb", "anidb"}
		p.lists.SetForTest(map[int]mapping.Entry{
			100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}, TMDB: mapping.Placement{ID: 77}},
			200: {AID: 200, TVDB: mapping.Placement{ID: 500, Season: 2, Mapped: true}, TMDB: mapping.Placement{ID: 88, Mapped: true, Season: 1}},
		})
		if c.cached {
			if _, err := p.anidb.Anime(context.Background(), 200); err != nil {
				t.Fatal(err)
			}
		}
		s, err := p.buildShow(context.Background(), "tvdb-500")
		if err != nil {
			t.Fatal(err)
		}
		ep := s.Episode(2, 3)
		if ep == nil {
			t.Fatalf("%s: S02E03 missing", name)
		}
		if ep.titles["tmdb"] != c.title {
			t.Errorf("%s: S02E03 TMDB title %q, want %q", name, ep.titles["tmdb"], c.title)
		}
		if d := strings.Join(ep.Directors, ","); d != c.directors {
			t.Errorf("%s: S02E03 directors %q, want %q", name, d, c.directors)
		}
		if c.cached && ep.Title != "TVDB Three" {
			t.Errorf("%s: S02E03 title %q, want TheTVDB's", name, ep.Title)
		}
	}
}

// /tvdb takes its episode list only from TheTVDB. Without a TheTVDB key, or
// for a show TheTVDB doesn't have, the build fails with what to do, and
// TheMovieDb (which has the show here) doesn't stand in.
func TestTVDBListOnlyFromTheTVDB(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	p.tvdb = sources.NewTVDB(fetch.New(t.TempDir()), "", "", "")
	if _, err := p.buildShow(context.Background(), "tvdb-500"); err == nil || !strings.Contains(err.Error(), "TVDB_API_KEY") {
		t.Errorf("no TVDB key: %v, want an error naming TVDB_API_KEY", err)
	}
	p = newTestProvider(t, &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		if path == "api4.thetvdb.com/v4/series/500/episodes/official/eng" {
			return 404, `{}`
		}
		return testSeries(path, q)
	}})
	if _, err := p.buildShow(context.Background(), "tvdb-500"); err == nil || !strings.Contains(err.Error(), "use /anidb") {
		t.Errorf("show not on TheTVDB: %v, want an error pointing to /anidb", err)
	}
}

// A TMDB mapping exception without a counterpart (episode 0) pairs the
// episode with nothing on TheMovieDb, in both modes: in /tvdb it doesn't
// fall back to the absolute number (TMDB S1E2 here).
func TestTMDBExceptionWithoutCounterpartIsNotPaired(t *testing.T) {
	for _, root := range []string{"tvdb-500", "anidb-100"} {
		p := newTestProvider(t, &fakeNet{answer: testSeries})
		p.lists.SetForTest(map[int]mapping.Entry{100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true},
			TMDB: mapping.Placement{ID: 77, Mapped: true, Season: 1,
				Episodes: []mapping.EpisodeMap{{AniDBSeason: 1, AniDBEp: 2, Season: 1, Ep: 0}}}}})
		s, err := p.buildShow(context.Background(), root)
		if err != nil {
			t.Fatalf("%s: %v", root, err)
		}
		if ep := s.Episode(1, 1); ep == nil || ep.titles["tmdb"] != "TMDB One" {
			t.Errorf("%s S01E01: %+v, want paired with TMDB S1E1", root, ep)
		}
		ep := s.Episode(1, 2)
		if ep == nil {
			t.Fatalf("%s: S01E02 missing", root)
		}
		if ep.titles["tmdb"] != "" {
			t.Errorf("%s S01E02: TMDB title %q, want none", root, ep.titles["tmdb"])
		}
		if w := strings.Join(ep.Writers, ","); w != "" {
			t.Errorf("%s S01E02: writers %q, want none (TMDB's would be from an unrelated episode)", root, w)
		}
	}
}

// An /anidb movie built before AniDB answers already uses its TMDB movie:
// an entry with only a TMDB movie id is taken for a movie.
func TestAniDBMovieWithoutDetailsUsesTMDBMovie(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		switch path {
		case "api.anidb.net/httpapi":
			return 500, ""
		case "api.themoviedb.org/3/movie/99":
			return 200, `{"title":"TMDB Movie","overview":"From TMDB's movie."}`
		case "api.themoviedb.org/3/movie/99/images":
			return 200, `{"posters":[{"file_path":"/movie.jpg"}]}`
		}
		return testSeries(path, q)
	}})
	p.lists.SetForTest(map[int]mapping.Entry{100: {AID: 100, TMDBMovie: 99}})
	s, err := p.buildShow(context.Background(), "anidb-100")
	if err != nil {
		t.Fatal(err)
	}
	if s.Summary != "From TMDB's movie." {
		t.Errorf("summary %q, want TMDB's movie's", s.Summary)
	}
	if len(s.Art.Posters) == 0 || s.Art.Posters[0] != "https://image.tmdb.org/t/p/original/movie.jpg" {
		t.Errorf("posters %v, want TMDB's movie poster", s.Art.Posters)
	}
	if !slices.Contains(s.Guids, "tmdb://99") {
		t.Errorf("guids %v, want tmdb://99", s.Guids)
	}
}

// An /anidb movie has the same GUIDs whether AniDB has answered yet or not
// (no tvdb:// for the TV series it is mapped to), and its one episode is
// the movie, under the show's title, in both builds.
func TestAniDBMovieGuidsWithAndWithoutDetails(t *testing.T) {
	movieXML := `<anime id="100"><type>Movie</type><startdate>2010-05-01</startdate>
<titles><title type="main" xml:lang="x-jat">Kizuna Test</title></titles><description>The movie.</description>
<episodes><episode><epno type="1">1</epno><length>90</length><title xml:lang="en">Complete Movie</title></episode></episodes></anime>`
	var guids [2][]string
	for i, details := range []bool{false, true} {
		p := newTestProvider(t, &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
			if path == "api.anidb.net/httpapi" {
				if !details {
					return 500, ""
				}
				return 200, movieXML
			}
			return testSeries(path, q)
		}})
		p.lists.SetForTest(map[int]mapping.Entry{100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 0, Mapped: true}, TMDBMovie: 99}})
		s, err := p.buildShow(context.Background(), "anidb-100")
		if err != nil {
			t.Fatal(err)
		}
		if s.Placeholders == details {
			t.Fatalf("details %v: placeholders %v", details, s.Placeholders)
		}
		guids[i] = s.Guids
		if ep := s.Episode(1, 1); ep == nil || ep.Title != s.Title || s.Title != "Kizuna Test" {
			t.Errorf("details %v: S01E01 %+v, want the movie under the show's title %q", details, ep, s.Title)
		}
	}
	if !slices.Equal(guids[0], guids[1]) || slices.Contains(guids[0], "tvdb://500") || !slices.Contains(guids[0], "tmdb://99") {
		t.Errorf("guids without details %v, with %v: want the same, tmdb://99 and no tvdb://500", guids[0], guids[1])
	}
}

// A /tvdb build reads TheMovieDb's episode list once, for both the titles
// and the crew, and a failed list still leaves the show incomplete.
func TestTVDBReadsTMDBListOnce(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	var mu sync.Mutex
	reads := 0
	ctx := fetch.WithObserver(context.Background(), func(e fetch.Event) {
		if e.Key == "tmdb/tv-77-s1.json" {
			mu.Lock()
			reads++
			mu.Unlock()
		}
	})
	s, err := p.buildShow(ctx, "tvdb-500")
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Errorf("TMDB season 1 read %d times, want 1", reads)
	}
	if ep := s.Episode(1, 2); ep == nil || ep.titles["tmdb"] != "TMDB Two" || strings.Join(ep.Writers, ",") != "TMDB Writer" {
		t.Errorf("S01E02: %+v, want TMDB's title and writer", ep)
	}

	for _, titles := range [][]string{{"tmdb", "tvdb"}, {"tvdb"}} { // the crew needs the list even when the titles don't
		p := newTestProvider(t, &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
			if path == "api.themoviedb.org/3/tv/77/season/2" {
				return 502, `{}`
			}
			return testSeries(path, q)
		}})
		p.cfg.EpisodeTitles = titles
		if s, err := p.buildShow(context.Background(), "tvdb-500"); err != nil || s.Complete {
			t.Errorf("titles %v, TMDB season fails: complete %v err %v, want incomplete", titles, s != nil && s.Complete, err)
		}
	}
}

// captureLogs sends slog's lines to w until restore is called. slog.SetDefault
// also redirects the log package, which restoring the old logger doesn't
// undo, so its output and flags are put back too.
func captureLogs(w io.Writer) (restore func()) {
	old, out, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))
	return func() {
		slog.SetDefault(old)
		log.SetOutput(out)
		log.SetFlags(flags)
	}
}

// /tvdb works without ANIDB_CLIENT: the show has no AniDB details to wait
// for, so it is complete and not rebuilt (and logged) every 2 minutes. With
// AniDB set up but not answering, it is rebuilt soon.
func TestTVDBWithoutAniDBIsComplete(t *testing.T) {
	failing := func(path string, q map[string][]string) (int, string) {
		if path == "api.anidb.net/httpapi" {
			return 500, ""
		}
		return testSeries(path, q)
	}
	for name, c := range map[string]struct {
		client   string
		complete bool
	}{
		"AniDB not configured": {"", true},
		"AniDB not answering":  {"kizunatest", false},
	} {
		net := &fakeNet{answer: failing}
		p := newTestProvider(t, net)
		dir := t.TempDir()
		f := fetch.New(dir)
		f.SetTransport(net)
		p.anidb = sources.NewAniDB(f, dir, c.client, "1")
		var logs strings.Builder
		restore := captureLogs(&logs)
		s, err := p.buildShow(context.Background(), "tvdb-500")
		restore()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.Complete != c.complete {
			t.Errorf("%s: complete %v, want %v", name, s.Complete, c.complete)
		}
		if s.Title != "Kizuna Test" {
			t.Errorf("%s: title %q, want the title dump's", name, s.Title)
		}
		if logged := strings.Contains(logs.String(), "AniDB details not available"); logged == c.complete {
			t.Errorf("%s: AniDB details logged %v, want %v:\n%s", name, logged, !c.complete, logs.String())
		}
	}
}

// /tvdb without AniDB details still has an original title (the title
// dump's) and a runtime: the average of TheTVDB's regular episodes.
func TestTVDBWithoutAniDBDetails(t *testing.T) {
	net := &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		if path == "api4.thetvdb.com/v4/series/500/episodes/official/eng" {
			return 200, `{"data":{"episodes":[
				{"id":11,"seasonNumber":1,"number":1,"name":"TVDB One","runtime":24},
				{"id":12,"seasonNumber":1,"number":2,"name":"TVDB Two","runtime":26},
				{"id":13,"seasonNumber":2,"number":1,"name":"TVDB Three"},
				{"id":14,"seasonNumber":0,"number":1,"name":"TVDB Special","runtime":90}]},"links":{"next":null}}`
		}
		return testSeries(path, q)
	}}
	p := newTestProvider(t, net)
	dir := t.TempDir()
	f := fetch.New(dir)
	f.SetTransport(net)
	p.anidb = sources.NewAniDB(f, dir, "", "1") // AniDB not configured
	p.titles.SetForTest(map[int][]anidb.Title{100: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test"},
		{Type: "official", Lang: "ja", Text: "絆テスト"}}})
	s, err := p.buildShow(context.Background(), "tvdb-500")
	if err != nil {
		t.Fatal(err)
	}
	if s.OriginalTitle != "絆テスト" {
		t.Errorf("original title %q, want the title dump's", s.OriginalTitle)
	}
	if s.Minutes != 25 {
		t.Errorf("runtime %d, want 25 (TheTVDB's regular episodes; the special doesn't count)", s.Minutes)
	}
}
