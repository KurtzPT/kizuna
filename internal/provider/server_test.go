package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"kizuna/internal/anidb"
	"kizuna/internal/config"
	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/sources"
)

func TestChildrenArePaged(t *testing.T) {
	dir := t.TempDir()
	p := New(config.Config{}, nil, sources.NewAniDB(fetch.New(dir), dir, "kizunatest", "1"), nil, nil, nil, nil, nil)
	s := &Show{Root: "anidb-1", Title: "Show", Complete: true, Episodes: map[int][]Ep{}}
	for i := 1; i <= 45; i++ {
		s.Episodes[1] = append(s.Episodes[1], Ep{Season: 1, Index: i})
	}
	s.Episodes[0] = []Ep{{Season: 0, Index: 1}, {Season: 0, Index: 2}}
	p.shows["anidb-1"] = cachedShow{s, time.Now()}

	cases := []struct {
		path             string
		size, total, idx int // idx: index of the first item
	}{
		{"/anidb/library/metadata/anidb-1-s1/children", 20, 45, 1},
		{"/anidb/library/metadata/anidb-1-s1/children?X-Plex-Container-Start=40&X-Plex-Container-Size=20", 5, 45, 41},
		{"/anidb/library/metadata/anidb-1-s1/children?X-Plex-Container-Start=99", 0, 45, 0},
		{"/anidb/library/metadata/anidb-1-s1/children?X-Plex-Container-Size=9223372036854775807", 45, 45, 1},
		{"/anidb/library/metadata/anidb-1/grandchildren?X-Plex-Container-Start=1&X-Plex-Container-Size=3", 3, 47, 2}, // S00E02, S01E01, S01E02
		{"/anidb/library/metadata/anidb-1/children", 2, 2, 0},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", c.path, nil)
		req.RemoteAddr, req.Host = "127.0.0.1:1234", "kizuna:32600"
		p.Handler().ServeHTTP(rec, req)
		var got Container
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: %d %s", c.path, rec.Code, rec.Body)
		}
		if rec.Code != 200 {
			t.Fatalf("%s: HTTP %d %s", c.path, rec.Code, rec.Body)
		}
		mc := got.MediaContainer
		if mc.Size != c.size || len(mc.Metadata) != c.size || mc.TotalSize != c.total {
			t.Errorf("%s: size %d (%d items) total %d, want %d of %d", c.path, mc.Size, len(mc.Metadata), mc.TotalSize, c.size, c.total)
			continue
		}
		if c.size > 0 && *mc.Metadata[0].Index != c.idx {
			t.Errorf("%s: first index %d, want %d", c.path, *mc.Metadata[0].Index, c.idx)
		}
	}
}

