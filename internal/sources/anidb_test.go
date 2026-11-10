package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

const anidbSample = `<?xml version="1.0" encoding="UTF-8"?>
<anime id="69" restricted="false">
  <type>TV Series</type><startdate>1999-10-20</startdate><enddate>2000-03-01</enddate>
  <titles>
    <title xml:lang="x-jat" type="main">One Piece</title>
    <title xml:lang="en" type="official">One Piece EN</title>
    <title xml:lang="ja" type="official">ワンピース</title>
    <title xml:lang="en" type="short">OP</title>
  </titles>
  <description>Pirates. http://anidb.net/ch123 [Luffy] sails.
Source: somewhere</description>
  <picture>123.jpg</picture>
  <tags><tag infobox="true"><name>shounen</name></tag><tag><name>hidden</name></tag></tags>
  <resources><resource type="2"><externalentity><identifier>21</identifier></externalentity></resource></resources>
  <episodes>
    <episode><epno type="1">2</epno><length>24</length><title xml:lang="en">Second</title></episode>
    <episode><epno type="1">1</epno><length>24</length><title xml:lang="en">First</title></episode>
    <episode><epno type="2">S1</epno><title xml:lang="en">Special</title></episode>
    <episode><epno type="3">C1</epno><title xml:lang="en">Opening</title></episode>
    <episode><epno type="3">C2</epno><title xml:lang="en">Opening 2</title></episode>
    <episode><epno type="3">C3</epno><title xml:lang="en">Ending</title></episode>
    <episode><epno type="4">T1</epno><title xml:lang="en">Trailer</title></episode>
  </episodes>
</anime>`

func TestParseAniDBNumbering(t *testing.T) {
	a, err := parseAniDB([]byte(anidbSample))
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]int{{0, 1}, {0, 101}, {0, 102}, {0, 151}, {0, 201}, {1, 1}, {1, 2}}
	if len(a.Episodes) != len(want) {
		t.Fatalf("got %d episodes, want %d", len(a.Episodes), len(want))
	}
	for i, w := range want {
		if got := [2]int{a.Episodes[i].Season, a.Episodes[i].Index}; got != w {
			t.Errorf("episode %d (%s): got S%dE%d, want S%dE%d", i, a.Episodes[i].EpNo, got[0], got[1], w[0], w[1])
		}
	}
	if a.Description != "Pirates. Luffy sails." {
		t.Errorf("description %q", a.Description)
	}
	if len(a.Genres) != 1 || a.Genres[0] != "Shounen" || len(a.MALIDs) != 1 || a.MALIDs[0] != 21 {
		t.Errorf("genres %v mal %v", a.Genres, a.MALIDs)
	}
}

// fakeAniDB answers every request with the next scripted (status, body).
// Every test that uses it asks one anime at a time.
func fakeAniDB(t *testing.T, answers ...[2]string) (*AniDB, *atomic.Int32) {
	var next atomic.Int32
	return fakeAniDBServer(t, func(int) [2]string {
		return answers[min(int(next.Add(1))-1, len(answers)-1)]
	})
}

// fakeAniDBServer answers each request with answer(aid), and counts them.
func fakeAniDBServer(t *testing.T, answer func(aid int) [2]string) (*AniDB, *atomic.Int32) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		aid, _ := strconv.Atoi(r.URL.Query().Get("aid"))
		a := answer(aid)
		if a[0] == "close" { // the connection drops after the request is read
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		code, _ := strconv.Atoi(a[0])
		w.WriteHeader(code)
		_, _ = w.Write([]byte(a[1]))
	}))
	t.Cleanup(srv.Close)
	old := anidbAPI
	anidbAPI = srv.URL + "/?client=%s&clientver=%s&aid=%d"
	t.Cleanup(func() { anidbAPI = old })
	dir := t.TempDir()
	c := NewAniDB(fetch.New(dir), dir, "test", "1")
	c.f.Throttle(srv.Listener.Addr().String(), 0) // no 4 s gap unless a test sets one
	return c, &calls
}

func TestAniDBEntryErrorNotRetried(t *testing.T) {
	c, calls := fakeAniDB(t, [2]string{"200", "<error>aid Missing or Invalid</error>"}, [2]string{"200", anidbSample})
	ctx := context.Background()
	if _, err := c.Anime(ctx, 1); err == nil {
		t.Fatal("want error")
	}
	if _, err := c.Anime(ctx, 1); err == nil || calls.Load() != 1 {
		t.Fatalf("same anime asked again within 24 h: calls=%d err=%v", calls.Load(), err)
	}
	if a, err := c.Anime(ctx, 69); err != nil || a == nil || calls.Load() != 2 {
		t.Fatalf("another anime must still be allowed: calls=%d err=%v", calls.Load(), err)
	}
}

