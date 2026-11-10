package plex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePlex answers the three calls the Renamer makes and notes them.
type fakePlex struct {
	shows    string            // the answer to /library/all
	extras   map[string]string // show rating key -> the answer to its /extras
	status   int               // answered instead of 200 when set
	statuses map[string]int    // the same for one call: "PUT /library/metadata/102"
	location string            // the Location header of every answer
	down     bool              // no answer at all
	calls    []string          // "GET /library/all?…", in order
}

func (f *fakePlex) RoundTrip(req *http.Request) (*http.Response, error) {
	if got := req.Header.Get("X-Plex-Token"); got != "secret" {
		return nil, fmt.Errorf("token header = %q", got)
	}
	if strings.Contains(req.URL.String(), "secret") {
		return nil, errors.New("the token is in the URL")
	}
	f.calls = append(f.calls, req.Method+" "+req.URL.RequestURI())
	if f.down {
		return nil, errors.New("connection refused")
	}
	body, status := "{}", http.StatusOK
	switch {
	case f.status != 0:
		status = f.status
	case f.statuses[req.Method+" "+req.URL.Path] != 0:
		status = f.statuses[req.Method+" "+req.URL.Path]
	case req.URL.Path == "/library/all":
		body = f.shows
	case strings.HasSuffix(req.URL.Path, "/extras"):
		body = f.extras[strings.Split(req.URL.Path, "/")[3]]
	}
	header := http.Header{}
	if f.location != "" {
		header.Set("Location", f.location)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: header, Request: req}, nil
}

// clock is the time a test sets by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// renamer returns a Renamer that talks to the fake, at a time that only
// moves with clock.add. Without Run, a test does the looks with work.
func renamer(plex http.RoundTripper) (*Renamer, *clock) {
	c := &clock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	r := New("http://plex:32400/", "secret")
	r.client.Transport = plex
	r.now = c.now
	r.after = func(time.Duration) <-chan time.Time { panic("a test without Run waits for nothing") }
	return r, c
}

