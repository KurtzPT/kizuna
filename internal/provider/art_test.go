package provider

import (
	"context"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"kizuna/internal/config"
	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// seasonArt: TVDB 500 lists aired-order seasons 0-4, each with its own
// art; season 2 has no background, season 4 nothing. TMDB 77 has season
// posters for its seasons 1 and 2, none for 3, and no season 0. Images are
// named for where they come from: "tvdb-s3-poster", "tmdb-series-logo".
func seasonArt(path string, q map[string][]string) (int, string) {
	tvdbArt := func(kind int, name string) string {
		return `{"type":` + strconv.Itoa(kind) + `,"image":"/` + name + `","score":1}`
	}
	switch path {
	case "api4.thetvdb.com/v4/series/500/extended":
		return 200, `{"data":{"seasons":[
			{"id":600,"number":0,"type":{"type":"official"}},{"id":601,"number":1,"type":{"type":"official"}},
			{"id":602,"number":2,"type":{"type":"official"}},{"id":603,"number":3,"type":{"type":"official"}},
			{"id":604,"number":4,"type":{"type":"official"}},{"id":703,"number":3,"type":{"type":"dvd"}}]}}`
	case "api4.thetvdb.com/v4/series/500/artworks":
		return 200, `{"data":{"artworks":[` + tvdbArt(2, "tvdb-series-poster") + `,` + tvdbArt(3, "tvdb-series-background") + `,` +
			tvdbArt(23, "tvdb-series-logo") + `]}}`
	case "api4.thetvdb.com/v4/seasons/600/extended":
		return 200, `{"data":{"artwork":[` + tvdbArt(7, "tvdb-s0-poster") + `]}}`
	case "api4.thetvdb.com/v4/seasons/601/extended":
		return 200, `{"data":{"artwork":[` + tvdbArt(7, "tvdb-s1-poster") + `,` + tvdbArt(8, "tvdb-s1-background") + `]}}`
	case "api4.thetvdb.com/v4/seasons/602/extended":
		return 200, `{"data":{"artwork":[` + tvdbArt(7, "tvdb-s2-poster") + `,` + tvdbArt(6, "tvdb-s2-banner") + `]}}`
	case "api4.thetvdb.com/v4/seasons/603/extended":
		return 200, `{"data":{"artwork":[` + tvdbArt(7, "tvdb-s3-poster") + `,` + tvdbArt(8, "tvdb-s3-background") + `]}}`
	case "api4.thetvdb.com/v4/seasons/604/extended":
		return 200, `{"data":{"artwork":[]}}`
	case "api.themoviedb.org/3/tv/77":
		return 200, `{"name":"TMDB Name","seasons":[{"season_number":1},{"season_number":2},{"season_number":3}]}`
	case "api.themoviedb.org/3/tv/77/images":
		return 200, `{"posters":[{"file_path":"/tmdb-series-poster"}],"backdrops":[{"file_path":"/tmdb-series-background"}],
			"logos":[{"file_path":"/tmdb-series-logo"}]}`
	case "api.themoviedb.org/3/tv/77/season/1":
		return 200, `{"episodes":[],"images":{"posters":[{"file_path":"/tmdb-s1-poster"}]}}`
	case "api.themoviedb.org/3/tv/77/season/2":
		return 200, `{"episodes":[],"images":{"posters":[{"file_path":"/tmdb-s2-poster"}]}}`
	case "api.themoviedb.org/3/tv/77/season/3":
		return 200, `{"episodes":[],"images":{"posters":[]}}`
	}
	return testSeries(path, q)
}

// names strips image URLs down to their names, for short comparisons.
func names(urls []string) string {
	out := make([]string, len(urls))
	for i, u := range urls {
		out[i] = u[strings.LastIndex(u, "/")+1:]
	}
	return strings.Join(out, " ")
}

// artOf builds an /anidb show's art (show and Plex seasons 1 and 0) for an
// entry on TVDB 500 and TMDB 77, with set changing the settings.
func artOf(t *testing.T, entry mapping.Entry, series, movie bool, set func(*config.Config)) (*Show, *fakeNet) {
	t.Helper()
	net := &fakeNet{answer: seasonArt}
	p := newTestProvider(t, net)
	p.cfg.TitleSources, p.cfg.SummarySources = nil, nil // only the art is asked for
	if set != nil {
		set(&p.cfg)
	}
	a := &sources.AniDBAnime{Picture: "https://cdn.anidb.net/images/main/anidb-poster"}
	ss := p.newSources(context.Background(), 100, a, entry, entry.TVDB.ID, entry.TMDB.ID, movie, firstEntry{series, series || movie})
	s := &Show{Episodes: map[int][]Ep{1: nil, 0: nil}}
	ss.anidbTargets(entry, plexSeasons(s))
	ss.fill(s)
	ss.fillSeasons(s)
	return s, net
}

func onTVDB(season int) mapping.Placement {
	return mapping.Placement{ID: 500, Season: season, Mapped: true}
}
func onTMDB(season int) mapping.Placement {
	return mapping.Placement{ID: 77, Season: season, Mapped: true}
}

// In /anidb the show's and its Season 1's art are of the season the
// mapping puts the entry in; the entry that starts its series gets the
// series' art first, then season 1's; Specials get season 0's. A season
// without art of a kind falls back to its site's series art.
func TestAniDBSeasonArt(t *testing.T) {
	cases := []struct {
		name                       string
		entry                      mapping.Entry
		series                     bool
		posters, backgrounds, logo string // the show's
		s1Posters, s1Backgrounds   string // its Season 1's
	}{
		{"first entry", mapping.Entry{TVDB: onTVDB(1), TMDB: onTMDB(1)}, true,
			"tvdb-series-poster tvdb-s1-poster", "tvdb-series-background tvdb-s1-background", "tvdb-series-logo",
			"tvdb-s1-poster", "tvdb-s1-background"},
		{"later entry", mapping.Entry{TVDB: onTVDB(3), TMDB: onTMDB(2)}, false,
			"tvdb-s3-poster", "tvdb-s3-background", "tvdb-series-logo", "tvdb-s3-poster", "tvdb-s3-background"},
		{"season without a background", mapping.Entry{TVDB: onTVDB(2)}, false,
			"tvdb-s2-poster", "tvdb-series-background", "tvdb-series-logo", "tvdb-s2-poster", "tvdb-series-background"},
		{"season without art", mapping.Entry{TVDB: onTVDB(4)}, false,
			"tvdb-series-poster", "tvdb-series-background", "tvdb-series-logo", "tvdb-series-poster", "tvdb-series-background"},
		{"special-only", mapping.Entry{TVDB: onTVDB(0)}, false,
			"tvdb-s0-poster", "tvdb-series-background", "tvdb-series-logo", "tvdb-s0-poster", "tvdb-series-background"},
		{"absolute", mapping.Entry{TVDB: onTVDB(-1), TMDB: onTMDB(2)}, false,
			"tvdb-series-poster", "tvdb-series-background", "tvdb-series-logo", "tvdb-series-poster", "tvdb-series-background"},
		{"not mapped", mapping.Entry{TVDB: mapping.Placement{ID: 500, Season: 3}}, false,
			"tvdb-series-poster", "tvdb-series-background", "tvdb-series-logo", "tvdb-series-poster", "tvdb-series-background"},
	}
	for _, c := range cases {
		s, _ := artOf(t, c.entry, c.series, false, nil)
		if got := names(s.Art.Posters); got != c.posters {
			t.Errorf("%s: posters %q, want %q", c.name, got, c.posters)
		}
		if got := names(s.Art.Backgrounds); got != c.backgrounds {
			t.Errorf("%s: backgrounds %q, want %q", c.name, got, c.backgrounds)
		}
		if got := names(s.Art.Logos); got != c.logo {
			t.Errorf("%s: logos %q, want %q", c.name, got, c.logo)
		}
		s1 := s.SeasonArt[1]
		if names(s1.Posters) != c.s1Posters || names(s1.Backgrounds) != c.s1Backgrounds {
			t.Errorf("%s: Season 1 posters %q backgrounds %q, want %q %q", c.name, names(s1.Posters), names(s1.Backgrounds),
				c.s1Posters, c.s1Backgrounds)
		}
		if sp := s.SeasonArt[0]; names(sp.Posters) != "tvdb-s0-poster" || names(sp.Logos) != "tvdb-series-logo" {
			t.Errorf("%s: Specials posters %q logos %q, want TheTVDB's season 0 poster and the series' logo", c.name,
				names(sp.Posters), names(sp.Logos))
		}
	}
}

// SEASON_ART_FALLBACK=false: a site's season without art of a kind gives
// nothing from that site, so the next source in the order is asked. A
// source without season art of the kind (AniDB, TheMovieDb backgrounds and
// logos), or a target without a season, still gives its series art.
func TestSeasonArtNoFallback(t *testing.T) {
	off := func(c *config.Config) { c.SeasonArtFallback = false }
	s, _ := artOf(t, mapping.Entry{TVDB: onTVDB(4), TMDB: onTMDB(2)}, false, false, off)
	if got := names(s.Art.Posters); got != "tmdb-s2-poster" {
		t.Errorf("TVDB season without art: posters %q, want TMDB season 2's", got)
	}
	if got := names(s.Art.Backgrounds); got != "tmdb-series-background" {
		t.Errorf("backgrounds %q, want TMDB's series background (it has no season backgrounds)", got)
	}
	if got := names(s.Art.Logos); got != "tvdb-series-logo" {
		t.Errorf("logos %q, want TVDB's series logo (no site has season logos)", got)
	}

	s, _ = artOf(t, mapping.Entry{TVDB: onTVDB(4), TMDB: onTMDB(3)}, false, false, off)
	if got := names(s.Art.Posters); got != "anidb-poster" {
		t.Errorf("no season posters anywhere: %q, want AniDB's own", got)
	}

	s, _ = artOf(t, mapping.Entry{TVDB: onTVDB(-1)}, false, false, off)
	if got := names(s.Art.Posters); got != "tvdb-series-poster" {
		t.Errorf("absolute: %q, want the series poster (no season to fall back from)", got)
	}

	s, _ = artOf(t, mapping.Entry{TVDB: onTVDB(2)}, false, false, func(c *config.Config) {
		off(c)
		c.PosterSources = []string{"anidb", "tvdb"}
	})
	if got := names(s.Art.Posters); got != "anidb-poster" {
		t.Errorf("AniDB first: %q, want its own poster", got)
	}
	if got := names(s.Art.Backgrounds); got != "" { // no TMDB id: nothing left after TheTVDB's season 2
		t.Errorf("season 2 without a background: %q, want none", got)
	}
	if got := names(s.SeasonArt[1].Posters); got != "anidb-poster" {
		t.Errorf("AniDB first, Season 1: %q, want AniDB's own poster", got)
	}
}

// TheMovieDb's backgrounds and logos are always its series' (its seasons
// have posters only). A season TheMovieDb doesn't list costs no request.
func TestTMDBSeasonArtKinds(t *testing.T) {
	tmdbOnly := func(c *config.Config) {
		c.SeasonArtFallback = false
		c.PosterSources, c.BackgroundSources, c.LogoSources = []string{"tmdb"}, []string{"tmdb"}, []string{"tmdb"}
	}
	s, _ := artOf(t, mapping.Entry{TMDB: onTMDB(2)}, false, false, tmdbOnly)
	if names(s.Art.Posters) != "tmdb-s2-poster" || names(s.Art.Backgrounds) != "tmdb-series-background" ||
		names(s.Art.Logos) != "tmdb-series-logo" {
		t.Errorf("TMDB season 2: posters %q backgrounds %q logos %q", names(s.Art.Posters), names(s.Art.Backgrounds), names(s.Art.Logos))
	}
	if got := names(s.SeasonArt[0].Posters); got != "" {
		t.Errorf("Specials: %q, want none (TMDB lists no season 0, and fallback is off)", got)
	}
	s, net := artOf(t, mapping.Entry{TMDB: onTMDB(2)}, false, false, nil)
	if got := names(s.SeasonArt[0].Posters); got != "tmdb-series-poster" {
		t.Errorf("Specials with fallback: %q, want the series poster", got)
	}
	if c := net.count("api.themoviedb.org/3/tv/77/season/0"); c != 0 {
		t.Errorf("season 0, which TMDB doesn't list, asked %d times", c)
	}
}

// ART_ALL_SOURCES=true lists, per source, the season's art and then (with
// SEASON_ART_FALLBACK) the series', before the next source's.
func TestSeasonArtAllSources(t *testing.T) {
	entry := mapping.Entry{TVDB: onTVDB(3), TMDB: onTMDB(2)}
	all := func(fallback bool) func(*config.Config) {
		return func(c *config.Config) { c.ArtAllSources, c.SeasonArtFallback = true, fallback }
	}
	s, _ := artOf(t, entry, false, false, all(true))
	if got, want := names(s.Art.Posters), "tvdb-s3-poster tvdb-series-poster tmdb-s2-poster tmdb-series-poster anidb-poster"; got != want {
		t.Errorf("posters %q, want %q", got, want)
	}
	if got, want := names(s.Art.Backgrounds), "tvdb-s3-background tvdb-series-background tmdb-series-background"; got != want {
		t.Errorf("backgrounds %q, want %q", got, want)
	}
	s, _ = artOf(t, entry, false, false, all(false))
	if got, want := names(s.Art.Posters), "tvdb-s3-poster tmdb-s2-poster anidb-poster"; got != want {
		t.Errorf("without fallback: posters %q, want %q", got, want)
	}
	s, _ = artOf(t, mapping.Entry{TVDB: onTVDB(1), TMDB: onTMDB(1)}, true, false, all(false))
	if got, want := names(s.Art.Posters), "tvdb-series-poster tvdb-s1-poster tmdb-series-poster tmdb-s1-poster anidb-poster"; got != want {
		t.Errorf("first entry: posters %q, want %q", got, want)
	}
}

// A season's art is fetched only when it is asked for: the first entry's
// season 1 is fetched once for the show and its Season 1, and a later
// entry's show never asks for another season.
func TestSeasonArtFetchedOnce(t *testing.T) {
	_, net := artOf(t, mapping.Entry{TVDB: onTVDB(1), TMDB: onTMDB(1)}, true, false, nil)
	if c := net.count("api4.thetvdb.com/v4/seasons/601/extended"); c != 1 {
		t.Errorf("season 1 asked %d times, want 1", c)
	}
	_, net = artOf(t, mapping.Entry{TVDB: onTVDB(3)}, false, false, nil)
	for _, id := range []string{"601", "602", "604"} {
		if c := net.count("api4.thetvdb.com/v4/seasons/" + id + "/extended"); c != 0 {
			t.Errorf("season record %s asked %d times, want none", id, c)
		}
	}
	if c := net.count("api4.thetvdb.com/v4/series/500/extended"); c != 1 {
		t.Errorf("series record asked %d times, want 1 (the season ids)", c)
	}
}

// A source first asked for a Plex season's art (here TheMovieDb's series
// art, the Specials' last fallback) that fails leaves the show incomplete,
// as one fill asked does: it is rebuilt in 2 minutes, not kept an hour.
func TestSeasonOnlySourceFailureIsIncomplete(t *testing.T) {
	for _, code := range []int{500, 200} {
		net := &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
			switch path {
			case "api4.thetvdb.com/v4/seasons/600/extended": // Specials: no poster on TheTVDB
				return 200, `{"data":{"artwork":[]}}`
			case "api4.thetvdb.com/v4/series/500/artworks":
				return 200, `{"data":{"artworks":[{"type":23,"image":"/tvdb-series-logo","score":1}]}}`
			case "api.themoviedb.org/3/tv/77/images":
				if code != 200 {
					return code, ``
				}
			}
			return seasonArt(path, q)
		}}
		p := newTestProvider(t, net)
		p.anidb = anidbWithState(t, net, "", anidbState{}) // no details: complete unless a source fails
		p.cfg.TitleSources, p.cfg.SummarySources = nil, nil
		p.cfg.PosterSources, p.cfg.BackgroundSources, p.cfg.LogoSources = []string{"tvdb", "tmdb"}, []string{"tvdb", "tmdb"}, []string{"tvdb", "tmdb"}
		p.lists.SetForTest(map[int]mapping.Entry{100: {AID: 100, TVDB: onTVDB(3), TMDB: onTMDB(2)}})
		s, err := p.buildShow(context.Background(), "anidb-100")
		if err != nil {
			t.Fatal(err)
		}
		if net.count("api.themoviedb.org/3/tv/77/images") != 1 {
			t.Fatalf("TMDB images %d: the Specials' fallback must ask it once", net.count("api.themoviedb.org/3/tv/77/images"))
		}
		if want := code == 200; s.Complete != want {
			t.Errorf("TMDB art answers %d: complete %v, want %v", code, s.Complete, want)
		}
	}
}