func TestAniDBBackoff(t *testing.T) {
	c, calls := fakeAniDB(t, [2]string{"500", "oops"}, [2]string{"200", anidbSample})
	ctx := context.Background()
	_, _ = c.Anime(ctx, 1)
	if _, err := c.Anime(ctx, 2); !errors.Is(err, ErrAniDBUnavailable) || calls.Load() != 1 {
		t.Fatalf("backoff must stop all calls: calls=%d err=%v", calls.Load(), err)
	}
	if w := time.Until(c.state.RetryAt); w < 25*time.Second || w > 30*time.Second {
		t.Errorf("first backoff step %v, want 30 s", w)
	}
	c.state.RetryAt = time.Time{} // backoff over
	if _, err := c.Anime(ctx, 2); err != nil || c.state.Failures != 0 {
		t.Fatalf("success must reset the backoff: failures=%d err=%v", c.state.Failures, err)
	}
}

// A state file that exists but doesn't parse (a write cut off by a crash or a
// full disk, under the old non-atomic save) may have held a ban or a full
// day of calls: AniDB isn't asked until 24 h after it was last written, and
// that pause survives the next save and restart.
func TestAniDBUnreadableStateWaits(t *testing.T) {
	for _, age := range []time.Duration{0, 20 * time.Hour, 30 * time.Hour} {
		dir := t.TempDir()
		path := filepath.Join(dir, "anidb-state.json")
		if err := os.WriteFile(path, []byte(`{"banned_until":"2026-11-0`), 0o644); err != nil {
			t.Fatal(err)
		}
		written := time.Now().Add(-age)
		if err := os.Chtimes(path, written, written); err != nil {
			t.Fatal(err)
		}
		c := NewAniDB(fetch.New(dir), dir, "test", "1")
		want := written.Add(24 * time.Hour)
		if d := c.state.RetryAt.Sub(want); d < -time.Second || d > time.Second {
			t.Errorf("written %v ago: retry at %v, want %v", age, c.state.RetryAt, want)
		}
		ok, why := c.allowed(1, false)
		if ok != (age >= 24*time.Hour) {
			t.Errorf("written %v ago: allowed %v (%s)", age, ok, why)
		}
		if age >= 24*time.Hour {
			continue
		}
		c.mu.Lock()
		c.save()
		c.mu.Unlock()
		if again := NewAniDB(fetch.New(dir), dir, "test", "1"); !again.state.RetryAt.Equal(c.state.RetryAt) {
			t.Errorf("written %v ago: after a save and restart, retry at %v, want %v", age, again.state.RetryAt, c.state.RetryAt)
		}
	}
	// No state file yet (first start): nothing to wait for.
	dir := t.TempDir()
	if ok, why := NewAniDB(fetch.New(dir), dir, "test", "1").allowed(1, false); !ok {
		t.Errorf("first start: not allowed (%s)", why)
	}
}

// A 503, or a 200 whose body is AniDB's <error>Banned</error>, is a ban.
func TestAniDBBanAnswers(t *testing.T) {
	for _, answer := range [][2]string{{"503", "Banned"}, {"200", "<error>Banned</error>"}} {
		c, calls := fakeAniDB(t, answer)
		_, _ = c.Anime(context.Background(), 1)
		if time.Until(c.state.BannedUntil) < 23*time.Hour {
			t.Fatalf("%v must pause AniDB for 24 h, banned until %v", answer, c.state.BannedUntil)
		}
		_, _ = c.Anime(context.Background(), 2)
		if calls.Load() != 1 {
			t.Fatalf("%v: calls during a ban: %d", answer, calls.Load())
		}
	}
}

