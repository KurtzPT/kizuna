package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"kizuna/internal/fetch"
	"kizuna/internal/sources"
)

// fullTVDB: testSeries, with TheTVDB's episodes complete (a summary, date,
// runtime and picture each).
func fullTVDB(path string, q map[string][]string) (int, string) {
	if path == "api4.thetvdb.com/v4/series/500/episodes/official/eng" {
		return 200, `{"data":{"episodes":[
			{"id":11,"seasonNumber":1,"number":1,"name":"TVDB One","image":"/e1.jpg","overview":"O1","aired":"2009-04-01","runtime":24},
			{"id":12,"seasonNumber":1,"number":2,"name":"TVDB Two","image":"/e2.jpg","overview":"O2","aired":"2009-04-08","runtime":24},
			{"id":13,"seasonNumber":2,"number":1,"name":"TVDB Three","image":"/e3.jpg","overview":"O3","aired":"2009-05-01","runtime":24},
			{"id":14,"seasonNumber":0,"number":1,"name":"TVDB Special","image":"/s1.jpg","overview":"OS","aired":"2009-06-01","runtime":24}]},
			"links":{"next":null}}`
	}
	return testSeries(path, q)
}

// tvdbOff: TheTVDB not set up.
func tvdbOff(t *testing.T) func(p *Provider) {
	return func(p *Provider) { p.tvdb = sources.NewTVDB(fetch.New(t.TempDir()), "", "", "") }
}

// Plex's episode list is read only when it can add something: not when the
// sources before plex gave every episode its values; when TheTVDB is off or
// its list failed, a gap is left, or plex comes first. For titles and fill
// values, in /tvdb only a missing title is a gap; those cases leave plex out
// of EPISODE_CREW_SOURCES, whose own gaps (a director or a writer) are the
// crew cases.
func TestPlexEpisodeListRead(t *testing.T) {
	failed := func(path string, q map[string][]string) (int, string) {
		if path == "api4.thetvdb.com/v4/series/500/episodes/official/eng" {
			return 503, `{}`
		}
		return fullTVDB(path, q)
	}
	noCrew := func(set func(p *Provider)) func(p *Provider) { // plex not in EPISODE_CREW_SOURCES
		return func(p *Provider) {
			p.cfg.EpisodeCrewSources = []string{"tvdb", "tmdb"}
			if set != nil {
				set(p)
			}
		}
	}
	crewOnly := func(tvdbOn bool, order ...string) func(p *Provider) { // plex in EPISODE_CREW_SOURCES alone
		return func(p *Provider) {
			if !tvdbOn {
				tvdbOff(t)(p)
			}
			p.cfg.EpisodeTitles, p.cfg.EpisodeFillSources, p.cfg.EpisodeCrewSources = []string{"anidb", "tvdb", "tmdb"}, []string{"tvdb"}, order
		}
	}
	for _, c := range []struct {
		name   string
		answer func(string, map[string][]string) (int, string)
		root   string
		set    func(p *Provider)
		read   bool
	}{
		{"anidb: nothing missing", fullTVDB, "anidb-100", noCrew(nil), false},
		{"anidb: pictures and summaries missing", testSeries, "anidb-100", nil, true},
		{"anidb: TheTVDB off", fullTVDB, "anidb-100", tvdbOff(t), true},
		{"anidb: TheTVDB's list failed", failed, "anidb-100", nil, true},
		{"anidb: plex first in EPISODE_TITLES", fullTVDB, "anidb-100", func(p *Provider) { p.cfg.EpisodeTitles = []string{"plex", "anidb", "tvdb"} }, true},
		{"anidb: plex first in EPISODE_FILL_SOURCES", fullTVDB, "anidb-100", func(p *Provider) { p.cfg.EpisodeFillSources = []string{"plex", "tvdb"} }, true},
		{"anidb: only plex for titles", fullTVDB, "anidb-100", func(p *Provider) { p.cfg.EpisodeTitles = []string{"plex"} }, true},
		{"anidb: plex not listed", testSeries, "anidb-100", noCrew(func(p *Provider) {
			p.cfg.EpisodeTitles, p.cfg.EpisodeFillSources = []string{"anidb", "tvdb"}, []string{"tvdb"}
		}), false},
		{"tvdb: pictures and summaries missing are no gap", testSeries, "tvdb-500", noCrew(nil), false},
		{"tvdb: a title missing", testSeries, "tvdb-500", noCrew(func(p *Provider) { p.cfg.EpisodeTitles = []string{"tmdb", "plex", "tvdb"} }), true},
		{"anidb crew: no gap", fullTVDB, "anidb-100", crewOnly(true, "tvdb", "tmdb", "plex"), false},
		{"tvdb crew: no gap", fullCrew, "tvdb-500", crewOnly(true, "tvdb", "tmdb", "plex"), false},
		{"anidb crew: a writer missing", crewGap, "anidb-100", crewOnly(true, "tvdb", "tmdb", "plex"), true},
		{"tvdb crew: a writer missing", testSeries, "tvdb-500", crewOnly(true, "tvdb", "tmdb", "plex"), true},
		{"anidb crew: a writer missing, tmdb not listed", fullTVDB, "anidb-100", crewOnly(true, "tvdb", "plex"), true},
		{"anidb crew: plex first", fullCrew, "anidb-100", crewOnly(true, "plex", "tvdb", "tmdb"), true},
		{"tvdb crew: plex first", fullCrew, "tvdb-500", crewOnly(true, "plex", "tvdb", "tmdb"), true},
		{"anidb crew: TheTVDB off, no gap", fullCrew, "anidb-100", crewOnly(false, "tvdb", "tmdb", "plex"), false},
		{"anidb crew: TheTVDB off, a gap", crewGap, "anidb-100", crewOnly(false, "tvdb", "tmdb", "plex"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			net := &fakeNet{answer: plexService(c.answer)}
			p := newTestProvider(t, net)
			withPlex(t, p, net)
			if c.set != nil {
				c.set(p)
			}
			if _, err := p.buildShow(context.Background(), c.root); err != nil {
				t.Fatal(err)
			}
			if got := plexAsked(net, "s1/children") > 0; got != c.read {
				t.Errorf("Plex's episode list read: %v, want %v", got, c.read)
			}
		})
	}
}

