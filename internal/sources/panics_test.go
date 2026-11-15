package sources

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

// panicOn is a log handler that panics with "boom" when msg is logged, on
// the goroutine that logs it: a panic on a goroutine this package starts,
// where only that goroutine can recover it. Every other line (debug too)
// goes to logs.
type panicOn struct {
	msg string
	slog.Handler
}

func (h panicOn) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		panic("boom")
	}
	return h.Handler.Handle(ctx, r)
}

// panicLogs makes msg panic until the test ends, and returns the rest of
// what is logged.
func panicLogs(t *testing.T, msg string) *logBuffer {
	logs := &logBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(panicOn{msg, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})}))
	t.Cleanup(func() { slog.SetDefault(old) })
	return logs
}

// panicsOn is a context whose fetch observer panics with "boom" for every
// read whose key contains part, on the goroutine that reads it.
func panicsOn(part string) context.Context {
	return fetch.WithObserver(context.Background(), func(e fetch.Event) {
		if strings.Contains(e.Key, part) {
			panic("boom")
		}
	})
}

// raised calls f and returns the panic it raised, as the goroutine that
// panicked kept it.
func raised(t *testing.T, f func()) *goroutinePanic {
	t.Helper()
	var r any
	func() {
		defer func() { r = recover() }()
		f()
	}()
	p, ok := r.(*goroutinePanic)
	if !ok {
		t.Fatalf("panicked with %#v, want the goroutine's panic", r)
	}
	return p
}

// checkRaised checks a raised panic has the goroutine's value and stack,
// where (a function name) shows, and that its log line carries both.
func checkRaised(t *testing.T, p *goroutinePanic, where string) {
	t.Helper()
	if p.value != "boom" || p.String() != "boom" || !strings.Contains(string(p.stack), where) {
		t.Fatalf("panic %v with stack\n%s\nwant boom from %s", p.value, p.stack, where)
	}
	var b strings.Builder
	slog.New(slog.NewTextHandler(&b, nil)).Error("show build panicked", "err", p)
	if s := b.String(); !strings.Contains(s, "err.panic=boom") || !strings.Contains(s, "err.stack=") || !strings.Contains(s, where) {
		t.Fatalf("log line lacks the panic or its stack: %s", s)
	}
}

// A panic in one of TheMovieDb's season goroutines is raised again on the
// caller (the show build, which recovers it) once every season is done,
// with the stack of the goroutine that panicked. Without that it would end
// the process.
func TestTMDBSeasonPanicRaisedOnCaller(t *testing.T) {
	const sq = "?api_key=key&language=en-US&append_to_response=images&include_image_language=en,ja,null"
	f := fetch.New(t.TempDir())
	f.SetTransport(&tvdbNet{answers: map[string]string{
		"api.themoviedb.org/3/tv/77?api_key=key&language=en-US": `{"seasons":[{"season_number":1},{"season_number":2},{"season_number":3}]}`,
		"api.themoviedb.org/3/tv/77/season/1" + sq:              `{"episodes":[{"episode_number":1}]}`,
		"api.themoviedb.org/3/tv/77/season/2" + sq:              `{"episodes":[{"episode_number":1}]}`,
		"api.themoviedb.org/3/tv/77/season/3" + sq:              `{"episodes":[{"episode_number":1}]}`,
	}})
	tm := NewTMDB(f, "key")
	f.ThrottleAll(0)
	p := raised(t, func() { _, _, _ = tm.EpisodesAndPosters(panicsOn("tv-77-s2-"), 77) })
	checkRaised(t, p, "EpisodesAndPosters")
	// The panic was taken: the next call has none.
	if eps, _, err := tm.EpisodesAndPosters(context.Background(), 77); err != nil || len(eps) != 3 {
		t.Errorf("next call: %d episodes, %v; want 3 and no error", len(eps), err)
	}
}