// tvdbSeasonTests: TVDB 500's seasons 1 and 2 hold episodes 1-2 and 3, its
// season 3 episodes 4-5; TMDB 77 splits them 1-2 and 3-5. Season 1 pairs
// with TMDB's, seasons 2 and 3 share one there: no pair.
func tvdbSeasonTests(path string, q map[string][]string) (int, string) {
	switch path {
	case "api4.thetvdb.com/v4/series/500/episodes/official/eng":
		return 200, `{"data":{"episodes":[
			{"id":11,"seasonNumber":1,"number":1,"name":"One"},{"id":12,"seasonNumber":1,"number":2,"name":"Two"},
			{"id":13,"seasonNumber":2,"number":1,"name":"Three"},
			{"id":14,"seasonNumber":3,"number":1,"name":"Four"},{"id":15,"seasonNumber":3,"number":2,"name":"Five"},
			{"id":16,"seasonNumber":0,"number":1,"name":"Special"}]},"links":{"next":null}}`
	case "api.themoviedb.org/3/tv/77":
		return 200, `{"name":"TMDB Name","seasons":[{"season_number":1},{"season_number":2}]}`
	case "api.themoviedb.org/3/tv/77/season/1":
		return 200, `{"episodes":[{"episode_number":1},{"episode_number":2}],"images":{"posters":[{"file_path":"/tmdb-s1-poster"}]}}`
	case "api.themoviedb.org/3/tv/77/season/2":
		return 200, `{"episodes":[{"episode_number":1},{"episode_number":2},{"episode_number":3}],
			"images":{"posters":[{"file_path":"/tmdb-s2-poster"}]}}`
	case "api.anidb.net/httpapi":
		return 200, strings.Replace(anidbXML, "<description>", "<picture>anidb-poster.jpg</picture><description>", 1)
	case "api4.thetvdb.com/v4/episodes/11/extended", "api4.thetvdb.com/v4/episodes/12/extended",
		"api4.thetvdb.com/v4/episodes/13/extended", "api4.thetvdb.com/v4/episodes/14/extended",
		"api4.thetvdb.com/v4/episodes/15/extended", "api4.thetvdb.com/v4/episodes/16/extended":
		return 200, `{"data":{}}`
	}
	return seasonArt(path, q)
}

