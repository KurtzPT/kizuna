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

// plexRequest sends one request through p's routes, from loopback and to
// the container's name, as Plex does.
func plexRequest(ctx context.Context, p *Provider, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
	req.RemoteAddr, req.Host = "127.0.0.1:1234", "kizuna:32600"
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	return rec
}

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
		{"/anidb/library/metadata/anidb-1/grandchildren?X-Plex-Container-Start=1&X-Plex-Container-Size=3", 3, 47, 2},   // S00E02, S01E01, S01E02
		{"/anidb/library/metadata/anidb-1/grandchildren?X-Plex-Container-Start=45&X-Plex-Container-Size=5", 2, 47, 44}, // S01E44, S01E45
		{"/anidb/library/metadata/anidb-1/grandchildren?X-Plex-Container-Start=47", 0, 47, 0},
		{"/anidb/library/metadata/anidb-1/children", 2, 2, 0},
	}
	for _, c := range cases {
		rec := plexRequest(context.Background(), p, "GET", c.path, "")
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
	var logs strings.Builder
	restore := captureLogs(&logs) // the panic's error line and stack
	defer restore()
	for range 2 {
		if _, err := p.show(ctx, "anidb-1"); err == nil || ctx.Err() != nil {
			t.Fatalf("got err %v (ctx %v), want the build's error", err, ctx.Err())
		}
	}
	if !strings.Contains(logs.String(), `level=ERROR msg="show build panicked"`) {
		t.Errorf("logs:\n%s\nwant the panic logged", logs.String())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.builds) != 0 || len(p.shows) != 0 || p.running != 0 {
		t.Errorf("left behind: builds %d shows %d running %d", len(p.builds), len(p.shows), p.running)
	}
}

