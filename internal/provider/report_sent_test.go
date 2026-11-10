package provider

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// The match report records what was sent to Plex: every episode value and
// image with its source, and the show item whole.

// sentXML is AniDB 100: episode 1 has a summary, an air date and a length,
// episode 2 only a title.
const sentXML = `<anime id="100"><type>TV Series</type><startdate>2009-04-01</startdate>
<titles><title type="main" xml:lang="x-jat">Kizuna Test</title></titles>
<picture>anidb-poster.jpg</picture><description>From AniDB.</description>
<episodes>
<episode><epno type="1">1</epno><length>24</length><airdate>2009-04-01</airdate><title xml:lang="en">The Start</title><summary>AniDB summary one.</summary></episode>
<episode><epno type="1">2</epno><title xml:lang="en">The Middle</title></episode>
</episodes></anime>`

// sentSeries is seasonArt's series (TheTVDB 500 with season art, TheMovieDb
// 77) with details on TheTVDB's episodes, 8 series posters on TheTVDB and 2
// on TheMovieDb (with AniDB's one, more than the 10 Plex gets), and
// testSeries' crew: TheTVDB's for episodes 1 and 2 (2 without a writer),
// TheMovieDb's for all three.
func sentSeries(path string, q map[string][]string) (int, string) {
	art := func(kind int, name string, score int) string {
		return `{"type":` + strconv.Itoa(kind) + `,"image":"/` + name + `","score":` + strconv.Itoa(score) + `}`
	}
	switch path {
	case "api.anidb.net/httpapi":
		return 200, sentXML
	case "api4.thetvdb.com/v4/series/500/episodes/official/eng":
		return 200, `{"data":{"episodes":[
			{"id":11,"seasonNumber":1,"number":1,"name":"TVDB One","image":"/e1.jpg","overview":"TVDB summary one.","aired":"2009-04-02","runtime":25},
			{"id":12,"seasonNumber":1,"number":2,"name":"TVDB Two","image":"/e2.jpg","overview":"TVDB summary two.","aired":"2009-04-09","runtime":23},
			{"id":13,"seasonNumber":2,"number":1,"name":"TVDB Three"},
			{"id":14,"seasonNumber":0,"number":1,"name":"TVDB Special","aired":"2009-05-01"}]},"links":{"next":null}}`
	case "api4.thetvdb.com/v4/series/500/artworks":
		list := []string{art(3, "tvdb-b1", 9), art(23, "tvdb-logo", 9)}
		for i := 1; i <= 8; i++ {
			list = append(list, art(2, "p"+strconv.Itoa(i), 100-i)) // sorted by score: p1 first
		}
		return 200, `{"data":{"artworks":[` + strings.Join(list, ",") + `]}}`
	case "api.themoviedb.org/3/tv/77":
		return 200, `{"name":"TMDB Name","seasons":[{"season_number":1},{"season_number":2}]}`
	case "api.themoviedb.org/3/tv/77/images":
		return 200, `{"posters":[{"file_path":"/tmdb-p1"},{"file_path":"/tmdb-p2"}],"backdrops":[{"file_path":"/tmdb-b1"}],
			"logos":[{"file_path":"/tmdb-logo"}]}`
	case "api.themoviedb.org/3/tv/77/season/1":
		return 200, `{"episodes":[{"episode_number":1,"name":"TMDB One","crew":[{"job":"Director","department":"Directing","name":"TMDB Director"}]},
			{"episode_number":2,"name":"TMDB Two","crew":[{"job":"Screenplay","department":"Writing","name":"TMDB Writer"},
				{"job":"Director","department":"Directing","name":"TMDB Director 2"}]}],"images":{"posters":[{"file_path":"/tmdb-s1-poster"}]}}`
	case "api.themoviedb.org/3/tv/77/season/2":
		return 200, `{"episodes":[{"episode_number":1,"name":"TMDB Three","crew":[{"job":"Director","department":"Directing","name":"TMDB Director 3"}]}],
			"images":{"posters":[{"file_path":"/tmdb-s2-poster"}]}}`
	}
	return seasonArt(path, q)
}