// In /tvdb each Plex season gets its TheTVDB season's art, TheMovieDb's
// season posters where its season pairs clearly with TheMovieDb's (else
// TheMovieDb's series poster), and AniDB's own poster; the show keeps the
// series' art. The season item carries its art as its image list.
func TestTVDBSeasonItems(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: tvdbSeasonTests})
	p.cfg.PosterSources = []string{"tmdb", "tvdb"}
	s, err := p.buildShow(context.Background(), "tvdb-500")
	if err != nil {
		t.Fatal(err)
	}
	if names(s.Art.Posters) != "tmdb-series-poster" || names(s.Art.Backgrounds) != "tvdb-series-background" {
		t.Errorf("show: posters %q backgrounds %q, want the series'", names(s.Art.Posters), names(s.Art.Backgrounds))
	}
	want := map[int]struct{ posters, backgrounds string }{
		1: {"tmdb-s1-poster", "tvdb-s1-background"},
		2: {"tmdb-series-poster", "tvdb-series-background"}, // shares TMDB season 2 with TVDB season 3
		3: {"tmdb-series-poster", "tvdb-s3-background"},
		0: {"tmdb-series-poster", "tvdb-series-background"}, // TMDB has no specials
	}
	for n, w := range want {
		a := s.SeasonArt[n]
		if names(a.Posters) != w.posters || names(a.Backgrounds) != w.backgrounds {
			t.Errorf("season %d: posters %q backgrounds %q, want %q %q", n, names(a.Posters), names(a.Backgrounds), w.posters, w.backgrounds)
		}
	}
	it := p.seasonItem(Modes["tvdb"], s, 1, false)
	if it.Thumb != s.SeasonArt[1].Posters[0] || it.Art != s.SeasonArt[1].Backgrounds[0] || it.ParentThumb != s.Art.Posters[0] {
		t.Errorf("Season 1 item thumb %q art %q parent %q", it.Thumb, it.Art, it.ParentThumb)
	}
	var types []string
	for _, im := range it.Image {
		types = append(types, im.Type+":"+names([]string{im.URL}))
	}
	if got := strings.Join(types, " "); got != "coverPoster:tmdb-s1-poster background:tvdb-s1-background clearLogo:tvdb-series-logo" {
		t.Errorf("Season 1 images %s", got)
	}

	p.cfg.PosterSources = []string{"anidb", "tvdb"}
	s, err = p.buildShow(context.Background(), "tvdb-500")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(s.SeasonArt[3].Posters); got != "anidb-poster.jpg" || names(s.Art.Posters) != got {
		t.Errorf("AniDB first: season 3 posters %q, show %q, want AniDB's own poster for both", got, names(s.Art.Posters))
	}
}

