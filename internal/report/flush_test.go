package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// clockAt makes s's clock read the returned time, which the test moves.
func clockAt(s *Store) *time.Time {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return &now
}

func pageExists(t *testing.T, dir, key string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, key+".html"))
	return err == nil
}

// A page that failed to write is tried again after flushEvery, also when it
// had only requests.
func TestRetryDue(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	now := clockAt(s)
	if err := os.Mkdir(filepath.Join(dir, "anidb-1.html"), 0o755); err != nil { // the page can't replace a folder
		t.Fatal(err)
	}
	s.AddRequest(Request{Key: "anidb-1", URL: "/a"})
	*now = now.Add(requestsEvery)
	s.flush(false, *now)
	if p := s.pending["anidb-1"]; p == nil || !p.due.Equal(now.Add(flushEvery)) {
		t.Errorf("pending %+v, want it due again flushEvery after the failure", p)
	}
}

// When Run ends it writes every update, due or not.
func TestRunFlushesAtShutdown(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	clockAt(s)
	s.AddRequest(Request{Key: "anidb-1", Time: time.Now(), URL: "/a", Status: 200}) // a recent request: the page is kept
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx)
	var doc ShowDoc
	readDoc(t, dir, "anidb-1", &doc)
	var rows []IndexRow
	readDoc(t, dir, "index", &rows)
	if len(doc.Requests) != 1 || len(rows) != 1 || len(s.pending) != 0 {
		t.Errorf("requests %+v, index %+v, %d pending: want everything written", doc.Requests, rows, len(s.pending))
	}
}

// testTimer is Run's timer in TestRunWritesWhenDue: it fires when the test
// says so, never by itself.
type testTimer struct{}

func (testTimer) Stop() bool { return true }

// While Run runs, it waits until the next update is due and then writes
// what is due, and only that: a page due later waits for its own time. A
// new update wakes it to wait for the sooner one. The timer fires when the
// test says, so nothing depends on the real clock.
func TestRunWritesWhenDue(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	var now atomic.Int64 // Run reads the clock while the test moves it
	now.Store(start.UnixNano())
	s.now = func() time.Time { return time.Unix(0, now.Load()).UTC() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asked, fire := make(chan time.Duration), make(chan time.Time)
	s.timer = func(d time.Duration) (<-chan time.Time, storeTimer) {
		select {
		case asked <- d:
		case <-ctx.Done():
		}
		return fire, testTimer{}
	}
	// nextAsk is the wait Run asks for next. The minute is only a safety
	// net: a Run that never asks fails the test instead of hanging it.
	nextAsk := func(why string) time.Duration {
		t.Helper()
		select {
		case d := <-asked:
			return d
		case <-time.After(time.Minute):
			t.Fatal("Run asked for no timer: " + why)
			return 0
		}
	}

	// A request only: due requestsEvery from now. Its signal is taken
	// here, so Run starts with nothing to wake it but what comes next.
	s.AddRequest(Request{Key: "anidb-1", Time: time.Now(), URL: "/a", Status: 200}) // a recent request: the page is kept
	<-s.wake
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	if d := nextAsk("a page is pending"); d != requestsEvery {
		t.Errorf("Run waits %v, want %v", d, requestsEvery)
	}

	// A build for another page, due sooner (flushEvery): it wakes Run,
	// which then waits for it instead.
	now.Store(start.Add(5 * time.Second).UnixNano())
	s.AddBuild("anidb-2", finished("anidb-2")) // due 7 s after start
	if d := nextAsk("a sooner update must wake it"); d != flushEvery {
		t.Errorf("after the build Run waits %v, want %v", d, flushEvery)
	}
	if pageExists(t, dir, "anidb-1") || pageExists(t, dir, "anidb-2") {
		t.Fatal("a page was written before its timer fired")
	}

	// The build's time comes: Run writes its page, not the other one, and
	// then waits for the other.
	now.Store(start.Add(5*time.Second + flushEvery).UnixNano())
	fire <- time.Time{}
	if d := nextAsk("a page is still pending"); d != 3*time.Second { // asked after the write
		t.Errorf("Run then waits %v, want 3s, for the first page", d)
	}
	if pageExists(t, dir, "anidb-1") || !pageExists(t, dir, "anidb-2") {
		t.Errorf("pages written: first %v, second %v; want only the one due", pageExists(t, dir, "anidb-1"), pageExists(t, dir, "anidb-2"))
	}

	cancel()
	<-done
	if !pageExists(t, dir, "anidb-1") {
		t.Error("Run ended without writing the first page")
	}
}

// showFile has ShowDoc's fields in ShowDoc's order: a file read and written
// as a showFile is the file encoding the ShowDoc gives. Every field of the
// doc is set, so one missing from showFile fails here.
func TestShowFileIsShowDoc(t *testing.T) {
	season := 2
	b := Build{ID: "id", Root: "anidb-1", Mode: "auto", Start: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), TookMS: 7,
		Complete: true, Error: "e", Match: &Match{Title: "m"}, Trace: []Span{{Src: "kizuna"}},
		Mapping: []MapRow{{AID: 1}}, Fields: []Field{{Name: "f", Offered: map[string]string{"anidb": "<b>"}}}}
	b.Art = []ArtGroup{{Level: "Season 2", Season: &season, Kind: "poster"}}
	b.Seasons = []Season{{Name: "s"}}
	b.Episodes = []Episode{{Title: "ep"}}
	b.EpsTotal, b.CrewPhotos = 3, "crew photos: 1 of 2"
	b.Item, b.ItemSrc = json.RawMessage(`{"title":"t <"}`), map[string]string{"title": "tvdb"}
	b.Checks = []Check{{Level: LevelWarn, Title: "c"}}
	doc := ShowDoc{Schema: schemaVersion, Key: "anidb-1", ShowHead: ShowHead{Title: "Show & co", Orig: "ショー", IDs: []ID{{"anidb", "1"}}},
		Builds: []Build{b, {ID: "older", Start: b.Start.Add(-time.Hour)}}, Requests: []Request{{URL: "/a", Count: 2, Answer: Answer{Kind: AnswerMemory}}},
		LastMatch: &Match{Title: "m"}}
	v := reflect.ValueOf(doc)
	for i := range v.NumField() {
		if v.Field(i).IsZero() {
			t.Fatalf("ShowDoc.%s is not set in the test", v.Type().Field(i).Name)
		}
	}
	// The same fields by name and tag, so one added to showFile alone (an
	// omitempty one the round trip below wouldn't show) fails too.
	fields := func(typ reflect.Type) (out []string) {
		for i := range typ.NumField() {
			out = append(out, typ.Field(i).Name+" "+string(typ.Field(i).Tag))
		}
		return out
	}
	if d, f := fields(reflect.TypeFor[ShowDoc]()), fields(reflect.TypeFor[showFile]()); !slices.Equal(d, f) {
		t.Errorf("showFile fields %q, ShowDoc fields %q", f, d)
	}
	want, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var file showFile
	if err := json.Unmarshal(want, &file); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("as a showFile\n%s\nwant\n%s", got, want)
	}
	if r := showRow(file); r.Checks != 1 || r.Complete || !r.Failed || r.Builds != 2 || !slices.Equal(r.Roots, []string{"anidb-1", ""}) {
		t.Errorf("row %+v", r)
	}
}