// sentProvider is a provider on answer with the match reports on and the
// art settings these tests count on. ask serves a GET like Plex and returns
// the first item of the answer; build returns the newest build on a page.
func sentProvider(t *testing.T, answer func(string, map[string][]string) (int, string)) (p *Provider, ask func(path string) map[string]any, build func(key string) report.Build) {
	t.Helper()
	p = newTestProvider(t, &fakeNet{answer: answer})
	p.cfg.PosterSources = []string{"tvdb", "tmdb", "anidb"}
	p.cfg.BackgroundSources, p.cfg.LogoSources = []string{"tvdb", "tmdb"}, []string{"tvdb", "tmdb"}
	p.cfg.ArtAllSources, p.cfg.SeasonArtFallback = false, true
	p.cfg.EpisodeFillSources, p.cfg.EpisodeTitles = []string{"tvdb"}, []string{"anidb", "tvdb", "tmdb"}
	p.cfg.SummarySources = []string{"anidb", "tvdb", "tmdb"}
	store, dir, _ := reportsOn(t, p)
	ask = func(path string) map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr, req.Host = "127.0.0.1:1234", "kizuna:32600"
		rec := httptest.NewRecorder()
		p.Handler().ServeHTTP(rec, req)
		var c struct {
			MediaContainer struct{ Metadata []map[string]any }
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil || rec.Code != 200 || len(c.MediaContainer.Metadata) != 1 {
			t.Fatalf("GET %s: %d %s (%v), want one item", path, rec.Code, rec.Body, err)
		}
		return c.MediaContainer.Metadata[0]
	}
	build = func(key string) report.Build {
		t.Helper()
		store.Flush()
		var doc report.ShowDoc
		readJSON(t, filepath.Join(dir, "data", key+".json"), &doc)
		if len(doc.Builds) == 0 {
			t.Fatalf("%s: no build on the page", key)
		}
		return doc.Builds[0]
	}
	return p, ask, build
}

// artText is the recorded art, one line per group: "Season 1(1) poster:
// a@tvdb* b@tmdb" (image names, each with its source; * = primary).
func artText(groups []report.ArtGroup) []string {
	var out []string
	for _, g := range groups {
		line := g.Level
		if g.Season != nil {
			line += "(" + strconv.Itoa(*g.Season) + ")"
		}
		line += " " + g.Kind + ":"
		for _, im := range g.Images {
			line += " " + names([]string{im.URL}) + "@" + im.Src
			if im.Primary {
				line += "*"
			}
		}
		out = append(out, line)
	}
	return out
}

func sameLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n%s\nwant\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func episodeOf(t *testing.T, b report.Build, season, index int) report.Episode {
	t.Helper()
	i := slices.IndexFunc(b.Episodes, func(e report.Episode) bool { return e.Season == season && e.Index == index })
	if i < 0 {
		t.Fatalf("S%02dE%02d not recorded", season, index)
	}
	return b.Episodes[i]
}

// sameItem: the recorded show item is the one Plex got.
func sameItem(t *testing.T, b report.Build, sent map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(b.Item, &got); err != nil || !reflect.DeepEqual(got, sent) {
		t.Errorf("recorded item (%v)\n%s\nwant the item sent\n%v", err, b.Item, sent)
	}
}

// sameEpisodes: every recorded episode says what its item sent to Plex
// (asked like Plex, under the show's key root): summary, date, thumb,
// duration, crew and GUIDs. The title is the report's own: "" where the
// item says "Episode N".
func sameEpisodes(t *testing.T, b report.Build, ask func(string) map[string]any, mode string) {
	t.Helper()
	if len(b.Episodes) == 0 {
		t.Errorf("%s: no episodes recorded", b.Root)
	}
	text := func(v any) string { s, _ := v.(string); return s }
	tags := func(v any, key string) []string {
		var out []string
		list, _ := v.([]any)
		for _, x := range list {
			out = append(out, text(x.(map[string]any)[key]))
		}
		return out
	}
	for _, e := range b.Episodes {
		it := ask(fmt.Sprintf("/%s/library/metadata/%s-s%d-e%d", mode, b.Root, e.Season, e.Index))
		ms, _ := it["duration"].(float64)
		sent := report.Episode{Summary: text(it["summary"]), Date: text(it["originallyAvailableAt"]), Thumb: text(it["thumb"]),
			Minutes: int(ms) / 60000, Directors: tags(it["Director"], "tag"), Writers: tags(it["Writer"], "tag"), Guids: tags(it["Guid"], "id")}
		got := report.Episode{Summary: e.Summary, Date: e.Date, Thumb: e.Thumb, Minutes: e.Minutes, Directors: e.Directors, Writers: e.Writers, Guids: e.Guids}
		if !reflect.DeepEqual(got, sent) || int(ms) != e.Minutes*60000 {
			t.Errorf("%s S%02dE%02d: recorded %+v, sent %+v (duration %v)", b.Root, e.Season, e.Index, got, sent, ms)
		}
		if e.Title != "" && e.Title != text(it["title"]) {
			t.Errorf("%s S%02dE%02d: recorded title %q, sent %q", b.Root, e.Season, e.Index, e.Title, it["title"])
		}
		// A source for each value sent, and for nothing else.
		has := map[string]bool{"summary": e.Summary != "", "date": e.Date != "", "thumb": e.Thumb != "", "minutes": e.Minutes > 0,
			"directors": len(e.Directors) > 0, "writers": len(e.Writers) > 0}
		for name, sent := range has {
			if src, ok := e.Src[name]; ok != sent || ok && src == "" {
				t.Errorf("%s S%02dE%02d: %s sent %v, its source %q", b.Root, e.Season, e.Index, name, sent, src)
			}
		}
		if len(e.Src) > len(has) {
			t.Errorf("%s S%02dE%02d: sources %v for values no episode has", b.Root, e.Season, e.Index, e.Src)
		}
	}
}