// With the legacy TheTVDB key, season posters come from v3's season images;
// v3 has no season backgrounds, so backgrounds are the series' even
// without SEASON_ART_FALLBACK.
func TestLegacySeasonArt(t *testing.T) {
	net := &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		switch {
		case path == "api.thetvdb.com/login":
			return 200, `{"token":"t3"}`
		case path == "api.thetvdb.com/series/500":
			return 200, `{"data":{"seriesName":"Series","poster":"v3-series-poster","fanart":"v3-series-fanart"}}`
		case path == "api.thetvdb.com/series/500/images/query" && url.Values(q).Get("keyType") == "season" && url.Values(q).Get("subKey") == "3":
			return 200, `{"data":[{"fileName":"v3-s3-poster"}]}`
		}
		return 0, ""
	}}
	p := newTestProvider(t, net)
	f := fetch.New(t.TempDir())
	f.SetTransport(net)
	p.tvdb = sources.NewTVDB(f, "", "", "legacy")
	p.cfg.SeasonArtFallback = false
	entry := mapping.Entry{TVDB: onTVDB(3)}
	ss := p.newSources(context.Background(), 100, nil, entry, 500, 0, false, firstEntry{})
	s := &Show{Episodes: map[int][]Ep{1: nil}}
	ss.anidbTargets(entry, plexSeasons(s))
	ss.fill(s)
	if names(s.Art.Posters) != "v3-s3-poster" || names(s.Art.Backgrounds) != "v3-series-fanart" {
		t.Errorf("legacy: posters %q backgrounds %q", names(s.Art.Posters), names(s.Art.Backgrounds))
	}
	if c := net.count("api4.thetvdb.com/v4/series/500/extended"); c != 0 {
		t.Errorf("v4 asked %d times", c)
	}
}