// An error about the client (its name or version) pauses every call, not
// just this anime, says once which settings to check, and survives a
// restart with the same settings, not one with new ones.
func TestAniDBClientRejectedPausesAll(t *testing.T) {
	logs := captureLogs(t)
	c, calls := fakeAniDB(t, [2]string{"200", "<error>Client Values Missing or Invalid</error>"}, [2]string{"200", anidbSample})
	ctx := context.Background()
	for aid := 1; aid <= 3; aid++ {
		if _, err := c.Anime(ctx, aid); err == nil {
			t.Fatalf("anime %d: want an error", aid)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d calls, want 1: the client error stops every call", calls.Load())
	}
	if _, failed := c.state.Failed[1]; failed {
		t.Error("anime 1 marked failed: the error was about the client")
	}
	if len(c.failWhy) != 0 { // pruned only with a Failed entry
		t.Errorf("failure reasons kept for a client error: %v", c.failWhy)
	}
	if s := logs.String(); strings.Count(s, "level=ERROR") != 1 || !strings.Contains(s, "ANIDB_CLIENTVER") {
		t.Errorf("want one error naming the settings:\n%s", s)
	}
	dir := filepath.Dir(c.statePath)
	if ok, _ := NewAniDB(fetch.New(dir), dir, "test", "1").allowed(4, false); ok {
		t.Error("restart with the same client: allowed")
	}
	if ok, why := NewAniDB(fetch.New(dir), dir, "test", "2").allowed(4, false); !ok {
		t.Errorf("restart with a new client version: %s", why)
	}
}

// A stale cached entry is parsed once and then kept in memory, by Peek and
// Anime alike, until its cache file changes.
func TestAniDBStaleEntryParsedOnce(t *testing.T) {
	c, calls := fakeAniDB(t, [2]string{"200", anidbSample})
	c.state.BannedUntil = time.Now().Add(time.Hour) // AniDB can't be asked: the stale copy is all there is
	path := filepath.Join(filepath.Dir(c.statePath), "anidb", "anime", "69.xml")
	write := func(age time.Duration) {
		if err := fetch.WriteFile(path, []byte(anidbSample)); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write(200 * 24 * time.Hour) // the entry ended in 2000: 180 days fresh
	first, fresh := c.Peek(69)
	if first == nil || fresh {
		t.Fatalf("peek: %v fresh=%v, want the stale entry", first, fresh)
	}
	if again, fresh := c.Peek(69); again != first || fresh {
		t.Fatal("peek again: parsed again, want the kept copy")
	}
	if a, err := c.Anime(context.Background(), 69); err != nil || a != first || calls.Load() != 0 {
		t.Fatalf("anime while AniDB is paused: %v, %d calls, same copy %v", err, calls.Load(), a == first)
	}
	write(190 * 24 * time.Hour) // a new copy (still stale)
	if newer, _ := c.Peek(69); newer == nil || newer == first {
		t.Fatal("cache file changed: want it parsed again")
	}
}

// A failed call answered with the stale copy says so in the match report.
func TestAniDBFailedCallReportsStale(t *testing.T) {
	c, _ := fakeAniDB(t, [2]string{"500", "oops"})
	path := filepath.Join(filepath.Dir(c.statePath), "anidb", "anime", "69.xml")
	if err := fetch.WriteFile(path, []byte(anidbSample)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-200 * 24 * time.Hour) // the entry ended in 2000: 180 days fresh
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	var events []fetch.Event
	ctx := fetch.WithObserver(context.Background(), func(e fetch.Event) { events = append(events, e) })
	if a, err := c.Anime(ctx, 69); err != nil || a == nil {
		t.Fatalf("want the stale copy: %v %v", a, err)
	}
	if n := len(events); n == 0 || events[n-1].Kind != fetch.KindStale || events[n-1].Err == nil {
		t.Fatalf("events %+v, want the last one stale, with the call's error", events)
	}
}

// admissions sends the aid of each AniDB call allowed (its debug line), so
// a test knows a call is past allowed and on its way to the 4 s queue.
type admissions chan int

func (a admissions) Enabled(context.Context, slog.Level) bool { return true }
func (a admissions) WithAttrs([]slog.Attr) slog.Handler       { return a }
func (a admissions) WithGroup(string) slog.Handler            { return a }
func (a admissions) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "anidb: calling the API" {
		r.Attrs(func(at slog.Attr) bool {
			if at.Key == "aid" {
				a <- int(at.Value.Int64())
			}
			return true
		})
	}
	return nil
}

// A slow state write (an fsync on a spun-down disk, here c.mu held) delays
// the call let through first; the one queued behind it still reaches AniDB
// at least the gap after it, not together with it. Only a lower bound is
// checked, at the server.
func TestAniDBGapCountsFromSend(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	c, _ := fakeAniDBServer(t, func(aid int) [2]string {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		return [2]string{"200", strings.Replace(anidbSample, `id="69"`, `id="`+strconv.Itoa(aid)+`"`, 1)}
	})
	u, err := url.Parse(anidbAPI)
	if err != nil {
		t.Fatal(err)
	}
	const gap = 100 * time.Millisecond
	c.f.ThrottleFrom(u.Host, time.Hour, time.Now()) // both queue up

	var wg sync.WaitGroup
	for aid := 1; aid <= 2; aid++ {
		wg.Go(func() { _, _ = c.Anime(context.Background(), aid) })
	}
	for fg, _ := c.f.Waiting(u.Host); fg < 2; fg, _ = c.f.Waiting(u.Host) {
		time.Sleep(time.Millisecond)
	}
	c.mu.Lock() // the slow write
	c.f.ThrottleFrom(u.Host, gap, time.Now())
	time.Sleep(4 * gap)
	c.mu.Unlock()
	wg.Wait()
	if len(at) != 2 {
		t.Fatalf("%d requests, want 2", len(at))
	}
	if d := at[1].Sub(at[0]); d < gap {
		t.Errorf("requests reached AniDB %v apart, want at least %v", d, gap)
	}
}

// A call waiting in the 4 s queue when the answer ahead of it brings a ban
// or a backoff is not sent when its turn comes: no request, its count given
// back, the anime not marked failed, the backoff not stepped up again.
// (The call ahead stays counted after a ban, not after a 500.)
func TestAniDBQueuedCallsStopAfterBan(t *testing.T) {
	for first, wantSent := range map[[2]string]int{{"200", "<error>Banned</error>"}: 1, {"503", "Banned"}: 1, {"500", "oops"}: 0} {
		out, hold := make(chan struct{}), make(chan struct{})
		c, calls := fakeAniDBServer(t, func(aid int) [2]string {
			if aid != 1 {
				return [2]string{"200", anidbSample}
			}
			close(out)
			<-hold
			return first
		})
		u, err := url.Parse(anidbAPI)
		if err != nil {
			t.Fatal(err)
		}
		c.f.Throttle(u.Host, time.Hour) // the second call waits until the test opens the gap

		ctx := context.Background()
		firstDone := make(chan struct{})
		go func() { _, _ = c.Anime(ctx, 1); close(firstDone) }() // goes out at once; its answer is held

		<-out // on the server: the second call queues behind it
		admitted := make(admissions, 4)
		old := slog.Default()
		slog.SetDefault(slog.New(admitted))
		secondErr := make(chan error, 1)
		go func() { _, err := c.Anime(ctx, 2); secondErr <- err }()
		for <-admitted != 2 { // allowed and counted before the answer ahead of it
		}
		close(hold)
		<-firstDone
		c.f.Throttle(u.Host, 0) // its turn
		err = <-secondErr
		slog.SetDefault(old)

		if !errors.Is(err, ErrAniDBUnavailable) || calls.Load() != 1 {
			t.Fatalf("after %v: second call err %v, %d requests, want it not sent", first, err, calls.Load())
		}
		c.mu.Lock()
		sent, failures, calling, failed := len(c.state.Sent), c.state.Failures, c.calling[2], c.state.Failed[2]
		c.mu.Unlock()
		if sent != wantSent || !failed.IsZero() || failures > 1 || calling != nil {
			t.Fatalf("after %v: %d calls counted, anime 2 failed at %v, %d failures, still calling %v; want %d, never, at most 1, no",
				first, sent, failed, failures, calling, wantSent)
		}

		c.mu.Lock()
		c.state.BannedUntil, c.state.RetryAt = time.Time{}, time.Time{} // the pause is over
		c.mu.Unlock()
		if a, err := c.Anime(ctx, 2); err != nil || a == nil || calls.Load() != 2 {
			t.Fatalf("after the pause, anime 2: %v, %d requests", err, calls.Load())
		}
	}
}

// Background fetches run at most anidbPrefetchWorkers at a time, the rest
// wait their turn in order, and every one is fetched in the end.
func TestAniDBPrefetchWorkers(t *testing.T) {
	hold := make(chan struct{})
	arrived := make(chan struct{}, 8)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-hold
		calls.Add(1)
		aid := r.URL.Query().Get("aid")
		_, _ = w.Write([]byte(strings.Replace(anidbSample, `id="69"`, `id="`+aid+`"`, 1)))
	}))
	t.Cleanup(srv.Close)
	old := anidbAPI
	anidbAPI = srv.URL + "/?client=%s&clientver=%s&aid=%d"
	t.Cleanup(func() { anidbAPI = old })
	dir := t.TempDir()
	c := NewAniDB(fetch.New(dir), dir, "test", "1")
	c.f.Throttle(srv.Listener.Addr().String(), 0)

	aids := []int{1, 2, 3, 4, 5}
	for _, aid := range aids {
		c.Prefetch(aid)
	}
	c.Prefetch(3) // already waiting: not twice
	for range anidbPrefetchWorkers {
		<-arrived
	}
	c.mu.Lock()
	workers, pending := c.workers, slices.Clone(c.pending)
	c.mu.Unlock()
	if workers != anidbPrefetchWorkers || !slices.Equal(pending, aids[anidbPrefetchWorkers:]) {
		t.Fatalf("%d workers, waiting %v; want %d, %v", workers, pending, anidbPrefetchWorkers, aids[anidbPrefetchWorkers:])
	}
	close(hold)
	for range len(aids) - anidbPrefetchWorkers {
		<-arrived
	}
	// Each joins its download, or reads the cache once it ended (a download
	// gone from fetch before its call ended is not asked again).
	for _, aid := range aids {
		if a, err := c.Anime(context.Background(), aid); err != nil || a.AID != aid {
			t.Fatalf("anime %d: %v %v", aid, a, err)
		}
	}
	c.WaitIdle()
	if int(calls.Load()) != len(aids) {
		t.Fatalf("%d calls, want %d", calls.Load(), len(aids))
	}
}