// EPISODE_FILL_SOURCES is an order: per episode and value, AniDB's own
// stays, then the first listed source with one gives it.
func TestPlexEpisodeFillOrder(t *testing.T) {
	for _, c := range []struct {
		order         []string
		thumb, source string
	}{
		{[]string{"tvdb", "plex"}, "https://artworks.thetvdb.com/e1.jpg", "tvdb"},
		{[]string{"plex", "tvdb"}, "https://plex/e1-1.jpg", "plex"},
	} {
		net := &fakeNet{answer: plexService(fullTVDB)}
		p := newTestProvider(t, net)
		withPlex(t, p, net)
		p.cfg.EpisodeFillSources = c.order
		s, err := p.buildShow(context.Background(), "anidb-100")
		if err != nil {
			t.Fatal(err)
		}
		ep := s.Episode(1, 1)
		if ep.Thumb != c.thumb || ep.src.thumb.name() != c.source || ep.Minutes != 24 || ep.src.minutes != srcAniDB {
			t.Errorf("%v: thumb %q from %s, minutes %d from %s; want %q from %s, AniDB's minutes",
				c.order, ep.Thumb, ep.src.thumb.name(), ep.Minutes, ep.src.minutes.name(), c.thumb, c.source)
		}
		want := map[string]string{"tvdb": "O1", "plex": "Plex summary 1-1"}[c.source]
		if ep.Summary != want {
			t.Errorf("%v: summary %q, want %q", c.order, ep.Summary, want)
		}
	}
}

// fullCrew: fullTVDB, with every episode's directors and writers at
// TheTVDB (each record has both) and TheMovieDb's season 1 complete too.
func fullCrew(path string, q map[string][]string) (int, string) {
	switch path {
	case "api4.thetvdb.com/v4/episodes/11/extended", "api4.thetvdb.com/v4/episodes/12/extended",
		"api4.thetvdb.com/v4/episodes/13/extended", "api4.thetvdb.com/v4/episodes/14/extended":
		return 200, `{"data":{"characters":[{"personName":"TVDB Writer","peopleType":"Writer"},
			{"personName":"TVDB Director","peopleType":"Director"}]}}`
	case "api.themoviedb.org/3/tv/77/season/1":
		return 200, `{"episodes":[{"episode_number":1,"name":"TMDB One","crew":[{"job":"Director","department":"Directing","name":"TMDB Director"},
				{"job":"Screenplay","department":"Writing","name":"TMDB Writer"}]},
			{"episode_number":2,"name":"TMDB Two","crew":[{"job":"Screenplay","department":"Writing","name":"TMDB Writer 2"},
				{"job":"Director","department":"Directing","name":"TMDB Director 2"}]}]}`
	}
	return fullTVDB(path, q)
}