// /health is only "ok" once both lists are loaded: without the mapping,
// TVDB tags and /tvdb mode find nothing, so Docker must see it unhealthy.
func TestHealthNeedsMapping(t *testing.T) {
	titles, lists := &anidb.Titles{}, &mapping.Lists{}
	p := New(config.Config{}, titles, nil, lists, nil, nil, nil, nil)
	check := func(wantCode int, wantStatus string, wantMissing ...string) {
		t.Helper()
		rec := plexRequest(context.Background(), p, "GET", "/health", "")
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
		return plexRequest(ctx, p, "GET", path, "")
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

// Stopping during the first list load answers the waiting requests 503
// "stopping" at once, not after their 25 s; once the lists are loaded,
// requests are served until the server stops taking them.
func TestStopDuringFirstLoad(t *testing.T) {
	dir := t.TempDir()
	p := New(config.Config{}, nil, sources.NewAniDB(fetch.New(dir), dir, "kizunatest", "1"), nil, nil, nil, nil, nil)
	p.shows["anidb-1"] = cachedShow{&Show{Root: "anidb-1", Title: "Show", Complete: true, Episodes: map[int][]Ep{}}, time.Now()}
	done := p.Loading()
	get := func() *httptest.ResponseRecorder {
		return plexRequest(context.Background(), p, "GET", "/anidb/library/metadata/anidb-1", "")
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- get() }() // waiting for the load, or not there yet: the same answer
	p.Stop()
	p.Stop() // again: nothing
	select {
	case rec := <-answered:
		if rec.Code != 503 || !strings.Contains(rec.Body.String(), `"stopping"`) {
			t.Errorf("stopped while loading: %d %s, want 503 stopping", rec.Code, rec.Body)
		}
	case <-time.After(requestTimeout / 2):
		t.Fatal("still waiting after Stop")
	}
	done()
	if rec := get(); rec.Code != 200 {
		t.Errorf("loaded, then stopping: %d %s, want the show", rec.Code, rec.Body)
	}
}

// A mode whose source isn't set up answers 503 with the reason (Plex keeps
// what it has) and logs it once per 10 minutes; its description still
// answers, so Plex can add the provider. The other mode is unaffected.
func TestModeOff(t *testing.T) {
	dir := t.TempDir()
	p := New(config.Config{}, nil, sources.NewAniDB(fetch.New(dir), dir, "kizunatest", "1"), nil,
		sources.NewTVDB(fetch.New(dir), "", "", ""), nil, nil, nil)
	p.shows["anidb-1"] = cachedShow{&Show{Root: "anidb-1", Title: "Show", Complete: true, Episodes: map[int][]Ep{}}, time.Now()}
	get := func(path string) *httptest.ResponseRecorder {
		return plexRequest(context.Background(), p, "GET", path, "")
	}
	var logs strings.Builder
	restore := captureLogs(&logs)
	for range 3 {
		if rec := get("/tvdb/library/metadata/tvdb-500"); rec.Code != 503 || !strings.Contains(rec.Body.String(), "TVDB_API_KEY") {
			t.Errorf("/tvdb without a TVDB key: %d %s, want 503 naming TVDB_API_KEY", rec.Code, rec.Body)
		}
	}
	restore()
	if n := strings.Count(logs.String(), `level=WARN msg="a request for a mode that is off`); n != 1 {
		t.Errorf("%d warnings, logs:\n%s\nwant one", n, logs.String())
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
	capped := everyTenMinutes{max: 2}
	capped.due("a")
	capped.due("b")
	if !capped.due("c") || len(capped.at) != 1 || !capped.due("a") {
		t.Errorf("past max: kept %v, want everything forgotten", capped.at)
	}
	// long keys are kept bounded, and distinct ones stay distinct
	var long everyTenMinutes
	a, b := strings.Repeat("x", 1<<20)+"a", strings.Repeat("x", 1<<20)+"b"
	if !long.due(a) || long.due(a) || !long.due(b) {
		t.Error("want each long key due once")
	}
	for k := range long.at {
		if len(k) > maxDueKey {
			t.Errorf("kept a key of %d bytes, want at most %d", len(k), maxDueKey)
		}
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
			return plexRequest(ctx, p, c.method, c.path, c.body)
		}
		if rec := serve(ctx); rec.Code != 503 || !strings.Contains(rec.Body.String(), "it is still being built") {
			t.Errorf("%s while the build runs: %d %s, want 503, still being built", c.path, rec.Code, rec.Body)
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

// A build that runs out of its own time without a show fails with
// errBuildTimeout: its waiters get a 503 saying the sources didn't answer,
// not that it is still being built (it is over). A show that doesn't exist
// is a 404.
func TestBuildOutOfTime(t *testing.T) {
	dir := t.TempDir()
	p := New(config.Config{}, nil, sources.NewAniDB(fetch.New(dir), dir, "kizunatest", "1"), nil, nil, nil, nil, nil)
	p.buildFn = func(ctx context.Context, root string) (*Show, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("anidb %s unknown", root)
	}
	var logs strings.Builder
	restore := captureLogs(&logs) // the failed builds' warnings
	defer restore()
	p.mu.Lock()
	b := p.newBuild("anidb-1")
	p.mu.Unlock()
	p.runBuild(context.Background(), time.Millisecond, "anidb-1", b)
	if !errors.Is(b.err, errBuildTimeout) || timedOut(b.err) {
		t.Errorf("build out of time: err %v, want errBuildTimeout, not a request's timeout", b.err)
	}
	rec := httptest.NewRecorder()
	writeUnavailable(rec, b.err)
	if body := rec.Body.String(); rec.Code != 503 || !strings.Contains(body, "the show's sources didn't answer in time, try again") ||
		strings.Contains(body, "still being built") {
		t.Errorf("build out of time: %d %s, want 503, sources didn't answer", rec.Code, body)
	}
	p.buildFn = func(context.Context, string) (*Show, error) { return nil, noShowError{errors.New("anidb 2 unknown")} }
	if rec := plexRequest(context.Background(), p, "GET", "/anidb/library/metadata/anidb-2", ""); rec.Code != 404 {
		t.Errorf("failed build: %d %s, want 404", rec.Code, rec.Body)
	}
}

// Only a show that doesn't exist answers 404 and "no match" (Plex keeps
// both): a source that can't answer now, or a key not set, answers 503 with
// the reason, so Plex asks again later.
func TestFailedBuildAnswers(t *testing.T) {
	tvdbDown := func(path string, q map[string][]string) (int, string) {
		if strings.HasPrefix(path, "api4.thetvdb.com/v4/series/500/episodes") {
			return 500, ""
		}
		return testSeries(path, q)
	}
	tvdbEmpty := func(path string, q map[string][]string) (int, string) {
		if strings.HasPrefix(path, "api4.thetvdb.com/v4/series/500/episodes") {
			return 404, ""
		}
		return testSeries(path, q)
	}
	const match = `{"type":2,"title":"Kizuna Test"}`
	cases := []struct {
		name         string
		answer       func(string, map[string][]string) (int, string)
		noTVDB       bool
		method, path string
		code         int
		why          string // in a 503's body
	}{
		{"TheTVDB down", tvdbDown, false, "GET", "/tvdb/library/metadata/tvdb-500", 503, "no episode list from TheTVDB"},
		{"TheTVDB down, match", tvdbDown, false, "POST", "/tvdb/library/metadata/matches", 503, "no episode list from TheTVDB"},
		{"no episodes on TheTVDB", tvdbEmpty, false, "GET", "/tvdb/library/metadata/tvdb-500", 404, ""},
		{"no episodes on TheTVDB, match", tvdbEmpty, false, "POST", "/tvdb/library/metadata/matches", 200, ""},
		{"no TVDB key, /anidb", testSeries, true, "GET", "/anidb/library/metadata/tvdb-500", 503, "TVDB_API_KEY"},
		{"no TVDB key, /auto", testSeries, true, "GET", "/auto/library/metadata/tvdb-500-s1", 503, "TVDB_API_KEY"},
		{"unknown AniDB id", testSeries, false, "GET", "/anidb/library/metadata/anidb-999", 404, ""},
	}
	for _, c := range cases {
		p := newTestProvider(t, &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
			if path == "api.anidb.net/httpapi" && !slices.Equal(q["aid"], []string{"100"}) {
				return 0, "" // 404
			}
			return c.answer(path, q)
		}})
		if c.noTVDB {
			dir := t.TempDir()
			p.tvdb = sources.NewTVDB(fetch.New(dir), "", "", "")
		}
		var logs strings.Builder
		restore := captureLogs(&logs) // the failed builds' warnings
		rec := plexRequest(context.Background(), p, c.method, c.path, match)
		restore()
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.why) {
			t.Errorf("%s: %d %s, want %d %q", c.name, rec.Code, rec.Body, c.code, c.why)
		}
		if c.code == 200 && strings.Contains(rec.Body.String(), "tvdb-500") {
			t.Errorf("%s: %s, want no match", c.name, rec.Body)
		}
	}
}

// A failed build answers for its show for failTTL, so the requests of one
// Plex refresh don't each build (download, warn) again; one out of time
// isn't kept: the next request builds.
func TestFailedBuildRemembered(t *testing.T) {
	dir := t.TempDir()
	p := New(config.Config{}, nil, sources.NewAniDB(fetch.New(dir), dir, "kizunatest", "1"), nil, nil, nil, nil, nil)
	builds := 0
	fail := errors.New("TheTVDB down")
	p.buildFn = func(context.Context, string) (*Show, error) { builds++; return nil, fail }
	var logs strings.Builder
	restore := captureLogs(&logs)
	defer restore()
	for range 3 {
		if _, err := p.show(context.Background(), "tvdb-5"); !errors.Is(err, fail) {
			t.Fatalf("got %v, want the build's error", err)
		}
	}
	if builds != 1 || strings.Count(logs.String(), "show build failed") != 1 {
		t.Errorf("%d builds, logs:\n%s\nwant one build and one warning", builds, logs.String())
	}
	p.mu.Lock()
	f := p.failed["tvdb-5"]
	f.at = time.Now().Add(-failTTL)
	p.failed["tvdb-5"] = f
	p.mu.Unlock()
	if _, err := p.show(context.Background(), "tvdb-5"); !errors.Is(err, fail) || builds != 2 {
		t.Errorf("after failTTL: %v, %d builds, want a new build", err, builds)
	}

	p.buildFn = func(ctx context.Context, _ string) (*Show, error) {
		builds++
		<-ctx.Done()
		return nil, fail
	}
	p.mu.Lock()
	clear(p.failed)
	p.mu.Unlock()
	for range 2 {
		p.mu.Lock()
		b := p.newBuild("tvdb-6")
		p.mu.Unlock()
		p.runBuild(context.Background(), time.Millisecond, "tvdb-6", b)
	}
	p.mu.Lock()
	_, kept := p.failed["tvdb-6"]
	p.mu.Unlock()
	if kept || builds != 4 {
		t.Errorf("out of time: kept %v after %d builds, want not kept", kept, builds)
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