// A /tvdb build: every episode value is TheTVDB's, but the crew TheMovieDb
// gave where TheTVDB had none. With ART_ALL_SOURCES the images are in
// source order with the source of each, the 10 of a kind Plex gets (not
// the 11 picked), and the item's thumb and art marked.
func TestSentReportTVDB(t *testing.T) {
	p, ask, build := sentProvider(t, sentSeries)
	p.cfg.ArtAllSources = true
	item := ask("/tvdb/library/metadata/tvdb-500")
	b := build("anidb-100")
	sameItem(t, b, item)
	sameEpisodes(t, b, ask, "tvdb")
	s, err := p.show(t.Context(), "tvdb-500")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Art.Posters) != 11 {
		t.Fatalf("%d posters picked, want 11: one more than Plex gets", len(s.Art.Posters))
	}

	const thumb1 = "https://artworks.thetvdb.com/e1.jpg"
	all := func(src string) map[string]string {
		return map[string]string{"summary": src, "date": src, "thumb": src, "minutes": src, "directors": src, "writers": src}
	}
	twoWriter := all("tvdb")
	twoWriter["writers"] = "tmdb"
	for _, want := range []report.Episode{
		{Season: 1, Index: 1, Title: "The Start", From: "anidb", Summary: "TVDB summary one.", Date: "2009-04-02", Thumb: thumb1, Minutes: 25,
			Directors: []string{"TVDB Director"}, Writers: []string{"TVDB Writer"}, Src: all("tvdb")},
		{Season: 1, Index: 2, Title: "The Middle", From: "anidb", Summary: "TVDB summary two.", Date: "2009-04-09",
			Thumb: "https://artworks.thetvdb.com/e2.jpg", Minutes: 23,
			Directors: []string{"TVDB Director 2"}, Writers: []string{"TMDB Writer"}, Src: twoWriter},
		{Season: 2, Index: 3, Title: "TVDB Three", From: "tvdb", Directors: []string{"TMDB Director 3"}, Src: map[string]string{"directors": "tmdb"}},
		{Season: 0, Index: 1, Title: "TVDB Special", From: "tvdb", Date: "2009-05-01", Src: map[string]string{"date": "tvdb"}},
	} {
		got := episodeOf(t, b, want.Season, want.Index)
		got.Titles, got.Pairs, got.Issue = nil, nil, "" // asserted elsewhere
		if !reflect.DeepEqual(got, want) {
			t.Errorf("S%02dE%02d\n%+v\nwant\n%+v", want.Season, want.Index, got, want)
		}
	}

	series := " p1@tvdb p2@tvdb p3@tvdb p4@tvdb p5@tvdb p6@tvdb p7@tvdb p8@tvdb"
	logos := " logo: tvdb-logo@tvdb tmdb-logo@tmdb"
	sameLines(t, "art", artText(b.Art), []string{
		"show poster: p1@tvdb*" + strings.TrimPrefix(series, " p1@tvdb") + " tmdb-p1@tmdb tmdb-p2@tmdb", // AniDB's, the 11th, isn't sent
		"show background: tvdb-b1@tvdb* tmdb-b1@tmdb",
		"show" + logos,
		"Specials(0) poster: tvdb-s0-poster@tvdb*" + series + " tmdb-p1@tmdb",
		"Specials(0) background: tvdb-b1@tvdb* tmdb-b1@tmdb",
		"Specials(0)" + logos,
		"Season 1(1) poster: tvdb-s1-poster@tvdb*" + series + " tmdb-s1-poster@tmdb",
		"Season 1(1) background: tvdb-s1-background@tvdb* tvdb-b1@tvdb tmdb-b1@tmdb",
		"Season 1(1)" + logos,
		"Season 2(2) poster: tvdb-s2-poster@tvdb*" + series + " tmdb-s2-poster@tmdb",
		"Season 2(2) background: tvdb-b1@tvdb* tmdb-b1@tmdb",
		"Season 2(2)" + logos,
	})
	// The images recorded are the ones on the items, in their order.
	var sent []string
	for _, im := range item["Image"].([]any) {
		sent = append(sent, im.(map[string]any)["url"].(string))
	}
	var recorded []string
	for _, g := range b.Art {
		if g.Season == nil {
			for _, im := range g.Images {
				recorded = append(recorded, im.URL)
			}
		}
	}
	sameLines(t, "the show item's images", recorded, sent)

	wantSeasons := []report.Season{
		{Name: "Specials", Count: 1, Index: 0, Thumb: "https://artworks.thetvdb.com/tvdb-s0-poster", Art: "https://artworks.thetvdb.com/tvdb-b1", Date: "2009-05-01"},
		{Name: "Season 1", Count: 2, Index: 1, Thumb: "https://artworks.thetvdb.com/tvdb-s1-poster", Art: "https://artworks.thetvdb.com/tvdb-s1-background", Date: "2009-04-02"},
		{Name: "Season 2", Count: 1, Index: 2, Thumb: "https://artworks.thetvdb.com/tvdb-s2-poster", Art: "https://artworks.thetvdb.com/tvdb-b1"},
	}
	if !slices.Equal(b.Seasons, wantSeasons) {
		t.Errorf("seasons\n%+v\nwant\n%+v", b.Seasons, wantSeasons)
	}
	for _, sn := range b.Seasons { // as the season items say
		it := ask("/tvdb/library/metadata/tvdb-500-s" + strconv.Itoa(sn.Index))
		thumb, _ := it["thumb"].(string)
		art, _ := it["art"].(string)
		date, _ := it["originallyAvailableAt"].(string)
		if sn.Thumb != thumb || sn.Art != art || sn.Date != date || float64(sn.Index) != it["index"] || sn.Name != it["title"] {
			t.Errorf("%s: recorded %+v, its item %v", sn.Name, sn, it)
		}
	}
}