// logs collects what is logged during the test, debug lines too.
func logs(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func extra(key, title, subtype, file string) string {
	return fmt.Sprintf(`{"ratingKey":%q,"title":%q,"subtype":%q,"Media":[{"Part":[{"file":%q}]}]}`, key, title, subtype, file)
}

func container(items ...string) string {
	return `{"MediaContainer":{"Metadata":[` + strings.Join(items, ",") + `]}}`
}

const (
	guid    = "tv.plex.agents.custom.kizuna.anidb://show/anidb-23"
	findAll = "GET /library/all?type=2&guid=tv.plex.agents.custom.kizuna.anidb%3A%2F%2Fshow%2Fanidb-23"
)

var bg = context.Background()

func sameCalls(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// twoLibraries is a show in two libraries, with extras of every kind.
func twoLibraries() *fakePlex {
	return &fakePlex{
		shows: container(`{"ratingKey":"100","title":"Show"}`, `{"ratingKey":"200","title":"Show"}`),
		extras: map[string]string{
			"100": container(
				extra("101", "OP01", "opening", "/media/Show/Show - OP01.mkv"),
				extra("102", "OP01a", "opening", "/media/Show/Show - OP01a.mkv"),
				extra("103", "Opening 2", "opening", "/media/Show/Show - OP02.mkv"), // already right
				extra("104", "NCED1", "ending", "/media/Show/Show - NCED1.mkv"),
				extra("105", "ED1", "ending", "/media/Show/Show - ED1.mkv"),
				extra("106", "Trailer", "trailer", "/media/Show/Trailers/OP1.mkv"), // not an opening for Plex
				`{"ratingKey":"107","title":"OP9","subtype":"opening"}`,            // no file
				extra("108", "Odd", "opening", "/media/Show/Show - Special.mkv"),   // no pattern in the name
			),
			"200": container(extra("201", "NCOP", "opening", `D:\Anime\Show\NCOP.mkv`)),
		},
	}
}

func TestRename(t *testing.T) {
	logs(t)
	plex := twoLibraries()
	r, _ := renamer(plex)
	shows, err := r.rename(bg, guid, nil)
	if err != nil {
		t.Fatal(err)
	}
	sameCalls(t, plex.calls, []string{
		findAll,
		"GET /library/metadata/100/extras",
		"PUT /library/metadata/101?title.value=Opening+1",
		"PUT /library/metadata/102?title.value=Opening+1a",
		"PUT /library/metadata/104?title.value=Ending+1+%28creditless%29",
		"PUT /library/metadata/105?title.value=Ending+1",
		"GET /library/metadata/200/extras",
		"PUT /library/metadata/201?title.value=Opening",
	})
	if want := []show{{"100", "Show"}, {"200", "Show"}}; !slices.Equal(shows, want) {
		t.Errorf("shows = %v, want %v", shows, want)
	}
}

// One title Plex refuses doesn't keep the others, or the next library, from
// theirs. The error names each call that failed.
func TestRenameGoesOn(t *testing.T) {
	logs(t)
	plex := twoLibraries()
	plex.statuses = map[string]int{"PUT /library/metadata/102": 500, "PUT /library/metadata/105": 500}
	r, _ := renamer(plex)
	_, err := r.rename(bg, guid, nil)
	if err == nil || !strings.Contains(err.Error(), "PUT /library/metadata/102: status 500") ||
		!strings.Contains(err.Error(), "PUT /library/metadata/105: status 500") {
		t.Errorf("err = %v, want both failed calls", err)
	}
	if got := len(plex.calls); got != 8 {
		t.Errorf("%d calls, want all 8:\n%s", got, strings.Join(plex.calls, "\n"))
	}

	// A library whose extras can't be read is skipped the same way.
	plex = twoLibraries()
	plex.statuses = map[string]int{"GET /library/metadata/100/extras": 500}
	r, _ = renamer(plex)
	if _, err := r.rename(bg, guid, nil); err == nil || !strings.Contains(err.Error(), "/library/metadata/100/extras: status 500") {
		t.Errorf("err = %v", err)
	}
	sameCalls(t, plex.calls, []string{findAll, "GET /library/metadata/100/extras",
		"GET /library/metadata/200/extras", "PUT /library/metadata/201?title.value=Opening"})
}

// A show Plex doesn't have, and one without extras, need no change. The
// first is worth a debug line, not a warning.
func TestRenameNothingToDo(t *testing.T) {
	out := logs(t)
	for _, plex := range []*fakePlex{
		{shows: container()},
		{shows: `{"MediaContainer":{"size":0}}`},
		{shows: container(`{"ratingKey":"100"}`), extras: map[string]string{"100": `{"MediaContainer":{"size":0}}`}},
	} {
		r, _ := renamer(plex)
		if _, err := r.rename(bg, guid, nil); err != nil {
			t.Error(err)
		}
		for _, c := range plex.calls {
			if strings.HasPrefix(c, "PUT") {
				t.Errorf("unexpected %s", c)
			}
		}
	}
	if got := out.String(); strings.Count(got, "level=DEBUG") != 2 || strings.Contains(got, "level=WARN") {
		t.Errorf("logs:\n%s\nwant a debug line for each show Plex doesn't have, and no warning", got)
	}
}

// A failure names the call and what went wrong, never the token, the
// server's address or the query (GUIDs, titles).
func TestRenameError(t *testing.T) {
	private := func(err error) bool {
		return !strings.Contains(err.Error(), "secret") && !strings.Contains(err.Error(), "plex:32400") && !strings.Contains(err.Error(), "guid=")
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		r, _ := renamer(&fakePlex{status: status})
		_, err := r.rename(bg, guid, nil)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("GET /library/all: status %d: Plex refused the token", status)) || !private(err) {
			t.Errorf("status %d: err = %v", status, err)
		}
	}
	r, _ := renamer(&fakePlex{status: http.StatusInternalServerError})
	if _, err := r.rename(bg, guid, nil); err == nil || !strings.HasSuffix(err.Error(), "GET /library/all: status 500") {
		t.Errorf("status 500: err = %v", err)
	}
	r, _ = renamer(&fakePlex{shows: "<html>"})
	if _, err := r.rename(bg, guid, nil); err == nil || !strings.Contains(err.Error(), "GET /library/all") || noAnswer(err) || !private(err) {
		t.Errorf("not JSON: err = %v", err)
	}
	r, _ = renamer(&fakePlex{down: true})
	if _, err := r.rename(bg, guid, nil); err == nil || !strings.Contains(err.Error(), "GET /library/all: connection refused") || !noAnswer(err) || !private(err) {
		t.Errorf("no answer: err = %v", err)
	}
}

// The token goes to PLEX_URL only: a redirect isn't followed (it is a
// failure with its status), and no proxy from the environment is used.
func TestTokenStaysWithPlex(t *testing.T) {
	plex := &fakePlex{status: http.StatusFound, location: "http://elsewhere.example/library/all"}
	r, _ := renamer(plex)
	_, err := r.rename(bg, guid, nil)
	if err == nil || !strings.Contains(err.Error(), "GET /library/all: status 302") {
		t.Errorf("err = %v, want the redirect's status", err)
	}
	sameCalls(t, plex.calls, []string{findAll})

	tr, ok := New("http://plex:32400", "secret").client.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Errorf("transport %T, want an http.Transport without a proxy", New("http://plex:32400", "secret").client.Transport)
	}
}

