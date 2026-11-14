package provider

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// tvdbCrewAsked: requests for TheTVDB's per-episode crew records.
func tvdbCrewAsked(net *fakeNet) int {
	net.mu.Lock()
	defer net.mu.Unlock()
	n := 0
	for path, c := range net.hits {
		if strings.HasPrefix(path, "api4.thetvdb.com/v4/episodes/") {
			n += c
		}
	}
	return n
}

// EPISODE_CREW_SOURCES is an order, per episode and field: with tmdb first,
// TheMovieDb's directors win where both sites have them, and each field
// goes to the first source that has it. The match report names each one's
// source. Both modes, Plex off.
func TestEpisodeCrewOrder(t *testing.T) {
	for _, c := range []struct {
		order []string
		wants map[string][]crewWant
	}{
		{[]string{"tvdb", "tmdb", "plex"}, map[string][]crewWant{
			"tvdb-500": {
				{1, 1, "TVDB Director", "TVDB Writer", "tvdb", "tvdb"},
				{1, 2, "TVDB Director 2", "TMDB Writer", "tvdb", "tmdb"},
				{2, 3, "TMDB Director 3", "", "tmdb", ""},
				{0, 1, "", "", "", ""},
			},
			"anidb-100": {
				{1, 1, "TVDB Director", "TVDB Writer", "tvdb", "tvdb"},
				{1, 2, "TVDB Director 2", "TMDB Writer", "tvdb", "tmdb"},
			},
		}},
		{[]string{"tmdb", "tvdb"}, map[string][]crewWant{
			"tvdb-500": {
				{1, 1, "TMDB Director", "TVDB Writer", "tmdb", "tvdb"},
				{1, 2, "TMDB Director 2", "TMDB Writer", "tmdb", "tmdb"},
				{2, 3, "TMDB Director 3", "", "tmdb", ""},
				{0, 1, "", "", "", ""},
			},
			"anidb-100": {
				{1, 1, "TMDB Director", "TVDB Writer", "tmdb", "tvdb"},
				{1, 2, "TMDB Director 2", "TMDB Writer", "tmdb", "tmdb"},
			},
		}},
	} {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			name := fmt.Sprintf("%v %s", c.order, root)
			p := newTestProvider(t, &fakeNet{answer: testSeries})
			p.cfg.EpisodeCrewSources = c.order
			s, err := p.buildShow(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			checkCrew(t, name, s, c.wants[root])
		}
	}
}

// Without tvdb in EPISODE_CREW_SOURCES (or with none), TheTVDB's crew
// records are never asked for, so nothing is pending and the build is
// complete. The episodes keep their TheTVDB episode, for their GUIDs and
// the special-only build. Both modes.
func TestEpisodeCrewWithoutTVDB(t *testing.T) {
	for _, c := range []struct {
		order []string
		wants map[string][]crewWant
	}{
		{nil, map[string][]crewWant{ // none
			"tvdb-500":  {{1, 1, "", "", "", ""}, {1, 2, "", "", "", ""}, {2, 3, "", "", "", ""}},
			"anidb-100": {{1, 1, "", "", "", ""}, {1, 2, "", "", "", ""}},
		}},
		{[]string{"tmdb", "plex"}, map[string][]crewWant{
			"tvdb-500": {
				{1, 1, "TMDB Director", "", "tmdb", ""},
				{1, 2, "TMDB Director 2", "TMDB Writer", "tmdb", "tmdb"},
				{2, 3, "TMDB Director 3", "", "tmdb", ""},
			},
			"anidb-100": {
				{1, 1, "TMDB Director", "", "tmdb", ""},
				{1, 2, "TMDB Director 2", "TMDB Writer", "tmdb", "tmdb"},
			},
		}},
	} {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			name := fmt.Sprintf("%v %s", c.order, root)
			net := &fakeNet{answer: testSeries}
			p := newTestProvider(t, net)
			p.cfg.EpisodeCrewSources = c.order
			// Every record would go to the background, pending, if one were asked.
			restore := sources.SetCrewBudgetForTest(0)
			s, err := p.buildShow(context.Background(), root)
			restore()
			if err != nil {
				t.Fatal(err)
			}
			p.tvdb.WaitIdle() // a record asked in the background would be counted too
			if n := tvdbCrewAsked(net); n != 0 || !s.Complete {
				t.Errorf("%s: %d TheTVDB crew records asked, complete %v; want none, complete", name, n, s.Complete)
			}
			checkCrew(t, name, s, c.wants[root])
			if ep := s.Episode(1, 2); ep == nil || ep.tvdbEp != 12 {
				t.Errorf("%s: S01E02 %+v, want paired with TheTVDB episode 12", name, ep)
			}
		}
	}
}

