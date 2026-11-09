package provider

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kizuna/internal/config"
	"kizuna/internal/fetch"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// Plex clients send the match body as JSON (numbers as numbers or strings)
// or as a form, some hints only in the query string; the body wins.
func TestReadMatch(t *testing.T) {
	one, two, three := 1, 2, 3
	cases := []struct {
		name, query, body string
		want              MatchRequest
	}{
		{"JSON numbers", "", `{"type":4,"title":"E","grandparentTitle":"Show","year":2009,"index":2,"parentIndex":1,"manual":1}`,
			MatchRequest{Type: 4, Title: "E", GrandparentTitle: "Show", Year: 2009, Index: &two, ParentIndex: &one, Manual: 1}},
		{"JSON strings", "", `{"type":"3","parentTitle":"Show","year":"2009","index":"3","manual":"true"}`,
			MatchRequest{Type: 3, ParentTitle: "Show", Year: 2009, Index: &three, Manual: 1}},
		{"JSON bool manual", "", `{"title":"Show","manual":true}`, MatchRequest{Type: 2, Title: "Show", Manual: 1}},
		{"JSON manual off", "", `{"title":"Show","manual":0}`, MatchRequest{Type: 2, Title: "Show"}},
		{"form", "", "type=2&title=Show+Name&year=2009&guid=tmdb%3A%2F%2F7&filename=%2Fa%2Fb.mkv&manual=1",
			MatchRequest{Type: 2, Title: "Show Name", Year: 2009, GUID: "tmdb://7", Filename: "/a/b.mkv", Manual: 1}},
		{"query fallback", "?title=Query&year=2001&manual=true", `{"title":"Body"}`,
			MatchRequest{Type: 2, Title: "Body", Year: 2001, Manual: 1}},
		{"query only", "?type=2&title=Query", "", MatchRequest{Type: 2, Title: "Query"}},
		{"garbage numbers", "", `{"title":"Show","year":"soon","index":"x"}`, MatchRequest{Type: 2, Title: "Show"}},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/anidb/library/metadata/matches"+c.query, strings.NewReader(c.body))
		got, err := readMatch(r)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !sameMatch(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
	if _, err := readMatch(httptest.NewRequest("POST", "/", strings.NewReader(`{"title":`))); err == nil {
		t.Error("broken JSON: want an error (HTTP 400)")
	}
}

func sameMatch(a, b MatchRequest) bool {
	same := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	if !same(a.Index, b.Index) || !same(a.ParentIndex, b.ParentIndex) {
		return false
	}
	a.Index, a.ParentIndex, b.Index, b.ParentIndex = nil, nil, nil, nil
	return a == b
}

// Keys and routes that name nothing answer 404, without building anything
// for a key that isn't a show key.
func TestMetadataNotFound(t *testing.T) {
	dir := t.TempDir()
	p := New(config.Config{}, nil, sources.NewAniDB(fetch.New(dir), dir, "kizunatest", "1"), nil, nil, nil, nil, nil)
	s := &Show{Root: "anidb-1", Title: "Show", Complete: true, Episodes: map[int][]Ep{1: {{Season: 1, Index: 1}}}}
	p.shows["anidb-1"] = cachedShow{s, time.Now()}
	p.buildFn = func(context.Context, string) (*Show, error) { return nil, errors.New("must not be built") }
	cases := []string{
		"/anidb/library/metadata/anidb-1/bogus",               // unknown sub
		"/anidb/library/metadata/anidb-1-s1-e1/children",      // an episode has no children
		"/anidb/library/metadata/anidb-1-s1-e1/grandchildren", //
		"/anidb/library/metadata/anidb-1-s1/grandchildren",    // nor a season grandchildren
		"/anidb/library/metadata/anidb-1-s7",                  // unknown season
		"/anidb/library/metadata/anidb-1-s7/children",         //
		"/anidb/library/metadata/anidb-1-s1-e9",               // unknown episode
		"/anidb/library/metadata/anidb-01",                    // not a show key
		"/anidb/library/metadata/foo-1",                       //
	}
	for _, path := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr, req.Host = "127.0.0.1:1234", "kizuna:32600"
		p.Handler().ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Errorf("%s: HTTP %d %s, want 404", path, rec.Code, rec.Body)
		}
	}
}

// A match request that joins a build a metadata request started puts its
// match in that build's report (the "Matched by title search" check); the
// first match request on a build is the one kept.
func TestJoinedMatchInReport(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	store, dir, _ := reportsOn(t, p)
	release := make(chan struct{})
	p.buildFn = func(ctx context.Context, root string) (*Show, error) {
		<-release
		return p.buildShow(ctx, root)
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel() // each caller gives up at once; the build goes on
	starter := report.WithRequest(gone, &report.Request{Mode: "anidb"})
	if _, err := p.show(starter, "anidb-100"); !timedOut(err) {
		t.Fatalf("starter: %v, want it to give up", err)
	}
	for _, score := range []int{90, 50} {
		m := &report.Match{Mode: "anidb", Type: "show", Title: "Kizuna Test", How: "title search", Root: "anidb-100", Score: score}
		if _, err := p.show(report.WithMatch(gone, m), "anidb-100"); !timedOut(err) {
			t.Fatalf("joiner: %v, want it to give up", err)
		}
	}
	p.mu.Lock()
	b := p.builds["anidb-100"]
	p.mu.Unlock()
	close(release)
	<-b.done
	store.Flush()

	var doc report.ShowDoc
	readJSON(t, filepath.Join(dir, "data", "anidb-100.json"), &doc)
	if len(doc.Builds) != 1 || doc.Builds[0].Match == nil || doc.Builds[0].Match.Score != 90 {
		t.Fatalf("builds %+v: want one, with the first joined match (score 90)", doc.Builds)
	}
	if !slices.ContainsFunc(doc.Builds[0].Checks, func(c report.Check) bool { return c.Title == "Matched by title search with score 90" }) {
		t.Errorf("checks %+v: want the title search check", doc.Builds[0].Checks)
	}
}

// An unmatched title's report page stays until a match for it gets its
// show: a build that fails leaves the title without a match in Plex.
func TestUnmatchedPageKeptUntilShow(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	store, dir, serve := reportsOn(t, p)
	page := filepath.Join(dir, "data", report.UnmatchedKey("Kizuna Test")+".json")
	store.Unmatched(report.Match{Mode: "anidb", Type: "show", Title: "Kizuna Test"})
	store.Flush()
	if _, err := os.Stat(page); err != nil {
		t.Fatal(err)
	}
	p.buildFn = func(context.Context, string) (*Show, error) { return nil, errors.New("anidb 100 unknown") }
	serve("POST", "/anidb/library/metadata/matches", `{"type":2,"title":"Kizuna Test"}`)
	store.Flush()
	if _, err := os.Stat(page); err != nil {
		t.Errorf("after a failed build: %v, want the unmatched page kept", err)
	}
	p.buildFn = nil
	serve("POST", "/anidb/library/metadata/matches", `{"type":2,"title":"Kizuna Test"}`)
	store.Flush()
	if _, err := os.Stat(page); !os.IsNotExist(err) {
		t.Errorf("after a match with its show: %v, want the unmatched page gone", err)
	}
}