// Each refresh is followed by one look per pass, counted from the refresh.
func TestQueue(t *testing.T) {
	plex := &fakePlex{shows: container()}
	r, c := renamer(plex)
	r.Queue(guid)
	if wait, more := r.work(bg); wait != passes[0] || !more || len(plex.calls) != 0 {
		t.Fatalf("at once: wait %v, %v, calls %v; want %v and no call", wait, more, plex.calls, passes[0])
	}
	c.add(passes[0] - time.Nanosecond)
	if wait, _ := r.work(bg); wait != time.Nanosecond || len(plex.calls) != 0 {
		t.Fatalf("just before the first look: wait %v, calls %v", wait, plex.calls)
	}
	c.add(time.Nanosecond)
	if wait, more := r.work(bg); wait != passes[1]-passes[0] || !more || len(plex.calls) != 1 {
		t.Fatalf("first look: wait %v, %v, %d calls; want %v and 1 call", wait, more, len(plex.calls), passes[1]-passes[0])
	}
	c.add(passes[1] - passes[0])
	if wait, more := r.work(bg); wait != 0 || more || len(plex.calls) != 2 {
		t.Fatalf("second look: wait %v, %v, %d calls; want nothing left and 2 calls", wait, more, len(plex.calls))
	}
	if len(r.waiting) != 0 {
		t.Errorf("%d shows still wait after their last look", len(r.waiting))
	}
}

// A show refreshed again while it waits gets both looks counted from the
// newer refresh: Plex has just reset its titles once more. The looks of the
// older refresh that didn't happen yet are replaced, not added to.
func TestQueueAgain(t *testing.T) {
	plex := &fakePlex{shows: container()}
	r, c := renamer(plex)
	r.Queue(guid)
	c.add(passes[0])
	r.work(bg) // the first refresh's first look
	c.add(85 * time.Second)
	r.Queue(guid) // 100 s after the first refresh, 20 s before its second look
	if wait, _ := r.work(bg); wait != passes[0] {
		t.Fatalf("wait %v after the second refresh, want %v", wait, passes[0])
	}
	c.add(passes[0])
	if wait, _ := r.work(bg); wait != passes[1]-passes[0] || len(plex.calls) != 2 {
		t.Fatalf("wait %v, %d calls; want %v until the second refresh's last look, and 2 calls", wait, len(plex.calls), passes[1]-passes[0])
	}
	c.add(passes[1] - passes[0])
	if _, more := r.work(bg); more || len(plex.calls) != 3 || len(r.waiting) != 0 {
		t.Errorf("at the end: more %v, %d calls, %d waiting; want 3 calls and nothing left", more, len(plex.calls), len(r.waiting))
	}

	// However often Plex asks, a show has one place per pass.
	for range 1000 {
		r.Queue(guid)
		c.add(time.Millisecond)
	}
	for i := range r.queues {
		if n := r.queues[i].Len(); n != 1 {
			t.Errorf("pass %d: %d looks queued for one show, want 1", i, n)
		}
	}
}