// render gives what replacing the slot in page.html gave.
func TestRenderSameAsReplace(t *testing.T) {
	p := page{Kind: "show", Generated: time.Date(2026, 11, 15, 1, 2, 3, 0, time.UTC), Version: "v", Data: json.RawMessage(`{"key":"anidb-1"}`)}
	got, err := render(p)
	if err != nil {
		t.Fatal(err)
	}
	slot := `{"kind":"show","generated":"2026-11-15T01:02:03Z","version":"v","show":{"key":"anidb-1"}}`
	if want := bytes.Replace(pageHTML, dataSlot, []byte(slot), 1); !bytes.Equal(got, want) {
		t.Error("render differs from page.html with its slot replaced")
	}
}

// A requests-only write of a long show's page doesn't decode its builds:
// a few hundred allocations, where decoding them took about 43,000. The
// bound is far above the count so a Go upgrade doesn't trip it.
func TestRequestsFlushAllocs(t *testing.T) {
	s, err := Open(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	const key = "anidb-69"
	s.AddBuild(key, longShow())
	s.Flush()
	n := 0
	allocs := testing.AllocsPerRun(3, func() {
		n++
		s.AddRequest(Request{Key: key, Time: time.Now(), URL: fmt.Sprint("/r/", n), Status: 200})
		s.flush(false, s.now().Add(requestsEvery))
	})
	if allocs > 3000 {
		t.Errorf("%.0f allocations for one request on a long show's page, want at most 3000", allocs)
	}
}

// writeSaved puts a show page's saved data in place, as an earlier version
// or a hand edit left it.
func writeSaved(t *testing.T, dir, key, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "data", key+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// clean: the saved data decodes into a ShowDoc without an error and is what
// encoding that doc gives, so nothing of the bad build is left.
func clean(t *testing.T, dir, key string) ShowDoc {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "data", key+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc ShowDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("saved data doesn't read back (%v):\n%s", err, data)
	}
	if again, _ := json.Marshal(doc); !bytes.Equal(again, data) {
		t.Fatalf("saved data isn't as encoding gives it:\n%s\nwant\n%s", data, again)
	}
	return doc
}

// savedBuildJSON is a saved build; values gives its tookMs and complete.
func savedBuildJSON(id string, start time.Time, values, extra string) string {
	return `{"id":"` + id + `","root":"anidb-1","mode":"anidb","start":"` + start.UTC().Format(time.RFC3339) +
		`",` + values + `,"trace":null,"epsTotal":0` + extra + `}`
}

const (
	goodValues = `"tookMs":1,"complete":true`
	episodes   = `,"episodes":[{"s":1,"e":1,"title":"x"}]`
)

// A build saved with a value of the wrong type (a hand edit, or a field
// whose type changed without a new schema) doesn't stop its page: what
// decodes is written again, as when the whole page was decoded, and the
// page takes requests and new builds as before. Second in the list and
// still holding its episodes, it is caught where it loses them.
func TestBadSavedBuildHeals(t *testing.T) {
	for name, bad := range map[string]string{
		"a head value":  `"tookMs":1,"complete":"yes"`,
		"another value": `"tookMs":"slow","complete":true`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, _ := Open(dir, "test")
			newest := time.Now().Add(-time.Hour)
			builds := savedBuildJSON("a", newest, goodValues, "") + "," + savedBuildJSON("b", newest.Add(-time.Hour), bad, episodes)
			writeSaved(t, dir, "anidb-1", `{"schema":3,"key":"anidb-1","title":"T","builds":[`+builds+`],"requests":[]}`)
			var logs strings.Builder
			defer captureLogs(&logs)()
			s.AddRequest(Request{Key: "anidb-1", Time: time.Now(), URL: "/a", Status: 200})
			s.Flush()
			if doc := clean(t, dir, "anidb-1"); len(doc.Builds) != 2 || doc.Builds[1].ID != "b" || doc.Builds[1].Episodes != nil || len(doc.Requests) != 1 {
				t.Fatalf("after a request: %d builds, %d requests", len(doc.Builds), len(doc.Requests))
			}
			s.AddBuild("anidb-1", finished("anidb-1"))
			s.Flush()
			if doc := clean(t, dir, "anidb-1"); len(doc.Builds) != 3 || len(s.pending) != 0 {
				t.Errorf("after a build: %d builds, %d pending", len(doc.Builds), len(s.pending))
			}
			if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
				t.Errorf("%d warnings, want the one for the bad build:\n%s", n, logs.String())
			}
		})
	}
}