// legacyCrew: TheTVDB with the legacy key, whose v3 episode list carries
// the crew; AniDB and TheMovieDb as in testSeries.
func legacyCrew(path string, q map[string][]string) (int, string) {
	switch path {
	case "api.thetvdb.com/login":
		return 200, `{"token":"t3"}`
	case "api.thetvdb.com/series/500":
		return 200, `{"data":{"seriesName":"Series","runtime":"24"}}`
	case "api.thetvdb.com/series/500/episodes":
		return 200, `{"data":[
			{"id":11,"airedSeason":1,"airedEpisodeNumber":1,"episodeName":"TVDB One","directors":["V3 Director"],"writers":["V3 Writer"]},
			{"id":12,"airedSeason":1,"airedEpisodeNumber":2,"episodeName":"TVDB Two","directors":["V3 Director 2"]}],"links":{"next":null}}`
	}
	return testSeries(path, q)
}

// With the legacy key, the crew comes with TheTVDB's episode list: it takes
// tvdb's place in the order, and no episode gets it when tvdb isn't listed.
// Both modes.
func TestEpisodeCrewLegacyList(t *testing.T) {
	for _, c := range []struct {
		order []string
		wants []crewWant
	}{
		{[]string{"tvdb", "tmdb"}, []crewWant{
			{1, 1, "V3 Director", "V3 Writer", "tvdb", "tvdb"},
			{1, 2, "V3 Director 2", "TMDB Writer", "tvdb", "tmdb"},
		}},
		{[]string{"tmdb", "tvdb"}, []crewWant{
			{1, 1, "TMDB Director", "V3 Writer", "tmdb", "tvdb"},
			{1, 2, "TMDB Director 2", "TMDB Writer", "tmdb", "tmdb"},
		}},
		{[]string{"tmdb"}, []crewWant{
			{1, 1, "TMDB Director", "", "tmdb", ""},
			{1, 2, "TMDB Director 2", "TMDB Writer", "tmdb", "tmdb"},
		}},
		{nil, []crewWant{{1, 1, "", "", "", ""}, {1, 2, "", "", "", ""}}},
	} {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			name := fmt.Sprintf("%v %s", c.order, root)
			net := &fakeNet{answer: legacyCrew}
			p := newTestProvider(t, net)
			f := fetch.New(t.TempDir())
			f.SetTransport(net)
			f.ThrottleAll(0)
			p.tvdb = sources.NewTVDB(f, "", "", "legacy")
			p.cfg.EpisodeCrewSources = c.order
			s, err := p.buildShow(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			checkCrew(t, name, s, c.wants)
			if n := tvdbCrewAsked(net); n != 0 {
				t.Errorf("%s: %d v4 crew records asked with the legacy key", name, n)
			}
		}
	}
}

// unlist takes src out of every source setting, as for a user who lists it
// nowhere.
func unlist(t *testing.T, p *Provider, src string) {
	t.Helper()
	c := &p.cfg
	for _, l := range []*[]string{&c.TitleSources, &c.SummarySources, &c.PosterSources, &c.BackgroundSources,
		&c.LogoSources, &c.EpisodeFillSources, &c.EpisodeCrewSources, &c.EpisodeTitles, &c.GenreSources,
		&c.CastSources, &c.StudioSources, &c.CrewSources, &c.RatingSources, &c.DateSources} {
		*l = slices.DeleteFunc(slices.Clone(*l), func(s string) bool { return s == src })
	}
	if c.Listed(src) {
		t.Fatalf("unlist: %s is still listed: a source setting is missing here", src)
	}
}

// asked: requests whose host and path start with prefix.
func asked(net *fakeNet, prefix string) int {
	net.mu.Lock()
	defer net.mu.Unlock()
	n := 0
	for path, c := range net.hits {
		if strings.HasPrefix(path, prefix) {
			n += c
		}
	}
	return n
}

const (
	tmdbSeason1  = "api.themoviedb.org/3/tv/77/season/1"
	tvdbEpisodes = "api4.thetvdb.com/v4/series/500/episodes/official/eng"
)

