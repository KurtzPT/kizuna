package sources

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
func fakeAniDB(t *testing.T, answers ...[2]string) (*AniDB, *int) {
	return fakeAniDBHeld(t, nil, answers...)
}

// fakeAniDBHeld is fakeAniDB whose answers wait until hold is closed (nil:
// no wait).
func fakeAniDBHeld(t *testing.T, hold <-chan struct{}, answers ...[2]string) (*AniDB, *int) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hold != nil {
			<-hold
		}
		a := answers[min(calls, len(answers)-1)]
		calls++
		code, _ := strconv.Atoi(a[0])
		w.WriteHeader(code)
		_, _ = w.Write([]byte(a[1]))
	}))
	t.Cleanup(srv.Close)
	old := anidbAPI
	anidbAPI = srv.URL + "/?client=%s&clientver=%s&aid=%d"
	t.Cleanup(func() { anidbAPI = old })
	dir := t.TempDir()
	return NewAniDB(fetch.New(dir), dir, "test", "1"), &calls
}

func TestAniDBEntryErrorNotRetried(t *testing.T) {
	c, calls := fakeAniDB(t, [2]string{"200", "<error>aid Missing or Invalid</error>"}, [2]string{"200", anidbSample})
	ctx := context.Background()
	if _, err := c.Anime(ctx, 1); err == nil {
		t.Fatal("want error")
	}
	if _, err := c.Anime(ctx, 1); err == nil || *calls != 1 {
		t.Fatalf("same anime asked again within 24 h: calls=%d err=%v", *calls, err)
	}
	if a, err := c.Anime(ctx, 69); err != nil || a == nil || *calls != 2 {
		t.Fatalf("another anime must still be allowed: calls=%d err=%v", *calls, err)
	}
}

func TestAniDBBackoff(t *testing.T) {
	c, calls := fakeAniDB(t, [2]string{"500", "oops"}, [2]string{"200", anidbSample})
	ctx := context.Background()
	_, _ = c.Anime(ctx, 1)
	if _, err := c.Anime(ctx, 2); !errors.Is(err, ErrAniDBUnavailable) || *calls != 1 {
		t.Fatalf("backoff must stop all calls: calls=%d err=%v", *calls, err)
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
		if *calls != 1 {
			t.Fatalf("%v: calls during a ban: %d", answer, *calls)
		}
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

// A call waiting in the 4 s queue when the answer ahead of it brings a ban
// or a backoff is not sent when its turn comes: no request, its count given
// back, the anime not marked failed, the backoff not stepped up again.
func TestAniDBQueuedCallsStopAfterBan(t *testing.T) {
	for _, first := range [][2]string{{"200", "<error>Banned</error>"}, {"503", "Banned"}, {"500", "oops"}} {
		hold := make(chan struct{})
		c, calls := fakeAniDBHeld(t, hold, first, [2]string{"200", anidbSample})
		u, err := url.Parse(anidbAPI)
		if err != nil {
			t.Fatal(err)
		}
		c.f.Throttle(u.Host, time.Hour) // the second call waits until the test opens the gap
		admitted := make(admissions, 4)
		old := slog.Default()
		slog.SetDefault(slog.New(admitted))

		ctx := context.Background()
		firstDone := make(chan struct{})
		go func() { _, _ = c.Anime(ctx, 1); close(firstDone) }() // goes out at once; its answer is held
		<-admitted
		secondErr := make(chan error, 1)
		go func() { _, err := c.Anime(ctx, 2); secondErr <- err }()
		<-admitted // allowed and counted before the answer ahead of it
		close(hold)
		<-firstDone
		c.f.Throttle(u.Host, 0) // its turn
		err = <-secondErr
		slog.SetDefault(old)

		if !errors.Is(err, ErrAniDBUnavailable) || *calls != 1 {
			t.Fatalf("after %v: second call err %v, %d requests, want it not sent", first, err, *calls)
		}
		c.mu.Lock()
		sent, failures, calling, failed := len(c.state.Sent), c.state.Failures, c.calling[2], c.state.Failed[2]
		c.mu.Unlock()
		if sent != 1 || !failed.IsZero() || failures > 1 || calling != nil {
			t.Fatalf("after %v: %d calls counted, anime 2 failed at %v, %d failures, still calling %v; want 1, never, at most 1, no",
				first, sent, failed, failures, calling)
		}

		c.mu.Lock()
		c.state.BannedUntil, c.state.RetryAt = time.Time{}, time.Time{} // the pause is over
		c.mu.Unlock()
		if a, err := c.Anime(ctx, 2); err != nil || a == nil || *calls != 2 {
			t.Fatalf("after the pause, anime 2: %v, %d requests", err, *calls)
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
	for _, aid := range aids { // joins each download (or reads its cache) once it ended
		if a, err := c.Anime(context.Background(), aid); err != nil || a.AID != aid {
			t.Fatalf("anime %d: %v %v", aid, a, err)
		}
	}
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
	if c.state.Failures != 1 || len(c.state.Failed) != 3 {
		t.Fatalf("%d failures (want 1), %d anime marked failed (want 3: each was asked)", c.state.Failures, len(c.state.Failed))
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

// A failed anime is blocked for 24 h: the failure says so, and the first
// skip afterwards is logged at info, once, with when and why.
func TestAniDBBlockedAnimeIsLoggedOnce(t *testing.T) {
	logs := captureLogs(t)
	c, calls := fakeAniDB(t, [2]string{"500", "oops"})
	ctx := context.Background()
	_, _ = c.Anime(ctx, 1)
	if s := logs.String(); !strings.Contains(s, "retry_anime_at=") {
		t.Fatalf("failure warning lacks retry_anime_at:\n%s", s)
	}
	c.mu.Lock()
	c.state.RetryAt = time.Time{} // the global backoff is over; the anime's block is not
	c.mu.Unlock()
	for range 3 { // rebuilds of the same show
		if _, err := c.Anime(ctx, 1); !errors.Is(err, ErrAniDBUnavailable) {
			t.Fatalf("blocked anime: err %v", err)
		}
	}
	if *calls != 1 {
		t.Fatalf("blocked anime was asked again: %d calls", *calls)
	}
	s := logs.String()
	if n := strings.Count(s, "level=INFO"); n != 1 {
		t.Fatalf("want one info line about the block, got %d:\n%s", n, s)
	}
	for _, want := range []string{"aid=1", "blocked_until=", "HTTP 500"} {
		if !strings.Contains(s, want) {
			t.Errorf("info line lacks %q:\n%s", want, s)
		}
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