// Looks happen in the order they are due, across the passes.
func TestQueueOrder(t *testing.T) {
	plex := &fakePlex{shows: container()}
	r, c := renamer(plex)
	r.Queue("a")
	c.add(110 * time.Second)
	r.work(bg) // a's first look
	r.Queue("b")
	c.add(passes[0]) // a's last look was due 5 s ago, b's first is due now
	plex.calls = nil
	r.work(bg)
	sameCalls(t, plex.calls, []string{"GET /library/all?type=2&guid=a", "GET /library/all?type=2&guid=b"})
}

// More shows than maxPending aren't planned, so memory stays bounded, and a
// warning says so at most once a minute.
func TestQueueBounded(t *testing.T) {
	out := logs(t)
	r, c := renamer(&fakePlex{})
	for i := range maxPending + 5 {
		r.Queue(fmt.Sprint("show-", i))
	}
	r.Queue("show-0") // one that already waits is still taken
	for i := range r.queues {
		if n := r.queues[i].Len(); n != maxPending {
			t.Errorf("pass %d: %d looks, want %d", i, n, maxPending)
		}
	}
	if n := strings.Count(out.String(), "too many shows"); n != 1 {
		t.Errorf("%d warnings for 5 shows left out, want 1:\n%s", n, out)
	}
	c.add(warnEvery - time.Second)
	r.Queue("late")
	c.add(time.Second)
	r.Queue("later")
	if n := strings.Count(out.String(), "too many shows"); n != 2 {
		t.Errorf("%d warnings, want a second one a minute after the first", n)
	}
}

// The second look uses the shows the first one found, and asks Plex for
// them again only when one of them is gone.
func TestSecondLook(t *testing.T) {
	logs(t)
	plex := twoLibraries()
	r, c := renamer(plex)
	r.Queue(guid)
	c.add(passes[0])
	r.work(bg)
	plex.calls = nil
	c.add(passes[1])
	r.work(bg)
	sameCalls(t, plex.calls, []string{
		"GET /library/metadata/100/extras",
		"PUT /library/metadata/101?title.value=Opening+1",
		"PUT /library/metadata/102?title.value=Opening+1a",
		"PUT /library/metadata/104?title.value=Ending+1+%28creditless%29",
		"PUT /library/metadata/105?title.value=Ending+1",
		"GET /library/metadata/200/extras",
		"PUT /library/metadata/201?title.value=Opening",
	})
	if len(r.waiting) != 0 {
		t.Errorf("%d shows remembered after their last look", len(r.waiting))
	}

	// The show got a new rating key between the looks.
	plex = twoLibraries()
	r, c = renamer(plex)
	r.Queue(guid)
	c.add(passes[0])
	r.work(bg)
	plex.calls = nil
	plex.shows = container(`{"ratingKey":"300","title":"Show"}`)
	plex.extras = map[string]string{"300": container(extra("301", "ED2", "ending", "/media/Show/ED2.mkv"))}
	plex.statuses = map[string]int{"GET /library/metadata/100/extras": 404}
	c.add(passes[1])
	r.work(bg)
	sameCalls(t, plex.calls, []string{
		"GET /library/metadata/100/extras",
		findAll,
		"GET /library/metadata/300/extras",
		"PUT /library/metadata/301?title.value=Ending+2",
	})
}