// TheMovieDb listed in no setting, its key set: it gets no request at all,
// not the lookup by TheTVDB id (/tvdb, a mapping without a TheMovieDb id),
// not the show, not a season. The show is complete, and its tmdb:// GUID
// comes from the mapping alone. Both modes.
func TestTMDBNotListedNotAsked(t *testing.T) {
	for _, root := range []string{"tvdb-500", "anidb-100"} {
		net := &fakeNet{answer: testSeries}
		p := newTestProvider(t, net)
		if root == "tvdb-500" { // no TheMovieDb id in the mapping: it would be looked up by TheTVDB's
			p.lists.SetForTest(map[int]mapping.Entry{100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}}})
		}
		unlist(t, p, "tmdb")
		s, err := p.buildShow(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		p.tvdb.WaitIdle()
		if n := asked(net, "api.themoviedb.org/"); n != 0 || !s.Complete {
			t.Errorf("%s: %d requests to TheMovieDb, complete %v; want none, complete", root, n, s.Complete)
		}
		if want := root == "anidb-100"; slices.Contains(s.Guids, "tmdb://77") != want {
			t.Errorf("%s: guids %v, tmdb://77 want %v (the mapping's only)", root, s.Guids, want)
		}
	}
}

// TheMovieDb's episode list is fetched only for what it gives: titles,
// crew, and in /tvdb the season posters that come with it. Listed only in
// POSTER_SOURCES, /tvdb fetches it and /anidb doesn't; listed only in
// EPISODE_CREW_SOURCES, both fetch it and take its crew, not its titles.
func TestTMDBEpisodeListWhenListed(t *testing.T) {
	for _, c := range []struct {
		name    string
		set     func(p *Provider)
		fetched map[string]bool // by root
		crew    bool
	}{
		{"POSTER_SOURCES only", func(p *Provider) { p.cfg.PosterSources = []string{"tvdb", "tmdb"} },
			map[string]bool{"tvdb-500": true, "anidb-100": false}, false},
		{"EPISODE_CREW_SOURCES only", func(p *Provider) { p.cfg.EpisodeCrewSources = []string{"tvdb", "tmdb"} },
			map[string]bool{"tvdb-500": true, "anidb-100": true}, true},
	} {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			name := c.name + ", " + root
			net := &fakeNet{answer: testSeries}
			p := newTestProvider(t, net)
			unlist(t, p, "tmdb")
			c.set(p)
			s, err := p.buildShow(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if got := net.count(tmdbSeason1) > 0; got != c.fetched[root] || !s.Complete {
				t.Errorf("%s: TheMovieDb's list fetched %v, complete %v; want fetched %v, complete", name, got, s.Complete, c.fetched[root])
			}
			ep := s.Episode(1, 2)
			if ep == nil || ep.titleSrc == "tmdb" || ep.Title == "TMDB Two" {
				t.Fatalf("%s: S01E02 %+v, want no TheMovieDb title", name, ep)
			}
			if got := ep.src.writers == srcTMDB; got != c.crew {
				t.Errorf("%s: S01E02 writers %v from %q, TheMovieDb's want %v", name, ep.Writers, ep.src.writers.name(), c.crew)
			}
		}
	}
}

// /tvdb pairs TheMovieDb's list through AniDB's entries, which asks AniDB
// for the series' other entries: only when that list is fetched or AniDB's
// episode titles are listed. With neither, entry 200 is not asked for.
func TestTVDBPairingOnlyForTMDBList(t *testing.T) {
	for _, c := range []struct {
		name   string
		set    func(p *Provider)
		second bool // AniDB 200 asked
	}{
		{"default lists", nil, true},
		{"neither", func(p *Provider) {
			unlist(t, p, "tmdb")
			p.cfg.EpisodeTitles = []string{"tvdb"}
		}, false},
	} {
		var mu sync.Mutex
		second := 0
		net := &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
			if path == "api.anidb.net/httpapi" && len(q["aid"]) > 0 && q["aid"][0] == "200" {
				mu.Lock()
				second++
				mu.Unlock()
				return 200, secondXML
			}
			return testSeries(path, q)
		}}
		p := newTestProvider(t, net)
		p.lists.SetForTest(map[int]mapping.Entry{
			100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}, TMDB: mapping.Placement{ID: 77}},
			200: {AID: 200, TVDB: mapping.Placement{ID: 500, Season: 2, Mapped: true}, TMDB: mapping.Placement{ID: 77}},
		})
		if c.set != nil {
			c.set(p)
		}
		if _, err := p.buildShow(context.Background(), "tvdb-500"); err != nil {
			t.Fatal(err)
		}
		p.anidb.WaitIdle()
		mu.Lock()
		got := second > 0
		mu.Unlock()
		if got != c.second {
			t.Errorf("%s: AniDB 200 asked %v, want %v", c.name, got, c.second)
		}
	}
}