// Calls already out when the first failure starts a backoff fail too (a
// dead server, 4 s apart): one backoff step, not one per call.
func TestAniDBOverlappingFailuresOneStep(t *testing.T) {
	c, _ := fakeAniDB(t, [2]string{"500", "oops"})
	calls := []*anidbCall{}
	for aid := 1; aid <= 3; aid++ {
		if ok, why := c.allowed(aid, false); !ok {
			t.Fatal(why)
		}
		call, err := c.sending(aid, false) // all three out before any answer
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	failed := &fetch.StatusError{Code: 500, URL: "x"}
	for i, call := range calls {
		c.result(i+1, call, failed)
	}
	if c.state.Failures != 1 || len(c.state.Failed) != 0 || len(c.state.Sent) != 0 {
		t.Fatalf("%d failures (want 1), %d anime blocked (want 0: a server error is no anime's fault), %d calls counted (want 0)",
			c.state.Failures, len(c.state.Failed), len(c.state.Sent))
	}
}

func TestAniDBExternalIDs(t *testing.T) {
	a, err := parseAniDB([]byte(`<anime id="23"><titles><title xml:lang="x-jat" type="main">Cowboy Bebop</title></titles>
<resources><resource type="1"><externalentity><identifier>13</identifier></externalentity></resource>
<resource type="2"><externalentity><identifier>1</identifier></externalentity></resource></resources></anime>`))
	if err != nil || len(a.ANNIDs) != 1 || a.ANNIDs[0] != 13 || len(a.MALIDs) != 1 || a.MALIDs[0] != 1 {
		t.Fatalf("ann %v mal %v err %v", a.ANNIDs, a.MALIDs, err)
	}
}

// logBuffer collects log lines (the fetch goroutine logs too).
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureLogs sends info and above to the returned buffer until the test ends.
func captureLogs(t *testing.T) *logBuffer {
	logs := &logBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return logs
}

// An anime AniDB answered with an error is blocked for 24 h: the failure
// says so, and the first skip afterwards is logged at info, once, with when
// and why.
func TestAniDBBlockedAnimeIsLoggedOnce(t *testing.T) {
	logs := captureLogs(t)
	c, calls := fakeAniDB(t, [2]string{"200", "<error>Anime not found</error>"})
	ctx := context.Background()
	_, _ = c.Anime(ctx, 1)
	if s := logs.String(); !strings.Contains(s, "retry_anime_at=") {
		t.Fatalf("failure warning lacks retry_anime_at:\n%s", s)
	}
	for range 3 { // rebuilds of the same show
		if _, err := c.Anime(ctx, 1); !errors.Is(err, ErrAniDBUnavailable) {
			t.Fatalf("blocked anime: err %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("blocked anime was asked again: %d calls", calls.Load())
	}
	s := logs.String()
	if n := strings.Count(s, "level=INFO"); n != 1 {
		t.Fatalf("want one info line about the block, got %d:\n%s", n, s)
	}
	for _, want := range []string{"aid=1", "blocked_until=", "Anime not found"} {
		if !strings.Contains(s, want) {
			t.Errorf("info line lacks %q:\n%s", want, s)
		}
	}
}

// BlockedUntil tells an anime in its 24 h block after AniDB answered with
// an error from one AniDB can't be asked about for a shorter while (a
// backoff, the daily limit), and ends with the block.
func TestAniDBBlockedUntil(t *testing.T) {
	var none *AniDB
	if _, blocked := none.BlockedUntil(1); blocked {
		t.Error("nil AniDB: want not blocked")
	}
	c, _ := fakeAniDB(t, [2]string{"200", "<error>Anime not found</error>"})
	if _, blocked := c.BlockedUntil(1); blocked {
		t.Error("never asked: want not blocked")
	}
	_, _ = c.Anime(context.Background(), 1) // AniDB answers with an error: aid 1 blocked
	c.mu.Lock()
	c.state.RetryAt = time.Now().Add(time.Hour) // and every other anime under a backoff
	failedAt := c.state.Failed[1]
	c.mu.Unlock()
	if until, blocked := c.BlockedUntil(1); !blocked || !until.Equal(failedAt.Add(anidbRetry)) {
		t.Errorf("failed anime: blocked %v until %v, want true until %v", blocked, until, failedAt.Add(anidbRetry))
	}
	if _, blocked := c.BlockedUntil(2); blocked {
		t.Error("another anime under the backoff: want not blocked")
	}
	c.mu.Lock()
	c.state.Failed[1] = time.Now().Add(-anidbRetry) // the block just ended
	c.mu.Unlock()
	if _, blocked := c.BlockedUntil(1); blocked {
		t.Error("block over: want not blocked")
	}
}

// Callers that ask for the same anime at once join one download, and each
// gets the entry: the one that started it reuses validate's parse, the
// others parse it themselves (CI runs this with -race).
func TestAniDBConcurrentCallers(t *testing.T) {
	c, _ := fakeAniDB(t, [2]string{"200", anidbSample})
	var wg sync.WaitGroup
	got := make([]*AniDBAnime, 4)
	for i := range got {
		wg.Go(func() { got[i], _ = c.Anime(context.Background(), 69) })
	}
	wg.Wait()
	for i, a := range got {
		if a == nil || a.AID != 69 || len(a.Episodes) != 7 {
			t.Fatalf("caller %d: %+v", i, a)
		}
	}
}

// A caller that looked for the cache file while another caller's call was
// out, and only asks once that call ended (its download gone from fetch),
// gets the entry that call stored: AniDB is never asked twice.
func TestAniDBNoSecondCallAfterAnotherStored(t *testing.T) {
	c, calls := fakeAniDB(t, [2]string{"200", anidbSample})
	ctx := context.Background()
	if a, err := c.Anime(ctx, 69); err != nil || a == nil {
		t.Fatalf("first call: %v %v", a, err)
	}
	// local found nothing (before the first call stored it); it asks now.
	if a, err := c.ask(ctx, 69, false, nil, time.Now()); err != nil || a == nil || a.AID != 69 || calls.Load() != 1 {
		t.Fatalf("second caller: %v %v, %d requests, want the stored entry and 1", a, err, calls.Load())
	}
	// A caller that joined the call (allowed's shortcut) but whose own
	// download started after it ended: its turn in the queue is refused.
	var notSent *anidbNotSent
	if _, err := c.sending(69, false); !errors.As(err, &notSent) {
		t.Fatalf("new call right after a fetch: %v, want it not sent", err)
	}
	if len(c.state.Sent) != 1 || c.calling[69] != nil {
		t.Fatalf("%d calls counted, calling %v; want 1, none", len(c.state.Sent), c.calling[69])
	}
}

// A download whose turn comes after the anime's last call left fetch but
// before its outcome was recorded (a failure, say, slower than the 4 s
// gap) is not sent: the same anime would be asked twice.
func TestAniDBNoCallBeforeLastOneRecorded(t *testing.T) {
	c, _ := fakeAniDB(t, [2]string{"500", "oops"})
	if ok, why := c.allowed(5, false); !ok {
		t.Fatal(why)
	}
	call, err := c.sending(5, false) // out; its answer is in, its result not run yet
	if err != nil {
		t.Fatal(err)
	}
	var notSent *anidbNotSent
	if _, err := c.sending(5, false); !errors.As(err, &notSent) || len(c.state.Sent) != 1 {
		t.Fatalf("second download's turn: %v, %d calls counted; want not sent, 1", err, len(c.state.Sent))
	}
	c.result(5, nil, notSent)
	c.result(5, call, &anidbEntryError{msg: "Anime not found", answered: true})
	if _, failed := c.state.Failed[5]; !failed || c.calling[5] != nil {
		t.Fatalf("anime 5 failed %v, calling %v; want failed, none", failed, c.calling[5])
	}
}

// An answer whose cache write fails is not asked again within 24 h, also
// after its copy in memory expired, and after a restart.
func TestAniDBFailedCacheWriteNotAskedAgain(t *testing.T) {
	c, calls := fakeAniDB(t, [2]string{"200", anidbSample})
	dir := filepath.Dir(c.statePath)
	// A folder where the file goes: the write fails.
	if err := os.MkdirAll(filepath.Join(dir, "anidb", "anime", "69.xml", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if a, err := c.Anime(ctx, 69); err != nil || a == nil {
		t.Fatalf("first call: %v %v", a, err)
	}
	c.memoMu.Lock()
	clear(c.memo) // an hour later
	c.memoMu.Unlock()
	if _, err := c.Anime(ctx, 69); !errors.Is(err, ErrAniDBUnavailable) || calls.Load() != 1 {
		t.Fatalf("after the memo expired: %v, %d requests; want unavailable and 1", err, calls.Load())
	}
	if ok, _ := NewAniDB(fetch.New(dir), dir, "test", "1").allowed(69, false); ok {
		t.Error("after a restart: allowed")
	}
}

func TestAniDBEpisodeListIndex(t *testing.T) {
	for _, c := range []struct {
		ep   AniDBEpisode
		want int
	}{
		{AniDBEpisode{Season: 1, Index: 5, EpNo: "5"}, 5},
		{AniDBEpisode{Season: 0, Index: 2, EpNo: "S2"}, 2},
		{AniDBEpisode{Season: 0, Index: 101, EpNo: "C1"}, 101},
		{AniDBEpisode{Season: 0, Index: 151, EpNo: "C3"}, 103}, // an ending: 151 here, C3 = 103 on Anime-Lists
		{AniDBEpisode{Season: 0, Index: 201, EpNo: "T1"}, 201},
		{AniDBEpisode{Season: 0, Index: 401, EpNo: "O1"}, 401},
	} {
		if got := c.ep.ListIndex(); got != c.want {
			t.Errorf("%s: ListIndex %d, want %d", c.ep.EpNo, got, c.want)
		}
	}
}

// An answer about the anime (an <error> entry) shows AniDB is reachable: the
// failures in a row before it are over, so the next outage starts again at
// the first backoff step.
func TestAniDBEntryAnswerEndsFailureStreak(t *testing.T) {
	c, _ := fakeAniDB(t, [2]string{"200", "<error>Anime not found</error>"})
	c.state.Failures = 3 // an outage, its backoff over
	if _, err := c.Anime(context.Background(), 1); err == nil {
		t.Fatal("want an error")
	}
	if c.state.Failures != 0 {
		t.Errorf("%d failures in a row after AniDB answered, want 0", c.state.Failures)
	}
}

// An answer that only looks like an entry (XML that doesn't parse: cut off,
// or another server's page) doesn't show AniDB is back: the streak goes on.
// The anime still waits its 24 h.
func TestAniDBUnreadableAnswerKeepsFailureStreak(t *testing.T) {
	c, _ := fakeAniDB(t, [2]string{"200", `<anime id="1"><titles><title>cut off`})
	c.state.Failures = 3
	if _, err := c.Anime(context.Background(), 1); err == nil {
		t.Fatal("want an error")
	}
	if c.state.Failures != 3 || c.state.Failed[1].IsZero() {
		t.Errorf("%d failures in a row, anime failed at %v; want 3, now", c.state.Failures, c.state.Failed[1])
	}
}

// A caller that came in after sending stopped a call, and joined that
// call's download, leaves a call counted that nothing sends: the stopped
// download's outcome gives it back. One already sent stays.
func TestAniDBNotSentGivesBackLeftoverCall(t *testing.T) {
	c, _ := fakeAniDB(t, [2]string{"200", anidbSample})
	c.mu.Lock()
	if why := c.admit(1, false, time.Now()); why != "" {
		t.Fatal(why)
	}
	c.mu.Unlock()
	c.result(1, nil, &anidbNotSent{"stopping"})
	c.mu.Lock()
	sent, calling := len(c.state.Sent), len(c.calling)
	c.mu.Unlock()
	if sent != 0 || calling != 0 {
		t.Fatalf("%d calls counted, %d calling; want 0, 0", sent, calling)
	}

	c.mu.Lock()
	if why := c.admit(2, false, time.Now()); why != "" {
		t.Fatal(why)
	}
	c.calling[2].sent = true
	c.mu.Unlock()
	c.result(2, nil, &anidbNotSent{"the last call for this anime just ended"})
	c.mu.Lock()
	sent, kept := len(c.state.Sent), c.calling[2] != nil
	c.mu.Unlock()
	if sent != 1 || !kept {
		t.Fatalf("a sent call: %d counted, still calling %v; want 1, true", sent, kept)
	}
}

// A restart keeps the 4 s gap from the last call the earlier run sent
// (state.Sent): the first call after it waits for the rest of the gap.
func TestAniDBGapSurvivesRestart(t *testing.T) {
	arrived := make(chan time.Time, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- time.Now()
		_, _ = w.Write([]byte(anidbSample))
	}))
	t.Cleanup(srv.Close)
	old := anidbAPI
	anidbAPI = srv.URL + "/?client=%s&clientver=%s&aid=%d"
	t.Cleanup(func() { anidbAPI = old })

	dir := t.TempDir()
	last := time.Now().Round(0).Add(-2 * time.Second) // the earlier run's last call, 2 s ago
	state, err := json.Marshal(map[string]any{"sent": []time.Time{last, last.Add(-time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "anidb-state.json"), state, 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewAniDB(fetch.New(dir), dir, "test", "1")
	if _, err := c.Anime(context.Background(), 69); err != nil {
		t.Fatal(err)
	}
	if at := <-arrived; at.Before(last.Add(anidbGap)) {
		t.Errorf("first call after the restart went out %v after the last one, want at least %v", at.Sub(last), anidbGap)
	}
}

// Once stopping, no background call goes out: the waiting prefetches are
// dropped, one queued behind the 4 s gap is stopped there with its count
// given back (the exit could cut it off before its outcome is recorded), and
// new ones aren't taken. A background call Plex joined, and Plex's own
// calls, still go out.
func TestAniDBStopEndsBackgroundCalls(t *testing.T) {
	c, calls := fakeAniDBServer(t, func(aid int) [2]string {
		return [2]string{"200", strings.Replace(anidbSample, `id="69"`, `id="`+strconv.Itoa(aid)+`"`, 1)}
	})
	u, err := url.Parse(anidbAPI)
	if err != nil {
		t.Fatal(err)
	}
	c.f.ThrottleFrom(u.Host, time.Hour, time.Now()) // every call waits until the test opens the gap

	admitted := make(admissions, 4)
	old := slog.Default()
	slog.SetDefault(slog.New(admitted))
	for aid := 1; aid <= 3; aid++ {
		c.Prefetch(aid)
	}
	for range anidbPrefetchWorkers { // 1 and 2 queued behind the gap, 3 waits for a worker
		<-admitted
	}
	joined := make(chan error, 1)
	go func() { // Plex asks for 1 too: it joins the background call
		a, err := c.Anime(context.Background(), 1)
		if err == nil && a.AID != 1 {
			err = errors.New("wrong entry")
		}
		joined <- err
	}()
	for {
		c.mu.Lock()
		fg := c.calling[1] != nil && c.calling[1].fg
		c.mu.Unlock()
		if fg {
			break
		}
		time.Sleep(time.Millisecond)
	}
	c.Stop()
	c.Prefetch(4) // after Stop: not taken
	slog.SetDefault(old)
	c.f.Throttle(u.Host, 0)

	if err := <-joined; err != nil {
		t.Fatalf("the call Plex joined: %v", err)
	}
	c.WaitIdle()
	c.mu.Lock()
	sent, calling, failed, pending, queued := len(c.state.Sent), len(c.calling), len(c.state.Failed), len(c.pending), len(c.queued)
	c.mu.Unlock()
	if calls.Load() != 1 || sent != 1 || calling != 0 || failed != 0 || pending != 0 || queued != 0 {
		t.Fatalf("%d requests, %d counted, %d calling, %d failed, %d waiting, %d queued; want 1, 1, 0, 0, 0, 0",
			calls.Load(), sent, calling, failed, pending, queued)
	}
	if a, err := c.Anime(context.Background(), 5); err != nil || a.AID != 5 || calls.Load() != 2 {
		t.Fatalf("Plex's own call while stopping: %v, %d requests", err, calls.Load())
	}
}

// A failure without AniDB's answer about the anime (here a 500) is no
// anime's fault: no anime waits 24 h, the backoff holds every call, and
// once it ends the same anime is asked again.
func TestAniDBOutageBlocksNoAnime(t *testing.T) {
	c, calls := fakeAniDB(t, [2]string{"500", "oops"}, [2]string{"200", anidbSample})
	ctx := context.Background()
	if _, err := c.Anime(ctx, 1); err == nil {
		t.Fatal("want an error")
	}
	if _, blocked := c.BlockedUntil(1); blocked || c.state.Failures != 1 {
		t.Fatalf("after a 500: blocked %v, %d failures; want false, 1", blocked, c.state.Failures)
	}
	if _, err := c.Anime(ctx, 1); !errors.Is(err, ErrAniDBUnavailable) || calls.Load() != 1 {
		t.Fatalf("during the backoff: calls=%d err=%v; want 1, unavailable", calls.Load(), err)
	}
	c.mu.Lock()
	c.state.RetryAt = time.Time{} // backoff over
	c.mu.Unlock()
	if a, err := c.Anime(ctx, 1); err != nil || a == nil || calls.Load() != 2 {
		t.Fatalf("after the backoff: calls=%d err=%v; want the anime asked again", calls.Load(), err)
	}
}

// Each kind of failed call: one without AniDB's answer about the anime
// blocks no anime and isn't one of the day's calls (only the backoff
// holds); a ban, a rejected client, an <error> about the anime and XML that
// doesn't parse stay counted, and block the anime (a rejected client blocks
// every call instead).
func TestAniDBFailureKinds(t *testing.T) {
	for name, c := range map[string]struct {
		answer  [2]string // "refused": nothing listens; "close": the connection drops after the request
		blocked bool      // the anime waits 24 h
		counted bool      // one of the day's calls
		pauses  bool      // every call waits (a backoff, a ban, a rejected client)
	}{
		"no connection":      {[2]string{"refused", ""}, false, false, true},
		"connection dropped": {[2]string{"close", ""}, false, false, true},
		"server error":       {[2]string{"500", "oops"}, false, false, true},
		"client error":       {[2]string{"404", "not here"}, false, false, true},
		"redirect":           {[2]string{"302", ""}, false, false, true},
		"broken gzip":        {[2]string{"200", "\x1f\x8b not gzip"}, false, false, true},
		"not AniDB's XML":    {[2]string{"200", "<html>maintenance</html>"}, false, false, true},
		"ban":                {[2]string{"503", "Banned"}, true, true, true},
		"banned answer":      {[2]string{"200", "<error>Banned</error>"}, true, true, true},
		"rejected client":    {[2]string{"200", "<error>Client Values Missing or Invalid</error>"}, false, true, true},
		"error answer":       {[2]string{"200", "<error>Anime not found</error>"}, true, true, false},
		"unparseable XML":    {[2]string{"200", "<anime id=\"1\"><titles>"}, true, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := fakeAniDB(t, c.answer)
			if c.answer[0] == "refused" {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr := ln.Addr().String()
				_ = ln.Close()
				anidbAPI = "http://" + addr + "/?client=%s&clientver=%s&aid=%d"
				a.f.Throttle(addr, 0)
			}
			if _, err := a.Anime(context.Background(), 1); err == nil {
				t.Fatal("want an error")
			}
			_, blocked := a.BlockedUntil(1)
			a.mu.Lock()
			sent := len(a.state.Sent)
			paused := a.paused(time.Now()) != ""
			a.mu.Unlock()
			if blocked != c.blocked || (sent == 1) != c.counted || sent > 1 || paused != c.pauses {
				t.Fatalf("blocked %v (want %v), %d calls counted (want counted %v), every call paused %v (want %v)",
					blocked, c.blocked, sent, c.counted, paused, c.pauses)
			}
		})
	}
	t.Run("answer too big", func(t *testing.T) { // too big to send here: fetch's error, straight to result
		a, _ := fakeAniDB(t)
		if ok, why := a.allowed(1, false); !ok {
			t.Fatal(why)
		}
		call, err := a.sending(1, false)
		if err != nil {
			t.Fatal(err)
		}
		a.result(1, call, errors.New("answer larger than 64 MB"))
		if _, blocked := a.BlockedUntil(1); blocked || len(a.state.Sent) != 0 || a.state.Failures != 1 {
			t.Fatalf("blocked %v, %d calls counted, %d failures; want false, 0, 1", blocked, len(a.state.Sent), a.state.Failures)
		}
	})
}
