package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"kizuna/internal/report"
)

// reportsOn turns the match reports on for p, and returns their folder and
// a func serving one request through the HTTP handler.
func reportsOn(t *testing.T, p *Provider) (*report.Store, string, func(method, path, body string) int) {
	t.Helper()
	dir := t.TempDir()
	store, err := report.Open(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	p.SetReports(store)
	return store, dir, func(method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr, req.Host = "127.0.0.1:1234", "kizuna:32600"
		rec := httptest.NewRecorder()
		p.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
}

// A match, a metadata request and a title that matches nothing, through
// the HTTP handler: the show's page holds the build (match, fields, trace,
// episodes) and both requests; the unmatched title gets its own page.
func TestMatchReport(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	store, dir, serve := reportsOn(t, p)
	serve("POST", "/tvdb/library/metadata/matches", `{"type":2,"title":"Kizuna Test"}`)
	serve("GET", "/tvdb/library/metadata/tvdb-500", "")
	serve("POST", "/anidb/library/metadata/matches", `{"type":2,"title":"Nothing Like This At All","year":1999}`)
	store.Flush()

	var doc report.ShowDoc
	readJSON(t, filepath.Join(dir, "data", "anidb-100.json"), &doc) // the TVDB series' page is its AniDB entry's
	if len(doc.Builds) != 1 {
		t.Fatalf("%d builds, want 1", len(doc.Builds))
	}
	b := doc.Builds[0]
	if b.Root != "tvdb-500" || b.Mode != "tvdb" || !b.Complete || b.Match == nil || b.Match.Root != "tvdb-500" || b.Match.How != "title search" ||
		b.Match.By != (report.MatchBy{Search: report.SearchFound, Best: 100}) {
		t.Errorf("build %s /%s complete %v match %+v", b.Root, b.Mode, b.Complete, b.Match)
	}
	if doc.Title != "Kizuna Test" || !slices.Contains(doc.IDs, report.ID{Site: "tmdb", Value: "77"}) {
		t.Errorf("title %q ids %v", doc.Title, doc.IDs)
	}
	var title *report.Field
	for i := range b.Fields {
		if b.Fields[i].Name == "Title" {
			title = &b.Fields[i]
		}
	}
	if title == nil || !slices.Equal(title.Used, []string{"anidb"}) || title.Offered["anidb"] != "Kizuna Test" || len(title.Asked) != 1 {
		t.Errorf("title field %+v: want AniDB's, asked first and alone", title)
	}
	tvdbFetched := slices.ContainsFunc(b.Trace, func(s report.Span) bool { return s.Src == "tvdb" && s.Kind == "net" })
	if !tvdbFetched {
		t.Errorf("trace %+v: want TVDB's downloads", b.Trace)
	}
	ep := b.Episodes[slices.IndexFunc(b.Episodes, func(e report.Episode) bool { return e.Season == 1 && e.Index == 1 })]
	if ep.Title != "The Start" || ep.From != "anidb" || ep.Pairs["anidb"] != "100 · 1" {
		t.Errorf("S01E01 %+v: want AniDB's title, paired with AniDB 100 episode 1", ep)
	}
	if len(doc.Requests) != 2 || doc.Requests[1].Answer != (report.Answer{Kind: report.AnswerNewBuild}) || doc.Requests[0].Build != b.ID ||
		doc.Requests[0].Answer.Kind != report.AnswerMemory || doc.Requests[0].Answer.Age < 0 || doc.Requests[0].Answer.After != 0 {
		t.Errorf("requests %+v: want the match (new build), then the metadata request from memory", doc.Requests)
	}
	if doc.LastMatch == nil || doc.LastMatch.Root != "tvdb-500" {
		t.Errorf("last match %+v", doc.LastMatch)
	}

	var un report.UnmatchedDoc
	readJSON(t, filepath.Join(dir, "data", report.UnmatchedKey("Nothing Like This At All")+".json"), &un)
	if un.Match.Root != "" || un.Match.Mode != "anidb" || len(un.Requests) != 1 || un.Match.By.TagRule() || un.Match.Query != "" ||
		un.Match.By.ID != "" || un.Match.By.Search == "" || un.Match.By.Search == report.SearchFound {
		t.Errorf("unmatched %+v: want a title search no-match, the title without tags", un)
	}
	for _, page := range []string{"index", "anidb-100", un.Key} {
		if _, err := os.Stat(filepath.Join(dir, page+".html")); err != nil {
			t.Error(err)
		}
	}
}

// In /auto, a [tvdb-N] tag with TheTVDB off is no match: its unmatched
// page and request say the tag needs /tvdb and what to set.
func TestAutoTagModeOffReport(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	p.tvdb = nil
	store, dir, serve := reportsOn(t, p)
	serve("POST", "/auto/library/metadata/matches", `{"type":2,"title":"Kizuna Test [tvdb-500]"}`)
	store.Flush()

	var un report.UnmatchedDoc
	readJSON(t, filepath.Join(dir, "data", report.UnmatchedKey("Kizuna Test")+".json"), &un)
	want := "tag [tvdb-500]: /tvdb is off, set TVDB_API_KEY or TVDB_LEGACY_API_KEY"
	if un.Match.Root != "" || un.Match.How != want || len(un.Requests) != 1 ||
		un.Requests[0].Answer != (report.Answer{Kind: report.AnswerNoMatch}) || un.Requests[0].Note != want {
		t.Errorf("unmatched %+v, want no match: %s", un, want)
	}
	// The page says a tag rule decided, not the title search, and its
	// example tag goes after the title without the tag already there.
	by := report.MatchBy{ID: report.MatchTagOff, Text: "[tvdb-500]", Off: "tvdb", Need: "TVDB_API_KEY or TVDB_LEGACY_API_KEY"}
	if !un.Match.By.TagRule() || un.Match.By != by || un.Match.Query != "Kizuna Test" {
		t.Errorf("match %+v: want a tag rule, %+v, query %q", un.Match, by, "Kizuna Test")
	}
}

// Two different id tags are a tag rule no-match too; a Fix Match that also
// ran the title search is not.
func TestRecordMatchTagRule(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	const title = "Show [anidb-1] [tvdb-2]"
	tags := idOutcome{kind: idManyTags, text: "[anidb-1] [tvdb-2]"}
	off := idOutcome{kind: idModeOff, text: "[tvdb-500]", path: "tvdb", need: "TVDB_API_KEY or TVDB_LEGACY_API_KEY"}
	nothing := searchOutcome{kind: searchNothing}
	for _, c := range []struct {
		d    decision
		how  string
		want bool
	}{
		{decision{title: title, id: tags}, "id tags [anidb-1] [tvdb-2]: only one is allowed", true},
		{decision{title: title, id: off}, "tag [tvdb-500]: /tvdb is off, set TVDB_API_KEY or TVDB_LEGACY_API_KEY", true},
		{decision{title: title, id: off, search: nothing}, "tag [tvdb-500]: /tvdb is off, set TVDB_API_KEY or TVDB_LEGACY_API_KEY; title search: no similar title", false},
		{decision{title: title, search: nothing}, "title search: no similar title", false},
	} {
		m := p.recordMatch(Modes["auto"], MatchRequest{Type: 2}, c.d)
		if m.How != c.how {
			t.Errorf("how %q, want %q", m.How, c.how)
		}
		if m.By.TagRule() != c.want || m.Query != "Show" {
			t.Errorf("%s: tag rule %v query %q, want %v and %q", c.how, m.By.TagRule(), m.Query, c.want, "Show")
		}
	}
	byTag := decision{title: "Show [anidb-1]", id: idOutcome{kind: idTag, text: "[anidb-1]"}, cands: []candidate{{"anidb-1", 100}}}
	if m := p.recordMatch(Modes["auto"], MatchRequest{Type: 2}, byTag); m.By.TagRule() || m.How != "tag [anidb-1]" || m.Root != "anidb-1" || m.Score != 100 ||
		m.By != (report.MatchBy{ID: report.MatchTag, Text: "[anidb-1]"}) {
		t.Errorf("a match by tag: %+v, want no tag rule no-match", m)
	}
}

// How each request was answered, as the report's kinds: a build that
// failed, the failure answering again, a request that gave up, the show
// from memory, a new build and a Fix Match list.
func TestRequestAnswers(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	reportsOn(t, p)
	ask := func(ctx context.Context) (report.Answer, error) {
		rq := &report.Request{Mode: "anidb"}
		_, err := p.show(report.WithRequest(ctx, rq), "anidb-100")
		return rq.Answer, err
	}
	bg := context.Background()
	p.buildFn = func(context.Context, string) (*Show, error) { return nil, errors.New("TheTVDB is down") }
	var logs strings.Builder
	restore := captureLogs(&logs) // the failed build's warning
	first, _ := ask(bg)
	again, err := ask(bg)
	restore()
	if first != (report.Answer{Kind: report.AnswerUnavailable}) {
		t.Errorf("a build that fails: %+v, want not available", first)
	}
	if err == nil || again.Kind != report.AnswerFailed || again.Age < 0 || again.After != 120 {
		t.Errorf("the failure again (%v): %+v, want failed, built again after 2 minutes", err, again)
	}

	p.mu.Lock()
	clear(p.failed)
	p.mu.Unlock()
	release := make(chan struct{})
	p.buildFn = func(ctx context.Context, root string) (*Show, error) {
		<-release
		return p.buildShow(ctx, root)
	}
	gone, cancel := context.WithCancel(bg)
	cancel() // each caller gives up at once; the build goes on
	for _, who := range []string{"the build's starter", "one that joined it"} {
		if a, err := ask(gone); !timedOut(err) || a != (report.Answer{Kind: report.AnswerTimedOut}) {
			t.Errorf("%s (%v): %+v, want timed out", who, err, a)
		}
	}
	p.mu.Lock()
	b := p.builds["anidb-100"]
	p.mu.Unlock()
	close(release)
	<-b.done
	if a, err := ask(bg); err != nil || a.Kind != report.AnswerMemory || a.Age < 0 || a.After != 0 {
		t.Errorf("the built show (%v): %+v, want from memory", err, a)
	}
	p.buildFn = nil
	p.mu.Lock()
	clear(p.shows)
	p.mu.Unlock()
	if a, err := ask(bg); err != nil || a != (report.Answer{Kind: report.AnswerNewBuild}) {
		t.Errorf("the show gone from memory (%v): %+v, want a new build", err, a)
	}
	rq := &report.Request{Mode: "anidb"}
	if items, err := p.Match(report.WithRequest(bg, rq), Modes["anidb"], MatchRequest{Type: 2, Title: "Kizuna Test", Manual: 1}); err != nil || len(items) == 0 ||
		rq.Answer != (report.Answer{Kind: report.AnswerFixMatch}) {
		t.Errorf("Fix Match (%v): %d items, %+v, want a list", err, len(items), rq.Answer)
	}
}

// A field's record: the sources asked up to the one used, all of them
// when none had a value or every source's images are joined, and the ones
// used.
func TestRecordFields(t *testing.T) {
	rec := report.NewRecorder("anidb-1", "anidb")
	ss := &showSources{rec: rec}
	order := []string{"anidb", "tvdb", "tmdb"}
	value := func(src string) string { return "from " + src }
	ss.recordField("Title", order, "tvdb", value)
	ss.recordField("Summary", order, "", value)
	ss.recordArt("Posters", order, []sourceCount{{"tvdb", 14}, {"tmdb", 9}}, true, value)
	ss.recordArt("Backgrounds", order, []sourceCount{{"tvdb", 3}}, false, value)
	ss.recordArt("Logos", order, nil, false, value)
	all := map[string]string{"anidb": "from anidb", "tvdb": "from tvdb", "tmdb": "from tmdb"}
	upToTVDB := map[string]string{"anidb": "from anidb", "tvdb": "from tvdb"}
	want := []report.Field{
		{Name: "Title", Order: order, Asked: []string{"anidb", "tvdb"}, Offered: upToTVDB, Used: []string{"tvdb"}},
		{Name: "Summary", Order: order, Asked: order, Offered: all},
		{Name: "Posters", Order: order, Asked: order, Offered: all, Used: []string{"tvdb", "tmdb"}},
		{Name: "Backgrounds", Order: order, Asked: []string{"anidb", "tvdb"}, Offered: upToTVDB, Used: []string{"tvdb"}},
		{Name: "Logos", Order: order, Asked: order, Offered: all},
	}
	if got := rec.Facts().Fields; !reflect.DeepEqual(got, want) {
		t.Errorf("fields\n%+v\nwant\n%+v", got, want)
	}
	// With the reports off nothing is noted, and nothing is asked again.
	off := &showSources{}
	never := func(string) string { t.Error("a source's value was read with the reports off"); return "" }
	off.recordField("Title", order, "tvdb", never)
	off.recordArt("Posters", order, []sourceCount{{"tvdb", 1}}, true, never)
}

// A field with no sources (X_SOURCES=none) has an empty order in the
// page's data, not null: the page's script maps over it.
func TestRecordFieldEmptyOrder(t *testing.T) {
	dir := t.TempDir()
	store, err := report.Open(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	rec := report.NewRecorder("anidb-1", "anidb")
	ss := &showSources{rec: rec}
	ss.recordField("Studio", nil, "", func(string) string { return "" })
	rec.Finish(time.Second, nil, report.Result{Head: report.ShowHead{Title: "Show"}})
	store.AddBuild("anidb-1", rec)
	store.Flush()
	data, err := os.ReadFile(filepath.Join(dir, "data", "anidb-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"order":[]`) || strings.Contains(string(data), `"order":null`) {
		t.Errorf("page data %s: want the field's order []", data)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

// A build started by a metadata request is reported under that request's
// provider: /auto shows /auto's GUIDs in "Sent to Plex". A second build
// within the hour gets AniDB's entry from memory, and the trace says so.
func TestMetadataBuildReport(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	store, dir, serve := reportsOn(t, p)
	serve("GET", "/auto/library/metadata/anidb-100", "")
	p.mu.Lock()
	delete(p.shows, "anidb-100") // as if it expired
	p.mu.Unlock()
	serve("GET", "/anidb/library/metadata/anidb-100", "")
	store.Flush()

	var doc report.ShowDoc
	readJSON(t, filepath.Join(dir, "data", "anidb-100.json"), &doc)
	if len(doc.Builds) != 2 {
		t.Fatalf("%d builds, want 2", len(doc.Builds))
	}
	first, second := doc.Builds[1], doc.Builds[0]
	var item Item
	if err := json.Unmarshal(first.Item, &item); err != nil {
		t.Fatalf("first build's item %s: %v", first.Item, err)
	}
	guid := item.GUID
	if first.Mode != "auto" || first.Match != nil || !strings.Contains(guid, ".auto://") {
		t.Errorf("first build /%s, guid %q: want /auto's", first.Mode, guid)
	}
	if second.Mode != "anidb" || first.ID == second.ID {
		t.Errorf("second build /%s id %s", second.Mode, second.ID)
	}
	kind := func(b report.Build, src string) report.SpanKind {
		for _, sp := range b.Trace {
			if sp.Src == src && sp.Label == "anidb/anime/100.xml" {
				return sp.Kind
			}
		}
		return ""
	}
	if kind(first, "anidb") != report.SpanNet || kind(second, "anidb") != report.SpanMemory {
		t.Errorf("AniDB rows %q then %q, want net then memory", kind(first, "anidb"), kind(second, "anidb"))
	}
}

// With the reports off, nothing is noted for them per episode.
func TestReportsOffNoPairs(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	for _, root := range []string{"anidb-100", "tvdb-500"} {
		s, err := p.show(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		s.eachEpisode(func(ep *Ep) {
			if ep.pairs != nil {
				t.Errorf("%s S%02dE%02d has pairs %v with the reports off", root, ep.Season, ep.Index, ep.pairs)
			}
		})
	}
}

// A bug in the report code is logged; it doesn't take the process down.
func TestSafeReport(t *testing.T) {
	ran := false
	safeReport("anidb-1", func() { ran = true; panic("report bug") })
	if !ran {
		t.Error("work didn't run")
	}
}

func TestSourceOfKey(t *testing.T) {
	for key, want := range map[string]string{
		"anidb/anime/1.xml":                             "anidb",
		"anidb/animetitles.xml":                         "anidb",
		"tvdb/series/5/episodes.json":                   "tvdb",
		"tvdb3/series/5.json":                           "tvdb",
		"tvdb4/x.json":                                  "tvdb",
		"tmdb/tv/77.json":                               "tmdb",
		"mal/v2-21-full.json":                           "mal",
		"ann/5.xml":                                     "ann",
		"mapping/anime-list-master.xml":                 "map",
		"https://api4.thetvdb.com/v4/login":             "tvdb",
		"https://api.themoviedb.org/3/tv/1":             "tmdb",
		"https://api.myanimelist.net/v2/anime/1":        "mal",
		"https://cdn.animenewsnetwork.com/encyclopedia": "ann",
		"http://api.anidb.net:9001/httpapi?aid=1":       "anidb",
		"https://raw.githubusercontent.com/x/y":         "other",
		"somewhere/else":                                "other",
	} {
		if got := sourceOfKey(key); got != want {
			t.Errorf("%s: %s, want %s", key, got, want)
		}
	}
}

// An incomplete build gets the generic check only when no other check
// already says why.
func TestBuildChecksIncomplete(t *testing.T) {
	generic := func(cs []report.Check) bool {
		return slices.ContainsFunc(cs, func(c report.Check) bool { return c.Title == "Built with something missing" })
	}
	s := &Show{Title: "Show", Complete: false}
	if !generic(buildChecks(s, report.Facts{}, nil)) {
		t.Error("nothing says why: want the generic check")
	}
	rec := report.NewRecorder("anidb-1", "anidb")
	rec.Check(report.Check{Level: report.LevelInfo, Title: "Something else"})
	if !generic(buildChecks(s, rec.Facts(), nil)) {
		t.Error("a recorded check that doesn't say why: want the generic check")
	}
	rec.Check(report.Check{Level: report.LevelWarn, Title: "A source failed: tmdb", Cause: true})
	if generic(buildChecks(s, rec.Facts(), nil)) {
		t.Error("a failed source already says why: no generic check")
	}
	s.Placeholders = true
	if generic(buildChecks(s, report.Facts{}, nil)) {
		t.Error("placeholder episodes already say why: no generic check")
	}
}

// The checks of a finished build, in the report: the ones recorded while
// it ran, then buildChecks' in its order.
func TestFinishReportChecksOrder(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	store, dir, _ := reportsOn(t, p)
	rec := report.NewRecorder("anidb-100", "anidb")
	rec.Check(report.Check{Level: report.LevelWarn, Title: "A source failed: tmdb", Cause: true})
	rec.SetMatch(&report.Match{Mode: "anidb", Title: "Show", Root: "anidb-100", Score: 90, By: report.MatchBy{Search: report.SearchFound, Best: 90}})
	ss := &showSources{rec: rec}
	ss.recordField("Summary", []string{"anidb", "tvdb"}, "", func(string) string { return "" })
	s := &Show{Root: "anidb-100", Title: "Show &amp; Tell", Complete: false, Episodes: map[int][]Ep{1: {{Season: 1, Index: 1, Title: "One", Date: "2020-01-01"}, {Season: 1, Index: 2}}}}
	p.finishReport(rec, "anidb-100", s, nil, time.Second)
	store.AddBuild("anidb-100", rec)
	store.Flush()
	var doc report.ShowDoc
	readJSON(t, filepath.Join(dir, "data", "anidb-100.json"), &doc)
	if len(doc.Builds) != 1 {
		t.Fatalf("%d builds, want 1", len(doc.Builds))
	}
	b := doc.Builds[0]
	want := []report.Check{
		{Level: report.LevelWarn, Title: "A source failed: tmdb"},
		{Level: report.LevelInfo, Title: "Matched by title search with score 90",
			Text: "Not an exact title. If it is the wrong show, add an id tag such as [anidb-N] to the folder name.", Section: report.SectionMatch},
		{Level: report.LevelWarn, Title: "1 episode has no real title",
			Text: `S01E02. No source in EPISODE_TITLES has a title for them, or only a placeholder like "Episode 5".`, Section: report.SectionEpisodes},
		{Level: report.LevelInfo, Title: "1 episode has no air date", Text: "S01E02.", Section: report.SectionEpisodes},
		{Level: report.LevelWarn, Title: "HTML entity in the show's title or summary", Text: "Plex shows it literally.", Section: report.SectionFields},
		{Level: report.LevelWarn, Title: "No source had a summary", Text: "Asked: anidb, tvdb.", Section: report.SectionFields},
	}
	if !reflect.DeepEqual(b.Checks, want) {
		t.Errorf("checks\n%+v\nwant\n%+v", b.Checks, want)
	}
	if doc.Title != "Show &amp; Tell" || b.Complete || b.EpsTotal != 2 || len(b.Episodes) != 2 || len(b.Seasons) != 1 ||
		b.Seasons[0] != (report.Season{Name: "Season 1", Count: 2, Index: 1, Date: "2020-01-01"}) ||
		len(b.Item) == 0 || len(b.Mapping) == 0 || b.TookMS != 1000 || b.Error != "" {
		t.Errorf("build %+v, want the show's head, seasons, episodes, item and mapping", b)
	}

	// A failed build: the head and the mapping only.
	failed := report.NewRecorder("anidb-100", "anidb")
	p.finishReport(failed, "anidb-100", nil, errors.New("boom"), time.Second)
	store.AddBuild("anidb-100", failed)
	store.Flush()
	doc = report.ShowDoc{}
	readJSON(t, filepath.Join(dir, "data", "anidb-100.json"), &doc)
	if b := doc.Builds[0]; len(doc.Builds) != 2 || b.Error != "boom" || b.Complete || b.Checks != nil || b.Episodes != nil || b.Item != nil || b.Art != nil || b.Seasons != nil || len(b.Mapping) == 0 || doc.Title != "Kizuna Test" {
		t.Errorf("failed build %+v (title %q), want its error, the mapping and the entry's title", doc.Builds[0], doc.Title)
	}
}

// Placeholder episodes wait for the next build only when AniDB is set up
// and may answer; without ANIDB_CLIENT the show is complete and the check
// says what to set, and for an anime in its 24 h block, until when.
func TestBuildChecksPlaceholders(t *testing.T) {
	until := time.Date(2026, 11, 10, 18, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		complete bool
		blocked  time.Time
		want     string
	}{{false, time.Time{}, "until the next build"}, {true, time.Time{}, "Set ANIDB_CLIENT"},
		{true, until, "block asking again until 2026-11-10 18:30 UTC"}} {
		s := &Show{Title: "Show", Complete: c.complete, Placeholders: true, anidbBlocked: c.blocked}
		cs := buildChecks(s, report.Facts{}, nil)
		i := slices.IndexFunc(cs, func(c report.Check) bool { return c.Title == "Placeholder episodes" })
		if i < 0 || !strings.Contains(cs[i].Text, c.want) || c.complete && strings.Contains(cs[i].Text, "next build") {
			t.Errorf("complete %v blocked %v: checks %+v, want placeholder text with %q", c.complete, c.blocked, cs, c.want)
		}
		if slices.ContainsFunc(cs, func(c report.Check) bool { return c.Title == "Built with something missing" }) {
			t.Errorf("complete %v blocked %v: the placeholder check already says why: no generic check", c.complete, c.blocked)
		}
	}
}

// The checks a build's match brings, word for word: a title search that
// wasn't exact, and a match by name inside a TheTVDB id, with the tags that
// make it sure. Everything else that decided brings none.
func TestBuildChecksMatch(t *testing.T) {
	const guid = "com.plexapp.agents.thetvdb://81797?lang=en"
	search := func(score int) *report.Check {
		return &report.Check{Level: report.LevelInfo, Title: "Matched by title search with score " + strconv.Itoa(score),
			Text: "Not an exact title. If it is the wrong show, add an id tag such as [anidb-N] to the folder name.", Section: report.SectionMatch}
	}
	byName := func(note string) *report.Check {
		return &report.Check{Level: report.LevelWarn, Title: "Matched by name inside a TheTVDB id",
			Text:    "The TheTVDB id names a whole series; the title or file name picked one of its movies or specials: " + note + ".",
			Section: report.SectionMatch}
	}
	film := &report.ByName{AID: 16983, Sure: "[anidb-16983] or [tmdb-900667]"}
	for _, c := range []struct {
		name  string
		by    report.MatchBy
		score int
		want  *report.Check
	}{
		{"title search, 90", report.MatchBy{Search: report.SearchFound, Best: 90}, 90, search(90)},
		{"title search, 99", report.MatchBy{Search: report.SearchFound, Best: 99}, 99, search(99)},
		{"title search, exact", report.MatchBy{Search: report.SearchFound, Best: 100}, 100, nil},
		{"tag", report.MatchBy{ID: report.MatchTag, Text: "[anidb-69]"}, 100, nil},
		{"guid", report.MatchBy{ID: report.MatchGUID, Text: guid}, 100, nil},
		{"tag, by name", report.MatchBy{ID: report.MatchTag, Text: "[tvdb-81797]", ByName: film}, 100,
			byName("AniDB 16983, may be wrong; use [anidb-16983] or [tmdb-900667] to be sure")},
		{"tag, by name, a lower name score", report.MatchBy{ID: report.MatchTag, Text: "[tvdb-81797]", ByName: film}, 87,
			byName("AniDB 16983, may be wrong; use [anidb-16983] or [tmdb-900667] to be sure")},
		{"guid, by name", report.MatchBy{ID: report.MatchGUID, Text: guid, ByName: &report.ByName{AID: 8857, Sure: "[anidb-8857]"}}, 100,
			byName("AniDB 8857, may be wrong; use [anidb-8857] to be sure")},
		// what a build's match never is, but the facts can say
		{"tag, then a title search", report.MatchBy{ID: report.MatchTag, Text: "[anidb-69]", Search: report.SearchFound, Best: 90}, 90, nil},
		{"title search, nothing", report.MatchBy{Search: report.SearchNone}, 0, nil},
		{"title search, below min", report.MatchBy{Search: report.SearchLow, Best: 62}, 0, nil},
		{"title search, no such season", report.MatchBy{Search: report.SearchFound, Best: 90, Missing: "season 3"}, 0, nil},
		{"two tags", report.MatchBy{ID: report.MatchManyTags, Text: "[anidb-69] {mal-21}"}, 0, nil},
		{"tag mode off", report.MatchBy{ID: report.MatchTagOff, Text: "[anidb-69]", Off: "anidb", Need: "ANIDB_CLIENT"}, 0, nil},
	} {
		facts := report.Facts{Match: &report.Match{By: c.by, Score: c.score}}
		got := buildChecks(&Show{Title: "Show", Complete: true}, facts, nil)
		var want []report.Check
		if c.want != nil {
			want = []report.Check{*c.want}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: checks %+v, want %+v", c.name, got, want)
		}
	}
	if got := buildChecks(&Show{Title: "Show", Complete: true}, report.Facts{}, nil); got != nil {
		t.Errorf("a build no match started: checks %+v, want none", got)
	}
}