// A failure is logged at most once a minute, without the token.
func TestWarnOncePerMinute(t *testing.T) {
	out := logs(t)
	r, c := renamer(&fakePlex{status: http.StatusUnauthorized})
	count := func() int { return strings.Count(out.String(), "can't set the opening and ending titles") }
	r.Queue("a")
	r.Queue("b")
	c.add(passes[0])
	r.work(bg)
	if count() != 1 {
		t.Fatalf("two failures at once: %d warnings, want 1:\n%s", count(), out)
	}
	c.add(warnEvery - time.Second)
	r.Queue("c")
	c.add(passes[0]) // past the minute: a's and b's last looks aren't due yet
	r.work(bg)
	if count() != 2 {
		t.Errorf("a failure over a minute later: %d warnings, want 2:\n%s", count(), out)
	}
	if got := out.String(); strings.Contains(got, "secret") || !strings.Contains(got, "PLEX_TOKEN") {
		t.Errorf("logs:\n%s", got)
	}
}

// When Plex doesn't answer three calls in a row, everything queued is
// dropped and nothing is taken for a minute, with one warning.
func TestPauseWhenPlexIsDown(t *testing.T) {
	out := logs(t)
	plex := &fakePlex{down: true, shows: container()}
	r, c := renamer(plex)
	for _, g := range []string{"a", "b", "c", "d", "e"} {
		r.Queue(g)
	}
	c.add(passes[0])
	if _, more := r.work(bg); more || len(plex.calls) != maxFailures || len(r.waiting) != 0 {
		t.Fatalf("more %v, %d calls, %d waiting; want %d calls and everything dropped", more, len(plex.calls), len(r.waiting), maxFailures)
	}
	got := out.String()
	if strings.Count(got, "Plex doesn't answer") != 1 || !strings.Contains(got, "PLEX_URL") || !strings.Contains(got, "shows_dropped=5") ||
		strings.Count(got, "level=WARN") != 2 { // the first failure's own warning, and the pause
		t.Errorf("logs:\n%s", got)
	}

	r.Queue("f")
	c.add(pause - time.Nanosecond)
	r.Queue("g")
	if len(r.waiting) != 0 {
		t.Fatalf("%d shows taken during the pause", len(r.waiting))
	}
	c.add(time.Nanosecond)
	plex.down = false
	r.Queue("h")
	c.add(passes[0])
	if r.work(bg); len(plex.calls) != maxFailures+1 || len(r.waiting) != 1 {
		t.Errorf("after the pause: %d calls, %d waiting; want the show looked at", len(plex.calls), len(r.waiting))
	}
}

// Only calls without an answer, one after the other, lead to the pause: an
// answer, whatever its status, starts the count again.
func TestPauseCount(t *testing.T) {
	out := logs(t)
	plex := &fakePlex{status: http.StatusUnauthorized}
	r, c := renamer(plex)
	for i := range 2 * maxFailures {
		r.Queue(fmt.Sprint("refused-", i))
	}
	c.add(passes[0])
	r.work(bg)
	if len(plex.calls) != 2*maxFailures || len(r.waiting) != 2*maxFailures {
		t.Fatalf("a refused token: %d calls, %d waiting; want no pause", len(plex.calls), len(r.waiting))
	}

	plex = &fakePlex{shows: container()}
	r, c = renamer(plex)
	look := func(name string, down bool) {
		plex.down = down
		r.Queue(name)
		c.add(passes[0])
		r.work(bg)
	}
	look("a", true)
	look("b", true)
	look("c", false) // an answer
	look("d", true)
	look("e", true)
	if strings.Contains(out.String(), "Plex doesn't answer") || len(r.waiting) != 5 {
		t.Fatalf("two failures, an answer, two failures: paused, %d waiting:\n%s", len(r.waiting), out)
	}
	look("f", true)
	if !strings.Contains(out.String(), "Plex doesn't answer") || len(r.waiting) != 0 {
		t.Errorf("a third failure in a row: no pause, %d waiting:\n%s", len(r.waiting), out)
	}
}