// An /anidb build: AniDB's values where it has them, TheTVDB's where
// EPISODE_FILL_SOURCES filled one in and for the thumbnail, crew from
// TheTVDB and, where it has none, TheMovieDb. Each kind's images come from
// its first source.
func TestSentReportAniDB(t *testing.T) {
	_, ask, build := sentProvider(t, sentSeries)
	item := ask("/anidb/library/metadata/anidb-100")
	b := build("anidb-100")
	sameItem(t, b, item)
	sameEpisodes(t, b, ask, "anidb")
	if guid, _ := item["guid"].(string); !strings.Contains(guid, ".anidb://") || len(item["Image"].([]any)) != 12 {
		t.Errorf("show item %v: want /anidb's GUID and 12 images", item)
	}

	for _, want := range []report.Episode{
		{Season: 1, Index: 1, Title: "The Start", From: "anidb", Summary: "AniDB summary one.", Date: "2009-04-01",
			Thumb: "https://artworks.thetvdb.com/e1.jpg", Minutes: 24, Directors: []string{"TVDB Director"}, Writers: []string{"TVDB Writer"},
			Src: map[string]string{"summary": "anidb", "date": "anidb", "minutes": "anidb", "thumb": "tvdb", "directors": "tvdb", "writers": "tvdb"}},
		{Season: 1, Index: 2, Title: "The Middle", From: "anidb", Summary: "TVDB summary two.", Date: "2009-04-09",
			Thumb: "https://artworks.thetvdb.com/e2.jpg", Minutes: 23, Directors: []string{"TVDB Director 2"}, Writers: []string{"TMDB Writer"},
			Src: map[string]string{"summary": "tvdb", "date": "tvdb", "minutes": "tvdb", "thumb": "tvdb", "directors": "tvdb", "writers": "tmdb"}},
	} {
		got := episodeOf(t, b, want.Season, want.Index)
		got.Titles, got.Pairs, got.Issue = nil, nil, ""
		if !reflect.DeepEqual(got, want) {
			t.Errorf("S%02dE%02d\n%+v\nwant\n%+v", want.Season, want.Index, got, want)
		}
	}
	// The entry starts its series: the series' art, then season 1's, for
	// the show; its Season 1 has the season's.
	sameLines(t, "art", artText(b.Art), []string{
		"show poster: p1@tvdb* p2@tvdb p3@tvdb p4@tvdb p5@tvdb p6@tvdb p7@tvdb p8@tvdb tvdb-s1-poster@tvdb",
		"show background: tvdb-b1@tvdb* tvdb-s1-background@tvdb",
		"show logo: tvdb-logo@tvdb",
		"Season 1(1) poster: tvdb-s1-poster@tvdb*",
		"Season 1(1) background: tvdb-s1-background@tvdb*",
		"Season 1(1) logo: tvdb-logo@tvdb",
	})
	want := []report.Season{{Name: "Season 1", Count: 2, Index: 1, Thumb: "https://artworks.thetvdb.com/tvdb-s1-poster",
		Art: "https://artworks.thetvdb.com/tvdb-s1-background", Date: "2009-04-01"}}
	if !slices.Equal(b.Seasons, want) {
		t.Errorf("seasons %+v, want %+v", b.Seasons, want)
	}
}