// /anidb fetches TheTVDB's episode list only when an episode setting lists
// tvdb, or for a special-only entry, whose episodes are TheTVDB's specials.
func TestAniDBTVDBListWhenListed(t *testing.T) {
	regular := mapping.Entry{AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}, TMDB: mapping.Placement{ID: 77}}
	special := mapping.Entry{AID: 100, TVDB: mapping.Placement{ID: 500, Season: 0, Mapped: true}, TMDB: mapping.Placement{ID: 77}}
	for _, c := range []struct {
		name    string
		entry   mapping.Entry
		listed  bool
		fetched bool
	}{
		{"listed", regular, true, true},
		{"not listed", regular, false, false},
		{"not listed, special-only", special, false, true},
	} {
		net := &fakeNet{answer: testSeries}
		p := newTestProvider(t, net)
		p.lists.SetForTest(map[int]mapping.Entry{100: c.entry})
		if !c.listed {
			p.cfg.EpisodeTitles, p.cfg.EpisodeFillSources, p.cfg.EpisodeCrewSources = []string{"anidb", "tmdb"}, nil, []string{"tmdb"}
		}
		s, err := p.buildShow(context.Background(), "anidb-100")
		if err != nil {
			t.Fatal(err)
		}
		if got := net.count(tvdbEpisodes) > 0; got != c.fetched || !s.Complete {
			t.Errorf("%s: TheTVDB's episode list fetched %v, complete %v; want fetched %v, complete", c.name, got, s.Complete, c.fetched)
		}
	}
}

// TheTVDB's crew records are asked only for the episodes the sources
// listed before tvdb left a gap in: with tmdb,tvdb and TheMovieDb giving
// season 1 both fields, only season 2's and the special's. With tvdb first
// (the default), every paired episode's, as before. Both modes.
func TestTVDBCrewRecordsOnlyForGaps(t *testing.T) {
	for _, c := range []struct {
		order []string
		wants map[string][]int // records asked, by root
	}{
		{[]string{"tvdb", "tmdb", "plex"}, map[string][]int{"tvdb-500": {11, 12, 13, 14}, "anidb-100": {11, 12}}},
		{[]string{"tmdb", "tvdb"}, map[string][]int{"tvdb-500": {13, 14}, "anidb-100": nil}},
	} {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			name := fmt.Sprintf("%v %s", c.order, root)
			net := &fakeNet{answer: fullCrew}
			p := newTestProvider(t, net)
			p.cfg.EpisodeCrewSources = c.order
			s, err := p.buildShow(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			p.tvdb.WaitIdle() // a record asked in the background would be counted too
			var got []int
			for id := 11; id <= 14; id++ {
				if net.count(fmt.Sprintf("api4.thetvdb.com/v4/episodes/%d/extended", id)) > 0 {
					got = append(got, id)
				}
			}
			if !slices.Equal(got, c.wants[root]) || !s.Complete {
				t.Errorf("%s: records asked %v, complete %v; want %v, complete", name, got, s.Complete, c.wants[root])
			}
		}
	}
}

// A show value fetched ahead (warm) that failed counts only once fill or
// fillSeasons read it: one no field reached leaves nothing missing, so the
// show isn't rebuilt for it and no failure is logged or reported.
func TestWarmedFailureCountsOnlyWhenRead(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	ss := &showSources{p: p}
	ss.tmdbText.from(77, true, func() (text, error) { return text{}, errors.New("TheMovieDb is down") })
	ss.tmdbText.warm()
	ss.unread() // what warm read doesn't count
	if f := ss.failed(); len(f) != 0 {
		t.Errorf("fetched ahead, not read: failed %v, want none", f)
	}
	ss.tmdbText.get() // fill reaches it
	if f := ss.failed(); !slices.Equal(f, []string{"tmdb text"}) {
		t.Errorf("read: failed %v, want [tmdb text]", f)
	}
}

