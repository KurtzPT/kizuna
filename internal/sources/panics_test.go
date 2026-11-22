package sources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

// panicOn is a log handler that panics with "boom" when msg is logged, on
// the goroutine that logs it: a panic on a goroutine this package starts,
// where only that goroutine can recover it. Every other line (debug too)
// goes to logs. from, if set, limits the panic to msg logged from within
// that function (a part of its name in the stack).
type panicOn struct {
	msg, from string
	slog.Handler
}

func (h panicOn) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg && (h.from == "" || strings.Contains(string(debug.Stack()), h.from)) {
		panic("boom")
	}
	return h.Handler.Handle(ctx, r)
}

// panicLogs makes msg panic until the test ends, and returns the rest of
// what is logged.
func panicLogs(t *testing.T, msg string) *logBuffer { return panicLogsFrom(t, msg, "") }

// panicLogsFrom is panicLogs, for msg logged from within from only.
func panicLogsFrom(t *testing.T, msg, from string) *logBuffer {
	logs := &logBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(panicOn{msg, from, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})}))
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
func raised(t *testing.T, f func()) *fetch.GoroutinePanic {
	t.Helper()
	var r any
	func() {
		defer func() { r = recover() }()
		f()
	}()
	p, ok := r.(*fetch.GoroutinePanic)
	if !ok {
		t.Fatalf("panicked with %#v, want the goroutine's panic", r)
	}
	return p
}