// Without EPISODE_FILL_SOURCES TheTVDB fills nothing in: an episode has
// only what AniDB has, and no source for the rest. The crew is not a fill.
func TestSentReportNoFill(t *testing.T) {
	p, ask, build := sentProvider(t, sentSeries)
	p.cfg.EpisodeFillSources = nil
	ask("/anidb/library/metadata/anidb-100")
	b := build("anidb-100")
	sameEpisodes(t, b, ask, "anidb")
	one, two := episodeOf(t, b, 1, 1), episodeOf(t, b, 1, 2)
	if want := map[string]string{"summary": "anidb", "date": "anidb", "minutes": "anidb", "directors": "tvdb", "writers": "tvdb"}; !maps.Equal(one.Src, want) || one.Thumb != "" {
		t.Errorf("S01E01 %+v, want sources %v and no thumbnail", one, want)
	}
	if want := map[string]string{"directors": "tvdb", "writers": "tmdb"}; !maps.Equal(two.Src, want) || two.Summary != "" || two.Date != "" || two.Minutes != 0 {
		t.Errorf("S01E02 %+v, want sources %v and nothing else", two, want)
	}
}

// A movie's one episode has the show's title and, when the show has one,
// its summary: both are recorded as the show's. A special-only movie's
// episode carries its TheTVDB special's GUID. Without AniDB details and
// with no summary on the show, the summary is the TheTVDB special's.
func TestSentReportMovie(t *testing.T) {
	for _, details := range []bool{true, false} {
		net := &fakeNet{answer: specialMovieSeries}
		p := specialsProvider(t, net)
		if details {
			p.anidb = anidbWithState(t, net, "kizunatest", anidbState{})
		}
		store, dir, _ := reportsOn(t, p)
		req := httptest.NewRequest("GET", "/auto/library/metadata/anidb-16983", nil)
		req.RemoteAddr, req.Host = "127.0.0.1:1234", "kizuna:32600"
		rec := httptest.NewRecorder()
		p.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("details %v: %d %s", details, rec.Code, rec.Body)
		}
		store.Flush()
		var doc report.ShowDoc
		readJSON(t, filepath.Join(dir, "data", "anidb-16983.json"), &doc)
		if len(doc.Builds) != 1 || len(doc.Builds[0].Episodes) != 1 {
			t.Fatalf("details %v: page %+v, want one build with the movie", details, doc)
		}
		got := doc.Builds[0].Episodes[0]
		got.Titles, got.Pairs, got.Issue = nil, nil, ""
		want := report.Episode{Season: 1, Index: 1, Title: "Kizuna Film Red", From: "show", Summary: "From AniDB.", Date: "2022-08-06",
			Thumb: "https://artworks.thetvdb.com/s1.jpg", Minutes: 115, Guids: []string{"tvdb://14"},
			Src: map[string]string{"summary": "show", "date": "tvdb", "thumb": "tvdb", "minutes": "anidb"}}
		if !details {
			want.Summary = "The movie, from TheTVDB."
			want.Src["summary"], want.Src["minutes"] = "tvdb", "tvdb"
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("details %v: the movie\n%+v\nwant\n%+v", details, got, want)
		}
	}
}