// crewGap: fullTVDB, where S01E02 has a writer at neither TheTVDB (record
// 12) nor TheMovieDb: a crew gap in both modes, and none for titles or fill
// values.
func crewGap(path string, q map[string][]string) (int, string) {
	if path == "api.themoviedb.org/3/tv/77/season/1" {
		return 200, `{"episodes":[{"episode_number":1,"name":"TMDB One","crew":[{"job":"Director","department":"Directing","name":"TMDB Director"}]},
			{"episode_number":2,"name":"TMDB Two","crew":[{"job":"Director","department":"Directing","name":"TMDB Director 2"}]}]}`
	}
	return fullTVDB(path, q)
}

// crewWant is an episode's directors and writers, and the source of each
// ("" for none), as the match report shows them (Ep.src).
type crewWant struct {
	season, index      int
	directors, writers string
	dSrc, wSrc         string
}

// checkCrew compares a show's episodes with wants.
func checkCrew(t *testing.T, name string, s *Show, wants []crewWant) {
	t.Helper()
	for _, w := range wants {
		ep := s.Episode(w.season, w.index)
		if ep == nil {
			t.Errorf("%s S%02dE%02d missing", name, w.season, w.index)
			continue
		}
		got := crewWant{w.season, w.index, strings.Join(ep.Directors, ","), strings.Join(ep.Writers, ","),
			ep.src.directors.name(), ep.src.writers.name()}
		if got != w {
			t.Errorf("%s S%02dE%02d: directors %q (%s) writers %q (%s), want %q (%s) %q (%s)", name, w.season, w.index,
				got.directors, got.dSrc, got.writers, got.wSrc, w.directors, w.dSrc, w.writers, w.wSrc)
		}
	}
}

// Episode directors and writers from Plex: where the sources listed before
// plex in EPISODE_CREW_SOURCES have none, each field on its own. Its list is
// read for that alone when an episode has such a gap, also with plex in no
// other list, and a list read only for crew gives no titles and no fill
// values. Both modes.
func TestPlexEpisodeCrew(t *testing.T) {
	wants := map[string][]crewWant{
		"tvdb-500": {
			{1, 1, "TVDB Director", "TVDB Writer", "tvdb", "tvdb"},
			{1, 2, "TVDB Director 2", "Plex Writer", "tvdb", "plex"},
			{2, 3, "TMDB Director 3", "Plex Writer", "tmdb", "plex"}, // no TheTVDB record, no TheMovieDb writer
			{0, 1, "Plex Director", "Plex Writer", "plex", "plex"},
		},
		"anidb-100": {
			{1, 1, "TVDB Director", "TVDB Writer", "tvdb", "tvdb"},
			{1, 2, "TVDB Director 2", "Plex Writer", "tvdb", "plex"},
		},
	}
	for _, c := range []struct {
		name string
		set  func(p *Provider)
	}{
		{"default lists", nil},
		{"plex only in EPISODE_CREW_SOURCES", func(p *Provider) {
			p.cfg.EpisodeTitles, p.cfg.EpisodeFillSources = []string{"anidb", "tvdb", "tmdb"}, []string{"tvdb"}
		}},
	} {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			name := c.name + ", " + root
			net := &fakeNet{answer: plexService(crewGap)}
			p := newTestProvider(t, net)
			withPlex(t, p, net)
			p.cfg.EpisodeCrewSources = []string{"tvdb", "tmdb", "plex"}
			if c.set != nil {
				c.set(p)
			}
			s, err := p.buildShow(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if read := plexAsked(net, "s1/children") > 0; !read || !s.Complete {
				t.Errorf("%s: Plex's list read %v, complete %v; want read for the gap, complete", name, read, s.Complete)
			}
			checkCrew(t, name, s, wants[root])
			s.eachEpisode(func(ep *Ep) {
				if ep.titles["plex"] != "" || ep.titleSrc == "plex" ||
					slices.Contains([]srcCode{ep.src.summary, ep.src.thumb, ep.src.date, ep.src.minutes}, srcPlex) {
					t.Errorf("%s S%02dE%02d: title %q from %s, sources %+v; want nothing but crew from Plex",
						name, ep.Season, ep.Index, ep.titles["plex"], ep.titleSrc, ep.src)
				}
			})
		}
	}
}