// checkRaised checks a raised panic has the goroutine's value and stack,
// where (a function name) shows, and that its log line carries both.
func checkRaised(t *testing.T, p *fetch.GoroutinePanic, where string) {
	t.Helper()
	if p.Value != "boom" || p.String() != "boom" || !strings.Contains(string(p.Stack), where) {
		t.Fatalf("panic %v with stack\n%s\nwant boom from %s", p.Value, p.Stack, where)
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
	p := raised(t, func() { _, _ = tv.EpisodeCrew(panicsOn("episodes/"), ids, time.Time{}) })
	checkRaised(t, p, "crewJob")
	if c := n.count(v4 + "/episodes/" + strconv.Itoa(len(ids)) + "/extended"); c != 1 {
		t.Errorf("the last record asked %d times, want 1: every record is taken", c)
	}
	tv.WaitIdle()
	// Nothing was left queued or held: the records are read from the cache.
	crew, pending := tv.EpisodeCrew(context.Background(), ids, time.Time{})
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
	tv.queueCrew([]int{1, 2}, crewTTL)
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

// A panic in a login the caller waits for goes on to the caller as it was,
// and counts as a failed login: the login under way ends (a later login
// would wait for it until its ctx ended), and the next one waits for the
// login pause.
func TestTVDBLoginPanicEndsTheLogin(t *testing.T) {
	tv, n := newTVDBTest(t, map[string]string{v4 + "/login": `{"data":{"token":"t"}}`}, "v4key", "", "")
	f := tv.f
	tv.f = nil // the login's request panics on the caller's goroutine
	var r any
	func() {
		defer func() { r = recover() }()
		_, _, _ = tv.login(context.Background())
	}()
	if _, ok := r.(runtime.Error); !ok {
		t.Fatalf("panicked with %#v, want the request's own panic", r)
	}
	tv.f = f
	tv.mu.Lock()
	running, loginErr := tv.loggingIn, tv.loginErr
	tv.mu.Unlock()
	if running != nil || loginErr == nil || !strings.Contains(loginErr.Error(), "login panicked") {
		t.Fatalf("login under way %v, login error %v; want none and the panic", running, loginErr)
	}
	if _, _, err := tv.login(context.Background()); !errors.Is(err, errTVDBLogin) || n.count(v4+"/login") != 0 {
		t.Fatalf("in the login pause: %v, %d logins; want the failed login and none sent", err, n.count(v4+"/login"))
	}
	tv.mu.Lock()
	tv.loginRetry = time.Time{} // the pause is over
	tv.mu.Unlock()
	if token, _, err := tv.login(context.Background()); token != "t" || err != nil {
		t.Errorf("after the pause: %q %v, want the token", token, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// aniDBState is what the fair use rules hold about anime aid.
type aniDBState struct {
	calls, failures int
	backoff         bool // RetryAt is ahead
	calling         *anidbCall
	blocked         bool // in its 24 h block
}

func (c *AniDB) stateOf(aid int) aniDBState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return aniDBState{len(c.state.Sent), c.state.Failures, time.Now().Before(c.state.RetryAt), c.calling[aid], !c.state.Failed[aid].IsZero()}
}

// endBackoff lets the next call out, as once the backoff is over.
func (c *AniDB) endBackoff() {
	c.mu.Lock()
	c.state.RetryAt = time.Time{}
	c.mu.Unlock()
}

// A panic in an AniDB call's download (here its transport; it could come
// after AniDB answered) is handled as a network error is: the call isn't
// counted, the anime isn't blocked, the backoff steps up. The process goes
// on, and the next call goes out once the backoff allows it.
func TestAniDBCallPanicIsANetworkFailure(t *testing.T) {
	c, requests := fakeAniDB(t, [2]string{"200", anidbSample})
	_ = captureLogs(t)
	var panicked int
	c.f.SetTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		panicked++
		panic("boom")
	}))
	ctx := context.Background()
	if _, err := c.Anime(ctx, 1); !errors.Is(err, fetch.ErrPanicked) {
		t.Fatalf("err %v, want the panic", err)
	}
	if st := c.stateOf(1); st.calls != 0 || st.failures != 1 || !st.backoff || st.calling != nil || st.blocked {
		t.Fatalf("state %+v; want no call counted, one failure and its backoff, the call ended, the anime not blocked", st)
	}
	if _, err := c.Anime(ctx, 1); !errors.Is(err, errAniDBUnavailable) || panicked != 1 {
		t.Fatalf("in the backoff: %v, %d requests; want the backoff and no request", err, panicked)
	}
	c.f.SetTransport(&http.Transport{}) // the fake AniDB server again
	c.endBackoff()
	if a, err := c.Anime(ctx, 1); err != nil || a == nil || requests.Load() != 1 {
		t.Errorf("after the backoff: %v, %d requests; want the entry, asked once", err, requests.Load())
	}
}

// A panic after AniDB's entry was accepted (here in the download's log
// line, once the entry is cached) is a network failure too: the anime is
// not held back as fetched for 24 h. The cached copy answers the next call,
// once the backoff is over, without asking AniDB.
func TestAniDBPanicAfterAnswer(t *testing.T) {
	c, requests := fakeAniDB(t, [2]string{"200", anidbSample})
	_ = panicLogs(t, "http")
	ctx := context.Background()
	if _, err := c.Anime(ctx, 1); !errors.Is(err, fetch.ErrPanicked) {
		t.Fatalf("err %v, want the panic", err)
	}
	if st := c.stateOf(1); st.calls != 0 || st.failures != 1 || !st.backoff || st.calling != nil || st.blocked || c.fetchedLately(1) {
		t.Fatalf("state %+v, fetched lately %v; want no call counted, the backoff, the call ended, the anime neither blocked nor fetched",
			st, c.fetchedLately(1))
	}
	c.endBackoff()
	if a, err := c.Anime(ctx, 1); err != nil || a == nil || requests.Load() != 1 {
		t.Errorf("after the backoff: %v, %d requests; want the cached entry, AniDB asked once", err, requests.Load())
	}
}

// A panic in an AniDB call's Before (sending, here once it marked the call
// sent: its state write fails and the log line panics) sends nothing, and
// is a network failure (the backoff). The call leaves the calls under way
// all the same, so once the backoff is over the anime is asked again under
// the usual rules. Kept, the next call would join it and be refused until a
// restart ("the last call for this anime just ended"), or, not marked sent
// yet, skip admit's checks. Its count goes back: nothing went out.
func TestAniDBSendingPanic(t *testing.T) {
	c, requests := fakeAniDB(t, [2]string{"200", anidbSample})
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	good := c.statePath
	c.mu.Lock()
	c.statePath = filepath.Join(notDir, "anidb-state.json") // every state write fails
	c.mu.Unlock()
	_ = panicLogsFrom(t, "AniDB state not saved; a restart now would forget the latest calls", "(*AniDB).sending")
	ctx := context.Background()
	if _, err := c.Anime(ctx, 1); !errors.Is(err, fetch.ErrPanicked) {
		t.Fatalf("err %v, want the panic", err)
	}
	c.mu.Lock()
	c.statePath = good
	c.mu.Unlock()
	if st := c.stateOf(1); st.calling != nil || st.calls != 0 || st.failures != 1 || !st.backoff || st.blocked || requests.Load() != 0 {
		t.Fatalf("state %+v, %d requests; want the call ended and not counted, the backoff, the anime not blocked, nothing sent",
			st, requests.Load())
	}
	if _, err := c.Anime(ctx, 1); !errors.Is(err, errAniDBUnavailable) || requests.Load() != 0 {
		t.Fatalf("in the backoff: %v, %d requests; want the backoff and no request", err, requests.Load())
	}
	c.endBackoff()
	if a, err := c.Anime(ctx, 1); err != nil || a == nil || requests.Load() != 1 {
		t.Errorf("after the backoff: %v, %d requests; want the entry, asked once", err, requests.Load())
	}
}

// The same, for a call sending admits itself (no call was under way when
// the download's turn came) and that panics before it is marked sent: in
// admit's debug line. The download is driven as ask drives it, without
// allowed, so sending is the one that admits the call.
func TestAniDBSendingPanicBeforeSent(t *testing.T) {
	c, requests := fakeAniDB(t, [2]string{"200", anidbSample})
	_ = panicLogsFrom(t, "anidb: calling the API", "(*AniDB).sending")
	var call *anidbCall
	_, err := c.f.Get(context.Background(), fetch.Request{URL: fmt.Sprintf(anidbAPI, c.client, c.clientVer, 1), Key: anidbKey(1), NoRedirect: true,
		Before:  func() (err error) { call, err = c.sending(1, false); return err },
		Done:    func(err error) { c.result(1, call, err) },
		NoStale: true, Quiet: true})
	if !errors.Is(err, fetch.ErrPanicked) {
		t.Fatalf("err %v, want the panic", err)
	}
	if st := c.stateOf(1); st.calling != nil || st.calls != 0 || st.failures != 1 || st.blocked || requests.Load() != 0 {
		t.Fatalf("state %+v, %d requests; want the call ended and not counted, one failure, the anime not blocked, nothing sent",
			st, requests.Load())
	}
}