// A season without art of its own: its item's thumb is the show's poster,
// which the season's row records, and it has no images, so no art group. A
// show no recorded build made has no sources for its images. Dates are
// recorded as sent: a year alone is its first day, one too short to be a
// date nothing.
func TestSentReportByHand(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	store, dir, _ := reportsOn(t, p)
	posters := make([]string, 12)
	for i := range posters {
		posters[i] = "https://img.example/p" + strconv.Itoa(i+1)
	}
	s := &Show{Root: "anidb-100", Title: "Show", Complete: true,
		Art:       sources.Art{Posters: posters, Backgrounds: []string{"https://img.example/b1"}},
		SeasonArt: map[int]sources.Art{1: {Backgrounds: []string{"https://img.example/s1-b"}}},
		Episodes: map[int][]Ep{
			1: {{Season: 1, Index: 1, Title: "One", Date: "2020", Minutes: 24, src: epSources{date: srcAniDB, minutes: srcAniDB, summary: srcAniDB}}},
			2: {{Season: 2, Index: 2, Title: "Two", Date: "200", src: epSources{date: srcTVDB}}},
		}}
	s.setImages()
	s.artBy = &artSources{show: artUsed{posterArt: {{"tvdb", 4}, {"tmdb", 8}}}}
	rec := report.NewRecorder("anidb-100", "anidb")
	p.finishReport(rec, "anidb-100", s, nil, time.Second)
	store.AddBuild("anidb-100", rec)
	store.Flush()
	var doc report.ShowDoc
	readJSON(t, filepath.Join(dir, "data", "anidb-100.json"), &doc)
	b := doc.Builds[0]

	sameLines(t, "art", artText(b.Art), []string{
		"show poster: p1@tvdb* p2@tvdb p3@tvdb p4@tvdb p5@tmdb p6@tmdb p7@tmdb p8@tmdb p9@tmdb p10@tmdb",
		"show background: b1@*",
		"Season 1(1) background: s1-b@*", // art of its own, without a poster: no thumb, no poster group
	})
	want := []report.Season{
		{Name: "Season 1", Count: 1, Index: 1, Art: "https://img.example/s1-b", Date: "2020-01-01"},
		{Name: "Season 2", Count: 1, Index: 2, Thumb: posters[0]}, // the show's poster
	}
	if !slices.Equal(b.Seasons, want) {
		t.Errorf("seasons %+v, want %+v", b.Seasons, want)
	}
	for _, sn := range b.Seasons {
		if it := p.seasonItem(Modes["anidb"], s, sn.Index, false); it.Thumb != sn.Thumb || it.Art != sn.Art || it.OriginallyAvailableAt != sn.Date {
			t.Errorf("%s: recorded %+v, its item thumb %q art %q date %q", sn.Name, sn, it.Thumb, it.Art, it.OriginallyAvailableAt)
		}
	}
	one, two := episodeOf(t, b, 1, 1), episodeOf(t, b, 2, 2)
	if one.Date != "2020-01-01" || one.Minutes != 24 || !maps.Equal(one.Src, map[string]string{"date": "anidb", "minutes": "anidb"}) {
		t.Errorf("S01E01 %+v, want the date as sent, the length, and a source for those two only", one)
	}
	if two.Date != "" || two.Src != nil {
		t.Errorf("S02E02 %+v, want no date (Plex got none) and no source", two)
	}
	var item Item
	if err := json.Unmarshal(b.Item, &item); err != nil || !reflect.DeepEqual(item, p.showItem(Modes["anidb"], s, false)) {
		t.Errorf("item %s (%v), want the show item", b.Item, err)
	}
}

// The item is recorded as the handlers write it: text is not escaped for
// HTML there. The saved page escapes it again, for its script.
func TestItemJSON(t *testing.T) {
	it := Item{RatingKey: "anidb-1", Title: "A <b> & </script>", Index: intp(0)}
	if got, want := string(itemJSON(it)), `{"ratingKey":"anidb-1","key":"","guid":"","type":"","title":"A <b> & </script>","index":0}`; got != want {
		t.Errorf("item %s, want %s", got, want)
	}
	rec := httptest.NewRecorder()
	writeJSON(rec, 200, it)
	if got := strings.TrimSuffix(rec.Body.String(), "\n"); got != string(itemJSON(it)) {
		t.Errorf("writeJSON writes %s, itemJSON %s", got, itemJSON(it))
	}
}

// With the reports off, nothing is kept for them on the show.
func TestReportsOffNoArtSources(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: sentSeries})
	for _, root := range []string{"anidb-100", "tvdb-500"} {
		s, err := p.show(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		if s.artBy != nil {
			t.Errorf("%s: art sources %+v kept with the reports off", root, s.artBy)
		}
	}
}