// Plex's pictures with ART_ALL_SOURCES are made once, from its show and
// its /images: when warm made them, fill's read still counts both as read.
func TestPlexAllArtCountsWhenRead(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	p.cfg.ArtAllSources = true
	ss := &showSources{p: p}
	ss.plexShow.from(1, true, func() (sources.PlexShow, error) { return sources.PlexShow{}, nil })
	ss.plexImages.from(1, true, func() (sources.PlexImages, error) { return sources.PlexImages{}, errors.New("Plex is down") })
	ss.plexArt() // warm
	ss.unread()
	if f := ss.failed(); len(f) != 0 {
		t.Errorf("fetched ahead, not read: failed %v, want none", f)
	}
	ss.plexArt() // fill
	if f := ss.failed(); !slices.Equal(f, []string{"plex images"}) {
		t.Errorf("read: failed %v, want [plex images]", f)
	}
}

// defaultLists clears the settings these tests read, so they run with the
// default source lists whatever the machine's environment holds. Call it
// before newTestProvider.
func defaultLists(t *testing.T) {
	for _, name := range []string{"TITLE_SOURCES", "SUMMARY_SOURCES", "POSTER_SOURCES", "BACKGROUND_SOURCES",
		"LOGO_SOURCES", "EPISODE_FILL_SOURCES", "EPISODE_CREW_SOURCES", "EPISODE_TITLES", "GENRE_SOURCES",
		"CAST_SOURCES", "STUDIO_SOURCES", "CREW_SOURCES", "RATING_SOURCES", "DATE_SOURCES", "RATING_COUNTRY",
		"ART_ALL_SOURCES", "SEASON_ART_FALLBACK", "PLEX_TOKEN", "MAL_CLIENT_ID"} {
		t.Setenv(name, "") // blank: the default
	}
}

// tvdbSeriesRecord is TheTVDB's series record with every detail the
// default lists ask it for; withStudio false leaves out the studio.
func tvdbSeriesRecord(withStudio bool) string {
	studio := ""
	if withStudio {
		studio = `"companies":[{"name":"TVDB Studio","primaryCompanyType":2}],`
	}
	return `{"data":{"firstAired":"2009-04-01","genres":[{"name":"TVDB Genre"}],` + studio +
		`"characters":[{"name":"Hero","personName":"TVDB Actor","peopleType":"Actor","sort":1},
			{"personName":"TVDB Show Director","peopleType":"Director","sort":2}],
		"contentRatings":[{"name":"TV-14","country":"usa"}]}}`
}

// siteShow: TheTVDB has every show field the default lists ask it for (its
// series record, posters, backgrounds and logos, its English text), AniDB
// the title and summary, over fullCrew's episodes (nothing left for Plex's
// episode list). TheMovieDb's show details (its title and summary come with
// them) are answered by tmdbDetails; the second value counts those requests.
func siteShow(withStudio bool, tmdbDetails func() (int, string)) (func(string, map[string][]string) (int, string), func() int) {
	var mu sync.Mutex
	asked := 0
	answer := func(path string, q map[string][]string) (int, string) {
		switch path {
		case "api4.thetvdb.com/v4/series/500/extended":
			return 200, tvdbSeriesRecord(withStudio)
		case "api4.thetvdb.com/v4/series/500/artworks":
			return 200, `{"data":{"artworks":[{"type":2,"image":"/poster.jpg"},{"type":3,"image":"/bg.jpg"},{"type":23,"image":"/logo.png"}]}}`
		case "api4.thetvdb.com/v4/series/500/translations/eng":
			return 200, `{"data":{"name":"TVDB Name","overview":"From TheTVDB."}}`
		case "api.themoviedb.org/3/tv/77":
			if strings.Contains(strings.Join(q["append_to_response"], ","), "aggregate_credits") { // not the episode list's
				mu.Lock()
				asked++
				mu.Unlock()
				return tmdbDetails()
			}
		}
		return fullCrew(path, q)
	}
	return answer, func() int {
		mu.Lock()
		defer mu.Unlock()
		return asked
	}
}