// A list without plex gives no crew from Plex, also when its list was read
// for titles. Both modes.
func TestPlexEpisodeCrewNotListed(t *testing.T) {
	for _, c := range []struct {
		root  string
		wants []crewWant
	}{
		{"tvdb-500", []crewWant{{1, 2, "TVDB Director 2", "", "tvdb", ""}, {0, 1, "", "", "", ""}}},
		{"anidb-100", []crewWant{{1, 2, "TVDB Director 2", "", "tvdb", ""}}},
	} {
		net := &fakeNet{answer: plexService(crewGap)}
		p := newTestProvider(t, net)
		withPlex(t, p, net)
		p.cfg.EpisodeTitles, p.cfg.EpisodeCrewSources = []string{"plex", "anidb", "tvdb"}, []string{"tvdb", "tmdb"}
		s, err := p.buildShow(context.Background(), c.root)
		if err != nil {
			t.Fatal(err)
		}
		if ep := s.Episode(1, 2); plexAsked(net, "s1/children") == 0 || ep == nil || ep.Title != "Plex Two" {
			t.Fatalf("%s: Plex's list not read for titles (S01E02 %+v)", c.root, ep)
		}
		checkCrew(t, c.root, s, c.wants)
	}
}

// plex first in EPISODE_CREW_SOURCES: Plex's list is always read, and its
// crew wins wherever Plex has the episode. Both modes.
func TestPlexEpisodeCrewFirst(t *testing.T) {
	at := func(season, index int) crewWant {
		return crewWant{season, index, "Plex Director", "Plex Writer", "plex", "plex"}
	}
	for _, c := range []struct {
		root  string
		wants []crewWant
	}{
		{"tvdb-500", []crewWant{at(1, 1), at(1, 2), at(2, 3), at(0, 1)}},
		{"anidb-100", []crewWant{at(1, 1), at(1, 2)}},
	} {
		net := &fakeNet{answer: plexService(fullCrew)}
		p := newTestProvider(t, net)
		withPlex(t, p, net)
		p.cfg.EpisodeCrewSources = []string{"plex", "tvdb", "tmdb"}
		s, err := p.buildShow(context.Background(), c.root)
		if err != nil {
			t.Fatal(err)
		}
		if plexAsked(net, "s1/children") == 0 {
			t.Errorf("%s: Plex's list not read", c.root)
		}
		checkCrew(t, c.root, s, c.wants)
	}
}

// While TheTVDB's records are still on their way and tvdb comes before
// plex, a gap can't be told from a record still to come: Plex's list is not
// read for crew, and the build is incomplete. The rebuild, with the records
// in, reads it for the gap. With plex first it is read all the same, and as
// Plex gives every episode both fields, no record is asked for: nothing is
// pending. Both modes.
func TestPlexEpisodeCrewTVDBPending(t *testing.T) {
	for _, c := range []struct {
		root      string
		order     []string
		plexFirst bool
	}{
		{"tvdb-500", []string{"tvdb", "tmdb", "plex"}, false},
		{"anidb-100", []string{"tvdb", "tmdb", "plex"}, false},
		{"tvdb-500", []string{"plex", "tvdb", "tmdb"}, true},
		{"anidb-100", []string{"plex", "tvdb", "tmdb"}, true},
	} {
		name := fmt.Sprintf("%s %v", c.root, c.order)
		net := &fakeNet{answer: plexService(crewGap)}
		p := newTestProvider(t, net)
		withPlex(t, p, net)
		p.cfg.EpisodeCrewSources = c.order
		restore := sources.SetCrewBudgetForTest(0) // every record goes to the background: pending
		t.Cleanup(restore)                         // also when the build panics
		s, err := p.buildShow(context.Background(), c.root)
		restore()
		if err != nil {
			t.Fatal(err)
		}
		if read := plexAsked(net, "s1/children") > 0; read != c.plexFirst || s.Complete != c.plexFirst {
			t.Errorf("%s, records pending: Plex's list read %v, complete %v; want read and complete %v", name, read, s.Complete, c.plexFirst)
		}
		p.tvdb.WaitIdle() // the records are in
		if n := tvdbCrewAsked(net); c.plexFirst && n != 0 {
			t.Errorf("%s: %d TheTVDB crew records asked, want none: Plex filled every episode", name, n)
		}
		if s, err = p.buildShow(context.Background(), c.root); err != nil {
			t.Fatal(err)
		}
		if read := plexAsked(net, "s1/children") > 0; !read || !s.Complete {
			t.Errorf("%s, rebuilt: Plex's list read %v, complete %v; want read, complete", name, read, s.Complete)
		}
		if ep := s.Episode(1, 2); ep == nil || strings.Join(ep.Writers, ",") != "Plex Writer" {
			t.Errorf("%s, rebuilt: S01E02 %+v, want Plex's writer", name, ep)
		}
	}
}