// A panic in TheTVDB's crew workers is raised again on the caller once
// they are done. Each worker goes on with the next record after one: with
// every record panicking, more records than workers still end (the sender
// isn't left waiting for a worker).
func TestTVDBCrewWorkerPanicRaisedOnCaller(t *testing.T) {
	answers := map[string]string{v4 + "/login": `{"data":{"token":"t"}}`}
	ids := make([]int, 0, 2*crewWorkers+1)
	for id := 1; id <= 2*crewWorkers+1; id++ {
		ids = append(ids, id)
		answers[v4+"/episodes/"+strconv.Itoa(id)+"/extended"] = `{"data":{"characters":[{"personName":"Dir","peopleType":"Director"}]}}`
	}
	tv, n := newTVDBTest(t, answers, "v4key", "", "")
	tv.f.ThrottleAll(0)
	p := raised(t, func() { _, _ = tv.EpisodeCrew(panicsOn("episodes/"), ids) })
	checkRaised(t, p, "crewJob")
	if c := n.count(v4 + "/episodes/" + strconv.Itoa(len(ids)) + "/extended"); c != 1 {
		t.Errorf("the last record asked %d times, want 1: every record is taken", c)
	}
	tv.WaitIdle()
	// Nothing was left queued or held: the records are read from the cache.
	crew, pending := tv.EpisodeCrew(context.Background(), ids)
	if len(crew) != len(ids) || pending {
		t.Errorf("next call: %d records, pending %v; want all %d", len(crew), pending, len(ids))
	}
}

// A panic in TheTVDB's background crew queue is logged, and ends only that
// record: it leaves the queue, and the batch goes on with the next one.
func TestTVDBBackgroundCrewPanicLogged(t *testing.T) {
	tv, _ := newTVDBTest(t, map[string]string{v4 + "/login": `{"data":{"token":"t"}}`}, "v4key", "", "") // the records: 404
	tv.f.ThrottleAll(0)
	logs := panicLogs(t, "tvdb: episode record not available")
	tv.queueCrew([]int{1, 2})
	tv.WaitIdle()
	if c := strings.Count(logs.String(), `msg="tvdb: background episode record panicked"`); c != 2 {
		t.Fatalf("%d panics logged, want 2 (one per record):\n%s", c, logs)
	}
	if s := logs.String(); !strings.Contains(s, "episode=2") || !strings.Contains(s, "goroutine") {
		t.Errorf("the log lines lack the record or the stack:\n%s", s)
	}
	tv.crewMu.Lock()
	queued := len(tv.crewQueued)
	tv.crewMu.Unlock()
	if queued != 0 {
		t.Errorf("%d records still queued", queued)
	}
}

// A panic in an AniDB background fetch is logged, and ends only that
// entry: it leaves the queue (a later Prefetch queues it again), and the
// worker goes on with the next one. AniDB isn't asked.
func TestAniDBPrefetchPanicLogged(t *testing.T) {
	c, calls := fakeAniDB(t, [2]string{"200", anidbSample})
	c.mu.Lock()
	c.state.BannedUntil = time.Now().Add(time.Hour) // nothing goes out
	c.mu.Unlock()
	logs := panicLogs(t, "anidb: not calling the API")
	c.Prefetch(1)
	c.Prefetch(2)
	c.WaitIdle()
	count := func() int { return strings.Count(logs.String(), `msg="anidb: background fetch panicked"`) }
	if n := count(); n != 2 {
		t.Fatalf("%d panics logged, want 2 (one per entry):\n%s", n, logs)
	}
	c.mu.Lock()
	queued, workers := len(c.queued), c.workers
	c.mu.Unlock()
	if queued != 0 || workers != 0 {
		t.Fatalf("%d entries queued, %d workers left; want none", queued, workers)
	}
	c.Prefetch(1)
	c.WaitIdle()
	if n := count(); n != 3 {
		t.Errorf("%d panics logged, want 3: the entry was queued again", n)
	}
	if calls.Load() != 0 {
		t.Errorf("AniDB asked %d times while banned", calls.Load())
	}
}