// With the default lists, TheTVDB's and TheMovieDb's show values are
// fetched even where a source before them has the field: TheTVDB's text
// (AniDB has the title and summary), TheMovieDb's details and pictures
// (TheTVDB has every detail and every kind of picture). Plex, MyAnimeList
// and Anime News Network, set up and listed after those, are not asked.
// Both modes.
func TestSiteValuesFetchedAhead(t *testing.T) {
	defaultLists(t)
	for _, root := range []string{"tvdb-500", "anidb-100"} {
		answer, tmdbDetails := siteShow(true, func() (int, string) {
			return 200, `{"name":"TMDB Name","overview":"From TheMovieDb.","genres":[{"name":"TMDB Genre"}]}`
		})
		net := &fakeNet{answer: plexService(answer)}
		p := newTestProvider(t, net)
		withPlex(t, p, net)
		f := fetch.New(t.TempDir())
		f.SetTransport(net)
		p.mal = sources.NewMAL(f, "client")
		f.ThrottleAll(0)
		p.lists.SetForTest(map[int]mapping.Entry{100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true},
			TMDB: mapping.Placement{ID: 77}, MAL: 5, ANN: 7}})
		s, err := p.buildShow(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		// The premise: the sources before them had every field.
		if s.Title != "Kizuna Test" || s.Summary != "From AniDB." || !slices.Equal(s.Genres, []string{"TVDB Genre"}) ||
			s.Studio != "TVDB Studio" || s.ContentRating != "TV-14" || !s.Complete ||
			len(s.Art.Logos) == 0 || !strings.HasSuffix(s.Art.Logos[0], "/logo.png") ||
			len(s.Art.Posters) == 0 || !strings.HasSuffix(s.Art.Posters[0], "/poster.jpg") {
			t.Errorf("%s: title %q summary %q genres %v studio %q rating %q logos %v posters %v complete %v; want AniDB's and TheTVDB's",
				root, s.Title, s.Summary, s.Genres, s.Studio, s.ContentRating, s.Art.Logos, s.Art.Posters, s.Complete)
		}
		if net.count("api4.thetvdb.com/v4/series/500/translations/eng") == 0 {
			t.Errorf("%s: TheTVDB's text not fetched", root)
		}
		if tmdbDetails() == 0 {
			t.Errorf("%s: TheMovieDb's details not fetched", root)
		}
		if net.count("api.themoviedb.org/3/tv/77/images") == 0 {
			t.Errorf("%s: TheMovieDb's pictures not fetched", root)
		}
		for _, host := range []string{plexHost, "api.myanimelist.net/", "cdn.animenewsnetwork.com/"} {
			if n := asked(net, host); n != 0 {
				t.Errorf("%s: %d requests to %s, want none", root, n, host)
			}
		}
	}
}

// A TheMovieDb value fetched ahead that fails: when no field needed it
// (AniDB and TheTVDB had them all), the show is complete, and neither the
// log nor the match report says a source failed. When a field needed it
// (TheTVDB without a studio), the show is incomplete and both say so, as
// before. Both modes.
func TestWarmedTMDBFailureOnlyWhenNeeded(t *testing.T) {
	defaultLists(t)
	for _, needed := range []bool{false, true} {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			name := fmt.Sprintf("%s, needed %v", root, needed)
			answer, tmdbDetails := siteShow(!needed, func() (int, string) { return 503, `{}` })
			p := newTestProvider(t, &fakeNet{answer: answer})
			store, dir, serve := reportsOn(t, p)
			var logs strings.Builder
			restore := captureLogs(&logs)
			mode, _, _ := strings.Cut(root, "-")
			serve("GET", "/"+mode+"/library/metadata/"+root, "")
			restore()
			store.Flush()
			if tmdbDetails() == 0 {
				t.Fatalf("%s: TheMovieDb's details not asked: nothing failed", name)
			}
			var doc report.ShowDoc
			readJSON(t, filepath.Join(dir, "data", "anidb-100.json"), &doc) // the TVDB series' page is its AniDB entry's
			if len(doc.Builds) != 1 {
				t.Fatalf("%s: %d builds, want 1", name, len(doc.Builds))
			}
			b := doc.Builds[0]
			if b.Complete == needed {
				t.Errorf("%s: complete %v, want %v", name, b.Complete, !needed)
			}
			reported := slices.ContainsFunc(b.Checks, func(c report.Check) bool { return strings.HasPrefix(c.Title, "A source failed") })
			noted := slices.ContainsFunc(b.Trace, func(s report.Span) bool { return strings.HasPrefix(s.Note, "failed: ") })
			logged := strings.Contains(logs.String(), "show sources failed")
			if reported != needed || noted != needed || logged != needed {
				t.Errorf("%s: failure reported %v, noted on the fields step %v, logged %v; want %v\nchecks %+v\n%s",
					name, reported, noted, logged, needed, b.Checks, logs.String())
			}
			if needed && !slices.ContainsFunc(b.Checks, func(c report.Check) bool { return c.Title == "A source failed: tmdb details" }) {
				t.Errorf("%s: checks %+v, want TheMovieDb's details named", name, b.Checks)
			}
		}
	}
}