// A build that panics must answer its callers and leave nothing behind, or
// every later request for the show would wait for a build that never ends.
func TestPanickingBuildIsReleased(t *testing.T) {
	p := New(config.Config{}, nil, nil, nil, nil, nil, nil, nil) // no sources: building panics
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 2 {
		if _, err := p.show(ctx, "anidb-1"); err == nil || ctx.Err() != nil {
			t.Fatalf("got err %v (ctx %v), want the build's error", err, ctx.Err())
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.builds) != 0 || len(p.shows) != 0 {
		t.Errorf("left behind: builds %d shows %d", len(p.builds), len(p.shows))
	}
}

// /health is only "ok" once both lists are loaded: without the mapping,
// TVDB tags and /tvdb mode find nothing, so Docker must see it unhealthy.
func TestHealthNeedsMapping(t *testing.T) {
	titles, lists := &anidb.Titles{}, &mapping.Lists{}
	p := New(config.Config{}, titles, nil, lists, nil, nil, nil, nil)
	check := func(wantCode int, wantStatus string, wantMissing ...string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/health", nil)
		req.RemoteAddr, req.Host = "127.0.0.1:1234", "kizuna:32600"
		p.Handler().ServeHTTP(rec, req)
		var got healthAnswer
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if rec.Code != wantCode || got.Status != wantStatus || !slices.Equal(got.Missing, wantMissing) || got.Version == "" {
			t.Errorf("HTTP %d %s, want %d status %q missing %v", rec.Code, rec.Body, wantCode, wantStatus, wantMissing)
		}
	}
	check(503, "starting")
	titles.SetForTest(map[int][]anidb.Title{1: {{Type: "main", Text: "A"}}})
	check(503, "degraded", "mapping")
	lists.SetForTest(map[int]mapping.Entry{1: {AID: 1}})
	check(200, "ok")
}

// While the first list load runs, /health answers "starting" even when a
// list is already there (a cached copy), and Plex's requests wait for the
// load instead of being answered without the lists. A request that gives up
// first gets a 503, not an answer Plex would keep.
func TestRequestsWaitForFirstLoad(t *testing.T) {
	titles, lists := &anidb.Titles{}, &mapping.Lists{}
	titles.SetForTest(map[int][]anidb.Title{1: {{Type: "main", Text: "A"}}})
	lists.SetForTest(map[int]mapping.Entry{1: {AID: 1}})
	dir := t.TempDir()
	p := New(config.Config{}, titles, sources.NewAniDB(fetch.New(dir), dir, "kizunatest", "1"), lists, nil, nil, nil, nil)
	p.shows["anidb-1"] = cachedShow{&Show{Root: "anidb-1", Title: "Show", Complete: true, Episodes: map[int][]Ep{}}, time.Now()}
	done := p.Loading()
	get := func(ctx context.Context, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx, "GET", path, nil)
		req.RemoteAddr, req.Host = "127.0.0.1:1234", "kizuna:32600"
		p.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := get(context.Background(), "/health"); rec.Code != 503 || !strings.Contains(rec.Body.String(), `"starting"`) {
		t.Errorf("health while loading: %d %s, want 503 starting", rec.Code, rec.Body)
	}
	gaveUp, cancel := context.WithCancel(context.Background())
	cancel()
	if rec := get(gaveUp, "/anidb/library/metadata/anidb-1"); rec.Code != 503 {
		t.Errorf("request given up while loading: %d %s, want 503", rec.Code, rec.Body)
	}
	answered := make(chan int, 1)
	go func() { answered <- get(context.Background(), "/anidb/library/metadata/anidb-1").Code }()
	select {
	case code := <-answered:
		t.Fatalf("answered %d before the first load was over", code)
	case <-time.After(50 * time.Millisecond):
	}
	done()
	select {
	case code := <-answered:
		if code != 200 {
			t.Errorf("after the load: HTTP %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request still waiting after the load")
	}
	if rec := get(context.Background(), "/health"); rec.Code != 200 {
		t.Errorf("health after the load: %d %s, want 200 ok", rec.Code, rec.Body)
	}
}

// A mode whose source isn't set up answers 503 with the reason (Plex keeps
// what it has) and logs it once per 10 minutes (the global logger isn't
// swapped here: that would change later tests' allocation counts); its description still
// answers, so Plex can add the provider. The other mode is unaffected.
func TestModeOff(t *testing.T) {
	dir := t.TempDir()
	p := New(config.Config{}, nil, sources.NewAniDB(fetch.New(dir), dir, "kizunatest", "1"), nil,
		sources.NewTVDB(fetch.New(dir), "", "", ""), nil, nil, nil)
	p.shows["anidb-1"] = cachedShow{&Show{Root: "anidb-1", Title: "Show", Complete: true, Episodes: map[int][]Ep{}}, time.Now()}
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr, req.Host = "127.0.0.1:1234", "kizuna:32600"
		p.Handler().ServeHTTP(rec, req)
		return rec
	}
	for range 3 {
		if rec := get("/tvdb/library/metadata/tvdb-500"); rec.Code != 503 || !strings.Contains(rec.Body.String(), "TVDB_API_KEY") {
			t.Errorf("/tvdb without a TVDB key: %d %s, want 503 naming TVDB_API_KEY", rec.Code, rec.Body)
		}
	}
	if p.offLogged.due("tvdb") {
		t.Error("/tvdb off: want it logged by the first request and not due again for 10 minutes")
	}
	if rec := get("/tvdb"); rec.Code != 200 {
		t.Errorf("/tvdb description: %d, want 200", rec.Code)
	}
	if rec := get("/anidb/library/metadata/anidb-1"); rec.Code != 200 {
		t.Errorf("/anidb with AniDB set up: %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := get("/auto/library/metadata/anidb-1"); rec.Code != 200 {
		t.Errorf("/auto with AniDB set up: %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestEveryTenMinutes(t *testing.T) {
	var e everyTenMinutes
	if !e.due("a") || e.due("a") || !e.due("b") {
		t.Error("want each key due once")
	}
	e.at["a"] = time.Now().Add(-11 * time.Minute)
	if !e.due("a") {
		t.Error("want a key due again after 10 minutes")
	}
}

// A show that isn't ready before the request's time runs out answers 503
// (Plex keeps what it has), in match and metadata alike, and the build goes
// on: the next request gets the show from memory.
func TestTimedOutShowAnswers503(t *testing.T) {
	titles := &anidb.Titles{}
	titles.SetForTest(map[int][]anidb.Title{100: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test"}}})
	lists := &mapping.Lists{}
	lists.SetForTest(map[int]mapping.Entry{100: {AID: 100}})
	dir := t.TempDir()
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/anidb/library/metadata/anidb-100", ""},
		{"POST", "/anidb/library/metadata/matches", `{"type":2,"title":"Kizuna Test"}`},
	} {
		p := New(config.Config{}, titles, sources.NewAniDB(fetch.New(dir), dir, "kizunatest", "1"), lists, nil, nil, nil, nil)
		ctx, giveUp := context.WithCancel(context.Background())
		release := make(chan struct{})
		p.buildFn = func(context.Context, string) (*Show, error) {
			giveUp() // the request runs out of time while the build runs
			<-release
			return &Show{Root: "anidb-100", Title: "Kizuna Test", Complete: true, Episodes: map[int][]Ep{1: {{Season: 1, Index: 1}}}}, nil
		}
		serve := func(ctx context.Context) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(ctx, c.method, c.path, strings.NewReader(c.body))
			req.RemoteAddr, req.Host = "127.0.0.1:1234", "kizuna:32600"
			p.Handler().ServeHTTP(rec, req)
			return rec
		}
		if rec := serve(ctx); rec.Code != 503 || !strings.Contains(rec.Body.String(), "in time") {
			t.Errorf("%s while the build runs: %d %s, want 503", c.path, rec.Code, rec.Body)
		}
		p.mu.Lock()
		b := p.builds["anidb-100"]
		p.mu.Unlock()
		if b == nil {
			t.Fatalf("%s: the build stopped with the request", c.path)
		}
		close(release)
		<-b.done
		if rec := serve(context.Background()); rec.Code != 200 || !strings.Contains(rec.Body.String(), "anidb-100") {
			t.Errorf("%s after the build: %d %s, want the show", c.path, rec.Code, rec.Body)
		}
	}
}

// A build that runs out of its own time without a show fails with a
// context error (the handlers answer 503); any other failure is a 404.
func TestBuildOutOfTime(t *testing.T) {
	dir := t.TempDir()
	p := New(config.Config{}, nil, sources.NewAniDB(fetch.New(dir), dir, "kizunatest", "1"), nil, nil, nil, nil, nil)
	p.buildFn = func(ctx context.Context, root string) (*Show, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("anidb %s unknown", root)
	}
	b := &build{done: make(chan struct{})}
	p.builds["anidb-1"] = b
	p.runBuild(context.Background(), time.Millisecond, "anidb-1", b)
	if !timedOut(b.err) {
		t.Errorf("build out of time: err %v, want a context error", b.err)
	}
	p.buildFn = func(context.Context, string) (*Show, error) { return nil, errors.New("anidb 2 unknown") }
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/anidb/library/metadata/anidb-2", nil)
	req.RemoteAddr, req.Host = "127.0.0.1:1234", "kizuna:32600"
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Errorf("failed build: %d %s, want 404", rec.Code, rec.Body)
	}
}

// A build ends a little before the request that started it gives up, so
// that request answers with what came back; one with little time left still
// gets a build worth caching.
func TestBuildTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if got := buildTime(ctx); got > requestTimeout-buildMargin || got < requestTimeout-2*buildMargin {
		t.Errorf("full request: %v, want about %v", got, requestTimeout-buildMargin)
	}
	if got := buildTime(context.Background()); got != requestTimeout-buildMargin {
		t.Errorf("no deadline: %v, want %v", got, requestTimeout-buildMargin)
	}
	late, cancelLate := context.WithTimeout(context.Background(), time.Second)
	defer cancelLate()
	if got := buildTime(late); got != minBuildTime {
		t.Errorf("little time left: %v, want %v", got, minBuildTime)
	}
}