// The newest build saved with a bad value is written again once, with one
// warning; the next write finds nothing to say.
func TestBadNewestBuildWarnsOnce(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	writeSaved(t, dir, "anidb-1", `{"schema":3,"key":"anidb-1","title":"T","builds":[`+
		savedBuildJSON("a", time.Now().Add(-time.Hour), `"tookMs":1,"complete":"yes"`, episodes)+`],"requests":[]}`)
	var logs strings.Builder
	defer captureLogs(&logs)()
	s.AddRequest(Request{Key: "anidb-1", Time: time.Now(), URL: "/a", Status: 200})
	s.Flush()
	if doc := clean(t, dir, "anidb-1"); len(doc.Builds) != 1 || len(doc.Builds[0].Episodes) != 1 {
		t.Fatalf("builds %+v, want the build with its episodes", doc.Builds)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("%d warnings on the first write, want 1:\n%s", n, logs.String())
	}
	logs.Reset()
	s.AddRequest(Request{Key: "anidb-1", Time: time.Now(), URL: "/b", Status: 200})
	s.Flush()
	if logs.Len() != 0 {
		t.Errorf("the next write logged:\n%s", logs.String())
	}
}

// A null in the saved builds is the zero build, as decoding into []Build
// made it, so the page's script gets an object.
func TestNullSavedBuild(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	writeSaved(t, dir, "anidb-1", `{"schema":3,"key":"anidb-1","title":"T","builds":[null],"requests":[]}`)
	s.AddRequest(Request{Key: "anidb-1", Time: time.Now(), URL: "/a", Status: 200})
	s.Flush()
	data, _ := os.ReadFile(filepath.Join(dir, "data", "anidb-1.json"))
	zero, _ := json.Marshal(Build{})
	if !bytes.Contains(data, []byte(`"builds":[`+string(zero)+`]`)) {
		t.Errorf("saved data %s, want the zero build in place of null", data)
	}
	clean(t, dir, "anidb-1")
}