// A panic in TheTVDB's background token renewal is logged and counts as a
// failed login: the login under way ends (a later login would wait for it
// forever), the old token stays in use, and no renewal starts during the
// login pause.
func TestTVDBRenewalPanicLogged(t *testing.T) {
	tv, _ := newTVDBTest(t, map[string]string{v4 + "/login": `{"data":{"token":"t-new"}}`}, "v4key", "", "")
	logs := captureLogs(t)
	tv.mu.Lock()
	tv.token, tv.tokenAt = "t-old", time.Now().Add(-8*24*time.Hour) // due for renewal, valid for weeks
	tv.mu.Unlock()
	tv.f = nil // the renewal's request panics on its goroutine
	ctx := context.Background()
	for call := 1; call <= 2; call++ {
		if token, _, err := tv.login(ctx); token != "t-old" || err != nil {
			t.Fatalf("call %d: %q %v, want the old token", call, token, err)
		}
		tv.WaitIdle()
	}
	if c := strings.Count(logs.String(), `msg="tvdb: token renewal panicked"`); c != 1 {
		t.Fatalf("%d renewal panics logged, want 1 (the second call is in the login pause):\n%s", c, logs)
	}
	tv.mu.Lock()
	running, loginErr := tv.loggingIn, tv.loginErr
	tv.mu.Unlock()
	if running != nil || loginErr == nil || !strings.Contains(loginErr.Error(), "renewal panicked") {
		t.Errorf("login under way %v, login error %v; want none and the panic", running, loginErr)
	}
}

// MyAnimeList's request carries the client id in a header, which Go would
// carry to whatever host a redirect names: a redirect isn't followed.
func TestMALNoRedirect(t *testing.T) {
	f := fetch.New(t.TempDir())
	var mu sync.Mutex
	var asked []string
	f.SetTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		asked = append(asked, r.URL.Host+" "+r.Header.Get("X-MAL-CLIENT-ID"))
		mu.Unlock()
		h := http.Header{}
		if r.URL.Host == "api.myanimelist.net" {
			h.Set("Location", "https://elsewhere.example/x")
			return &http.Response{StatusCode: http.StatusFound, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader(`{"synopsis":"x"}`)), Request: r}, nil
	}))
	m := NewMAL(f, "client-secret")
	f.ThrottleAll(0)
	if a, err := m.Anime(context.Background(), 1); err == nil {
		t.Errorf("%+v, want the redirect as an error", a)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "api.myanimelist.net client-secret" {
		t.Errorf("asked %q, want MyAnimeList only", asked)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A panic in an AniDB call's download (here its transport; it could come
// after AniDB answered) is the call's error, and not one the backoff takes
// as AniDB being down: the call stays counted, the backoff doesn't move,
// and the anime waits its 24 h as after an error about it, so a panic that
// comes back doesn't ask AniDB again.
func TestAniDBCallPanicStaysCounted(t *testing.T) {
	c, _ := fakeAniDB(t, [2]string{"200", anidbSample})
	_ = captureLogs(t)
	var sent int
	c.f.SetTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent++
		panic("boom")
	}))
	ctx := context.Background()
	if _, err := c.Anime(ctx, 1); !errors.Is(err, fetch.ErrPanicked) {
		t.Fatalf("err %v, want the panic", err)
	}
	c.mu.Lock()
	calls, failures, retryAt, calling, blocked := len(c.state.Sent), c.state.Failures, c.state.RetryAt, c.calling[1], !c.state.Failed[1].IsZero()
	c.mu.Unlock()
	if calls != 1 || failures != 0 || !retryAt.IsZero() || calling != nil || !blocked {
		t.Fatalf("calls %d, failures %d, retry at %v, call under way %v, blocked %v; want 1 call counted, no backoff, the call ended, the anime blocked",
			calls, failures, retryAt, calling, blocked)
	}
	if _, err := c.Anime(ctx, 1); !errors.Is(err, errAniDBUnavailable) || sent != 1 {
		t.Errorf("again: %v, %d requests; want the 24 h block and no request", err, sent)
	}
}