// Plex's episodes set no TheTVDB episode: no episode GUIDs and no crew
// requests come from them.
func TestPlexEpisodesNoTVDBEpisode(t *testing.T) {
	net := &fakeNet{answer: plexService(testSeries)}
	p := newTestProvider(t, net)
	withPlex(t, p, net)
	tvdbOff(t)(p)
	s, err := p.buildShow(context.Background(), "anidb-100")
	if err != nil {
		t.Fatal(err)
	}
	ep := s.Episode(1, 2)
	if ep == nil || ep.Thumb != "https://plex/e1-2.jpg" || ep.tvdbEp != 0 || ep.titles["plex"] != "Plex Two" {
		t.Fatalf("S01E02 %+v, want Plex's picture and title, no TheTVDB episode", ep)
	}
	it := p.episodeItem(Modes["anidb"], s, ep)
	for _, g := range it.Guid {
		if strings.HasPrefix(g.ID, "tvdb://") {
			t.Errorf("episode GUID %s from Plex's list", g.ID)
		}
	}
	if net.count("api4.thetvdb.com/v4/episodes/12/extended") != 0 {
		t.Error("a TheTVDB crew record asked")
	}
}

// /tvdb: Plex's titles land on each episode's own TheTVDB spot, also where
// TheMovieDb splits the seasons differently.
func TestPlexTitlesInTVDBMode(t *testing.T) {
	net := &fakeNet{answer: plexService(func(path string, q map[string][]string) (int, string) {
		switch path { // TheMovieDb: one season of three
		case "api.themoviedb.org/3/tv/77":
			return 200, `{"name":"TMDB Name","seasons":[{"season_number":1}]}`
		case "api.themoviedb.org/3/tv/77/season/1":
			return 200, `{"episodes":[{"episode_number":1,"name":"TMDB One"},{"episode_number":2,"name":"TMDB Two"},{"episode_number":3,"name":"TMDB Three"}]}`
		}
		return testSeries(path, q)
	})}
	p := newTestProvider(t, net)
	withPlex(t, p, net)
	p.cfg.EpisodeTitles = []string{"plex", "tmdb"}
	s, err := p.buildShow(context.Background(), "tvdb-500")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct {
		season, index int
		title         string
	}{{1, 1, "Plex One"}, {1, 2, "Plex Two"}, {2, 3, "Plex Three"}, {0, 1, "Plex Special"}} {
		if ep := s.Episode(w.season, w.index); ep == nil || ep.Title != w.title {
			t.Errorf("S%02dE%02d: %+v, want %q", w.season, w.index, ep, w.title)
		}
	}
}

// ART_ALL_SOURCES: /images is asked for, and Plex's copies of TheTVDB's and
// TheMovieDb's pictures are left out when that site is set up and listed,
// kept when it isn't.
func TestPlexAllArt(t *testing.T) {
	for _, c := range []struct {
		name               string
		set                func(p *Provider)
		posters, backdrops string
	}{
		{"both set up and listed", nil,
			"https://image.tmdb.org/t/p/original/p.jpg https://plex/poster.jpg https://assets.fanart.tv/poster.jpg",
			"https://plex/bg.jpg https://plex/gracenote-bg.jpg"},
		{"TheMovieDb not listed", func(p *Provider) { p.cfg.PosterSources = []string{"tvdb", "plex"} },
			"https://plex/poster.jpg https://image.tmdb.org/t/p/original/plex-tmdb.jpg https://assets.fanart.tv/poster.jpg",
			"https://plex/bg.jpg https://plex/gracenote-bg.jpg"},
		{"TheTVDB off", tvdbOff(t),
			"https://image.tmdb.org/t/p/original/p.jpg https://plex/poster.jpg https://assets.fanart.tv/poster.jpg",
			"https://plex/bg.jpg https://artworks.thetvdb.com/plex-tvdb-bg.jpg https://plex/gracenote-bg.jpg"},
	} {
		t.Run(c.name, func(t *testing.T) {
			net := &fakeNet{answer: plexService(testSeries)}
			p := newTestProvider(t, net)
			withPlex(t, p, net)
			p.cfg.ArtAllSources = true
			if c.set != nil {
				c.set(p)
			}
			entry := p.lists.ByAID(100)
			ss := p.newSources(context.Background(), 100, entry, 500, 77, false, firstEntry{true, true})
			ss.setAniDB(context.Background(), nil, entry)
			s := &Show{Episodes: map[int][]Ep{1: nil}}
			ss.fill(s)
			if got := strings.Join(s.Art.Posters, " "); got != c.posters {
				t.Errorf("posters %s\nwant    %s", got, c.posters)
			}
			if got := strings.Join(s.Art.Backgrounds, " "); got != c.backdrops {
				t.Errorf("backgrounds %s\nwant        %s", got, c.backdrops)
			}
			if plexAsked(net, "/images") != 1 {
				t.Error("/images not asked")
			}
		})
	}
}