// Stopping cuts the call that is running: no warning, and it doesn't count
// as Plex not answering.
func TestStopDuringCall(t *testing.T) {
	out := logs(t)
	ctx, cancel := context.WithCancel(bg)
	r, c := renamer(roundTrip(func(req *http.Request) (*http.Response, error) {
		cancel()
		return nil, req.Context().Err()
	}))
	r.Queue("a")
	r.Queue("b")
	c.add(passes[0])
	if _, more := r.work(ctx); more || r.failures != 0 || out.Len() != 0 {
		t.Errorf("more %v, %d failures, logs %q; want the work ended quietly", more, r.failures, out)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Run does the looks when their time comes and ends with its context. The
// test ends each wait itself and sees each call to Plex as it is made.
func TestRun(t *testing.T) {
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	calls := make(chan string)
	r, c := renamer(roundTrip(func(req *http.Request) (*http.Response, error) {
		select {
		case calls <- req.Method + " " + req.URL.Path:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(container())), Header: http.Header{}}, nil
	}))
	timer := make(chan time.Time, 1)
	r.after = func(time.Duration) <-chan time.Time { return timer }
	fire := func() {
		select {
		case timer <- c.now():
		default: // Run hasn't taken the last one yet: it will see the new time
		}
	}
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	r.Queue(guid)
	for _, wait := range []time.Duration{passes[0], passes[1] - passes[0]} {
		c.add(wait)
		fire()
		if got := <-calls; got != "GET /library/all" {
			t.Errorf("call %q", got)
		}
	}
	cancel()
	<-done
	r.Queue(guid) // nobody listens any more: still returns at once
	r.Queue(guid)
}

func TestCheckURL(t *testing.T) {
	for _, ok := range []string{"http://192.168.1.10:32400", "https://plex.example.com", "http://plex:32400/", "http://[fd00::1]:32400"} {
		if err := CheckURL(ok); err != nil {
			t.Errorf("CheckURL(%q) = %v", ok, err)
		}
	}
	for bad, want := range map[string]string{
		"192.168.1.10:32400":            "must look like",
		"plex":                          "must look like",
		"ftp://plex":                    "must look like",
		"http://":                       "must look like",
		"http://:32400":                 "must look like",
		"http://a b":                    "must look like",
		"http://plex:32400/web":         "without a path",
		"http://plex:32400/web/index":   "without a path",
		"http://plex:32400?X-Plex=1":    "? or #",
		"http://plex:32400/?":           "? or #",
		"http://plex:32400/#top":        "? or #",
		"http://plex:32400#":            "? or #",
		"http://user:hunter2@plex:3240": "user name or password",
		"http://hunter2@plex:32400":     "user name or password",
	} {
		err := CheckURL(bad)
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("CheckURL(%q) = %v, want an error with %q", bad, err, want)
		}
	}
}

// "(creditless)" tells a creditless file from the regular one with the same
// title. Without a regular one there is nothing to tell apart, also when two
// creditless files share a title.
func TestTitlesCreditless(t *testing.T) {
	var a answer
	err := json.Unmarshal([]byte(container(
		extra("1", "NCOP1", "opening", "/media/Show/NCOP1.mkv"),
		extra("2", "Creditless OP1", "opening", "/media/Show/Creditless OP1.mkv"),
		extra("3", "NCED1", "ending", "/media/Show/NCED1.mkv"),
	)), &a)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range titles(a.MediaContainer.Metadata) {
		got = append(got, n.to)
	}
	if want := []string{"Opening 1", "Opening 1", "Ending 1"}; !slices.Equal(got, want) {
		t.Errorf("titles = %q, want %q", got, want)
	}
}
