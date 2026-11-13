package provider

import (
	"context"
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
// its list failed, a gap is left, or plex comes first. In /tvdb only a
// missing title is a gap.
func TestPlexEpisodeListRead(t *testing.T) {
	failed := func(path string, q map[string][]string) (int, string) {
		if path == "api4.thetvdb.com/v4/series/500/episodes/official/eng" {
			return 503, `{}`
		}
		return fullTVDB(path, q)
	}
	for _, c := range []struct {
		name   string
		answer func(string, map[string][]string) (int, string)
		root   string
		set    func(p *Provider)
		read   bool
	}{
		{"anidb: nothing missing", fullTVDB, "anidb-100", nil, false},
		{"anidb: pictures and summaries missing", testSeries, "anidb-100", nil, true},
		{"anidb: TheTVDB off", fullTVDB, "anidb-100", tvdbOff(t), true},
		{"anidb: TheTVDB's list failed", failed, "anidb-100", nil, true},
		{"anidb: plex first in EPISODE_TITLES", fullTVDB, "anidb-100", func(p *Provider) { p.cfg.EpisodeTitles = []string{"plex", "anidb", "tvdb"} }, true},
		{"anidb: plex first in EPISODE_FILL_SOURCES", fullTVDB, "anidb-100", func(p *Provider) { p.cfg.EpisodeFillSources = []string{"plex", "tvdb"} }, true},
		{"anidb: only plex for titles", fullTVDB, "anidb-100", func(p *Provider) { p.cfg.EpisodeTitles = []string{"plex"} }, true},
		{"anidb: plex not listed", testSeries, "anidb-100", func(p *Provider) {
			p.cfg.EpisodeTitles, p.cfg.EpisodeFillSources = []string{"anidb", "tvdb"}, []string{"tvdb"}
		}, false},
		{"tvdb: pictures and summaries missing are no gap", testSeries, "tvdb-500", nil, false},
		{"tvdb: a title missing", testSeries, "tvdb-500", func(p *Provider) { p.cfg.EpisodeTitles = []string{"tmdb", "plex", "tvdb"} }, true},
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

// Episode directors and writers: Plex's only where TheTVDB and TheMovieDb
// have none, each field on its own, once its list was read for the build.
func TestPlexEpisodeCrew(t *testing.T) {
	net := &fakeNet{answer: plexService(testSeries)}
	p := newTestProvider(t, net)
	withPlex(t, p, net)
	p.cfg.EpisodeTitles = []string{"plex", "anidb", "tvdb", "tmdb"} // Plex's list is read
	s, err := p.buildShow(context.Background(), "tvdb-500")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct {
		season, index     int
		directors, writer string
	}{
		{1, 1, "TVDB Director", "TVDB Writer"},
		{1, 2, "TVDB Director 2", "TMDB Writer"},
		{2, 3, "TMDB Director 3", "Plex Writer"}, // no TheTVDB record, no TheMovieDb writer
		{0, 1, "Plex Director", "Plex Writer"},
	} {
		ep := s.Episode(w.season, w.index)
		if d, wr := strings.Join(ep.Directors, ","), strings.Join(ep.Writers, ","); d != w.directors || wr != w.writer {
			t.Errorf("S%02dE%02d: directors %q writers %q, want %q %q", w.season, w.index, d, wr, w.directors, w.writer)
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
