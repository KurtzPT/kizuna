package provider

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

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
	lists.SetForTest(map[int]mapping.Entry{100: {AID: 100, TVDB: 500, TVDBSeason: 1, TMDBTV: 77}})
	cfg := config.Load()
	return New(cfg, titles, sources.NewAniDB(f, dir, "kizunatest", "1"), lists,
		sources.NewTVDB(f, "key", "", ""), sources.NewTMDB(f, "key"), sources.NewMAL(f, ""), sources.NewANN(f))
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

// A rejected TVDB key is tried once, not once per TVDB call.
func TestRejectedTVDBKeyIsRemembered(t *testing.T) {
	net := &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		if path == "api4.thetvdb.com/v4/login" {
			return 401, `{}`
		}
		return testSeries(path, q)
	}}
	p := newTestProvider(t, net)
	for range 3 {
		s, err := p.buildShow(context.Background(), "tvdb-500") // falls back to TMDB's episode list
		if err != nil {
			t.Fatal(err)
		}
		if ep := s.Episode(2, 3); ep == nil {
			t.Fatalf("S02E03 from TMDB missing: %v", s.Episodes)
		}
	}
	if n := net.count("api4.thetvdb.com/v4/login"); n != 1 {
		t.Errorf("TVDB login tried %d times, want 1", n)
	}
}