// The match report says, per source, how much art came from the season
// and how much from the series, for the show and each Plex season.
func TestSeasonArtReport(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: tvdbSeasonTests})
	store, dir, serve := reportsOn(t, p)
	serve("GET", "/tvdb/library/metadata/tvdb-500", "")
	store.Flush()
	var doc report.ShowDoc
	readJSON(t, filepath.Join(dir, "data", "anidb-100.json"), &doc)
	if len(doc.Builds) != 1 {
		t.Fatalf("%d builds", len(doc.Builds))
	}
	fields := map[string]report.Field{}
	for _, f := range doc.Builds[0].Fields {
		fields[f.Name] = f
	}
	if f := fields["Posters"]; f.Offered["tvdb"] != "1 posters" || !slices.Equal(f.Used, []string{"tvdb"}) {
		t.Errorf("show posters %+v", f)
	}
	if f := fields["Season 1 backgrounds"]; f.Offered["tvdb"] != "season 1: 1 backgrounds" {
		t.Errorf("Season 1 backgrounds %+v", f)
	}
	if f := fields["Season 2 backgrounds"]; f.Offered["tvdb"] != "series: 1 backgrounds" {
		t.Errorf("Season 2 backgrounds %+v", f)
	}
	if f := fields["Specials posters"]; f.Offered["tvdb"] != "specials: 1 posters" {
		t.Errorf("Specials posters %+v", f)
	}
}

