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

// A page with only new requests waits requestsEvery; a build or a no-match
// answer still makes it due after flushEvery, and takes the requests with it.
func TestRequestsWait(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	now := clockAt(s)
	start := *now
	s.AddRequest(Request{Key: "anidb-1", Time: time.Now(), URL: "/a", Status: 200}) // a recent request: the page is kept
	if next, ok := s.nextDue(); !ok || !next.Equal(start.Add(requestsEvery)) {
		t.Errorf("next write at %v (%v), want %v", next, ok, start.Add(requestsEvery))
	}
	s.flush(false, start.Add(requestsEvery-time.Millisecond))
	if pageExists(t, dir, "anidb-1") || s.pending["anidb-1"] == nil {
		t.Fatal("a page with only requests was written before requestsEvery")
	}
	s.flush(false, start.Add(requestsEvery))
	var doc ShowDoc
	readDoc(t, dir, "anidb-1", &doc)
	if len(doc.Requests) != 1 || len(s.pending) != 0 {
		t.Fatalf("requests %+v, %d pending: want the request written at requestsEvery", doc.Requests, len(s.pending))
	}

	*now = start.Add(time.Minute)
	s.AddRequest(Request{Key: "anidb-1", Time: time.Now(), URL: "/b", Status: 200})
	*now = now.Add(time.Second)
	s.AddBuild("anidb-1", finished("anidb-1"))
	key := s.Unmatched(Match{Mode: "anidb", Title: "Nothing"})
	if next, _ := s.nextDue(); !next.Equal(now.Add(flushEvery)) {
		t.Errorf("next write at %v, want %v: a build is due after flushEvery", next, now.Add(flushEvery))
	}
	s.flush(false, now.Add(flushEvery))
	doc = ShowDoc{}
	readDoc(t, dir, "anidb-1", &doc)
	if len(doc.Builds) != 1 || len(doc.Requests) != 2 || doc.Requests[0].URL != "/b" || !pageExists(t, dir, key) {
		t.Errorf("%d builds, requests %+v, no-match page %v: want the build, its request and the no-match page",
			len(doc.Builds), doc.Requests, pageExists(t, dir, key))
	}
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

// oldShowData is how writeShow made a show page's data before it kept the
// builds as saved JSON: everything decoded, changed and encoded again.
func oldShowData(prev []byte, key string, builds []Build, heads []ShowHead, requests []Request) ([]byte, error) {
	var doc ShowDoc
	if prev != nil {
		if err := json.Unmarshal(prev, &doc); err != nil {
			return nil, err
		}
	}
	doc.Schema, doc.Key = schemaVersion, key
	for i, b := range builds {
		if j := slices.IndexFunc(doc.Builds, func(x Build) bool { return x.ID == b.ID }); j >= 0 {
			doc.Builds[j] = b
		} else {
			doc.Builds = append(doc.Builds, b)
		}
		if heads[i].Title != "" {
			doc.ShowHead = heads[i]
		}
	}
	slices.SortStableFunc(doc.Builds, func(a, b Build) int { return b.Start.Compare(a.Start) })
	doc.Builds = doc.Builds[:min(len(doc.Builds), maxBuilds)]
	for i := 1; i < len(doc.Builds); i++ {
		doc.Builds[i].Episodes, doc.Builds[i].Art = nil, nil
	}
	for _, rq := range requests {
		if m := rq.Match; m != nil && (doc.LastMatch == nil || !m.Time.Before(doc.LastMatch.Time)) {
			doc.LastMatch = m
		}
	}
	doc.Requests = newestRequests(doc.Requests, requests)
	if doc.Title == "" {
		doc.Title = key
	}
	if doc.Builds == nil {
		doc.Builds = []Build{}
	}
	if doc.Requests == nil {
		doc.Requests = []Request{}
	}
	return json.Marshal(doc)
}

// A show page written from the saved builds as they are is byte for byte
// the page decoding and encoding them gave, through new builds, a build
// written again, old builds losing their lists, and builds dropping out.
// Each step starts from the file the step before wrote, so old and new
// files mix.
func TestRawBuildsSameFile(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	const key = "anidb-1"
	base := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour) // lately: an old page goes
	build := func(n int) *Recorder {
		r := NewRecorder(key, []string{"anidb", "tvdb"}[n%2])
		r.b.ID, r.b.Start = fmt.Sprint("build-", n), base.Add(time.Duration(n)*time.Hour)
		r.Field(Field{Name: "Title", Order: []string{"anidb"}, Asked: []string{"anidb"}, Offered: map[string]string{"anidb": "Show <b> & co"}, Used: []string{"anidb"}})
		season := 1
		var err error
		if n == 2 {
			err = fmt.Errorf("AniDB is down")
		}
		r.Finish(time.Second, err, Result{Head: ShowHead{Title: fmt.Sprint("Show ", n), IDs: []ID{{"anidb", "1"}}},
			Episodes: []Episode{{Season: 1, Index: n, Title: "Ep   </script>", Titles: map[string]string{"anidb": "x", "tvdb": "y"}}},
			Seasons:  []Season{{Name: "Season 1", Count: 1, Index: 1}}, Item: json.RawMessage(`{"title":"Show"}`),
			Art: []ArtGroup{{Level: "Season 1", Season: &season, Kind: "poster", Images: []ArtImage{{URL: fmt.Sprint("https://img.example/", n), Primary: true}}}}})
		return r
	}
	request := func(n int, match bool) Request {
		rq := Request{Key: key, Time: base.Add(time.Duration(n) * time.Minute), Mode: "tvdb", Method: "GET", URL: fmt.Sprint("/r/", n%3), Status: 200, Answer: Answer{Kind: AnswerMemory, Age: int64(n)}}
		if match {
			rq.Match = &Match{Time: rq.Time, Mode: "tvdb", Type: "show", Title: "Show", Root: key, How: "title search"}
		}
		return rq
	}
	a := build(0)
	steps := []struct {
		name     string
		builds   []*Recorder
		requests []Request
	}{
		{"first build", []*Recorder{a}, []Request{request(1, true), request(2, false), request(3, false)}},
		{"requests only", nil, []Request{request(4, false), request(4, false), request(5, false)}},
		{"a newer build", []*Recorder{build(1)}, []Request{request(6, true)}},
		{"the first build again", []*Recorder{a}, nil},
		{"builds drop out", []*Recorder{build(2), build(3), build(4), build(5), build(6)}, []Request{request(7, false)}},
		{"requests only again", nil, []Request{request(8, false)}},
	}
	var prev []byte
	for _, st := range steps {
		for _, r := range st.builds {
			s.AddBuild(key, r)
		}
		for _, rq := range st.requests {
			s.AddRequest(rq)
		}
		s.flush(false, s.now().Add(requestsEvery))
		var builds []Build
		var heads []ShowHead
		for _, r := range st.builds {
			b, h := r.snapshot(func() { s.AddBuild(key, r) })
			builds, heads = append(builds, b), append(heads, h)
		}
		want, err := oldShowData(prev, key, builds, heads, st.requests)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dir, "data", key+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: data\n%s\nwant\n%s", st.name, got, want)
		}
		html, err := os.ReadFile(filepath.Join(dir, key+".html"))
		if err != nil || !bytes.Contains(html, got) {
			t.Fatalf("%s: the page doesn't hold its data (%v)", st.name, err)
		}
		prev = got
	}
	if r := s.index[key]; r.Builds != maxBuilds || r.Title != "Show 6" || !r.Last.Equal(base.Add(6*time.Hour)) {
		t.Errorf("index row %+v", r)
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
