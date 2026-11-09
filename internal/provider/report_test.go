package provider

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	if b.Root != "tvdb-500" || b.Mode != "tvdb" || !b.Complete || b.Match == nil || b.Match.Root != "tvdb-500" || b.Match.How != "title search" {
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
	ep := b.Episodes[slices.IndexFunc(b.Episodes, func(e report.Episode) bool { return e.S == 1 && e.E == 1 })]
	if ep.Title != "The Start" || ep.From != "anidb" || ep.Pairs["anidb"] != "100 · 1" {
		t.Errorf("S01E01 %+v: want AniDB's title, paired with AniDB 100 episode 1", ep)
	}
	if len(doc.Requests) != 2 || doc.Requests[1].Answer != "new build" || doc.Requests[0].Build != b.ID ||
		!strings.HasPrefix(doc.Requests[0].Answer, "memory") {
		t.Errorf("requests %+v: want the match (new build), then the metadata request from memory", doc.Requests)
	}
	if doc.LastMatch == nil || doc.LastMatch.Root != "tvdb-500" {
		t.Errorf("last match %+v", doc.LastMatch)
	}

	var un report.UnmatchedDoc
	readJSON(t, filepath.Join(dir, "data", report.UnmatchedKey("Nothing Like This At All")+".json"), &un)
	if un.Match.Root != "" || un.Match.Mode != "anidb" || len(un.Requests) != 1 {
		t.Errorf("unmatched %+v", un)
	}
	for _, page := range []string{"index", "anidb-100", un.Key} {
		if _, err := os.Stat(filepath.Join(dir, page+".html")); err != nil {
			t.Error(err)
		}
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
	guid := ""
	for _, kv := range first.Output {
		if kv[0] == "guid" {
			guid = kv[1]
		}
	}
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
	if !generic(buildChecks(s, &report.Build{}, nil)) {
		t.Error("nothing says why: want the generic check")
	}
	failed := &report.Build{Checks: []report.Check{{Level: report.LevelWarn, Title: "A source failed: tmdb", Cause: true}}}
	if generic(buildChecks(s, failed, nil)) {
		t.Error("a failed source already says why: no generic check")
	}
	s.Placeholders = true
	if generic(buildChecks(s, &report.Build{}, nil)) {
		t.Error("placeholder episodes already say why: no generic check")
	}
}