// tmdbSeasonPairs pairs a TheTVDB season with TheMovieDb's only when all
// its episodes found there are in that one season, and no other TheTVDB
// season's are.
func TestTMDBSeasonPairs(t *testing.T) {
	// tvdb: TheTVDB's episodes as season -> absolute numbers (specials: their
	// number); tmdb: TheMovieDb's the same way.
	type list map[int][]int
	show := func(tvdb list) *Show {
		s := &Show{Episodes: map[int][]Ep{}}
		for n, abs := range tvdb {
			for i, a := range abs {
				s.Episodes[n] = append(s.Episodes[n], Ep{Season: n, Index: a, listed: spot{n, i + 1}})
			}
		}
		return s
	}
	index := func(tmdb list) episodeIndex {
		var eps []sources.Episode
		for n, abs := range tmdb {
			for i, a := range abs {
				e := sources.Episode{Season: n, Number: i + 1}
				if n > 0 {
					e.Absolute = a
				}
				eps = append(eps, e)
			}
		}
		return indexEpisodes(eps)
	}
	none := tvdbPairing{}
	cases := []struct {
		name       string
		tvdb, tmdb list
		pr         tvdbPairing
		want       map[int]int
	}{
		{"1:1", list{1: {1, 2}, 2: {3}}, list{1: {1, 2}, 2: {3}}, none, map[int]int{1: 1, 2: 2}},
		{"two TVDB seasons in one TMDB season", list{1: {1, 2}, 2: {3, 4}, 3: {5}}, list{1: {1, 2, 3, 4}, 2: {5}}, none, map[int]int{3: 2}},
		{"one TVDB season split on TMDB", list{1: {1, 2, 3, 4}, 2: {5}}, list{1: {1, 2}, 2: {3, 4}, 3: {5}}, none, map[int]int{2: 3}},
		{"specials", list{0: {1, 2}, 1: {1}}, list{0: {1, 2}, 1: {1}}, none, map[int]int{0: 0, 1: 1}},
		{"no TMDB counterpart", list{1: {1, 2}, 2: {3}}, list{1: {1, 2}}, none, map[int]int{1: 1}},
		{"on another TMDB show", list{1: {1, 2}, 2: {3}}, list{1: {1, 2}, 2: {3}},
			tvdbPairing{other: map[spot]otherPlace{{2, 1}: {}}}, map[int]int{1: 1}},
		{"paired through AniDB, not by number", list{1: {1, 2}, 2: {3}}, list{1: {1}, 2: {2, 3}},
			tvdbPairing{other: map[spot]otherPlace{{1, 2}: {spot{1, 1}, true}}}, map[int]int{1: 1, 2: 2}},
	}
	for _, c := range cases {
		if got := tmdbSeasonPairs(show(c.tvdb), index(c.tmdb), c.pr); !maps.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
