package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func readDoc(t *testing.T, dir, key string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "data", key+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

// A page keeps the newest MaxBuilds builds (episodes for the newest only)
// and MaxRequests requests, across flushes and a restart.
func TestStoreKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	for i := range MaxBuilds + 2 {
		r := NewRecorder("anidb-1", "anidb")
		r.b.ID, r.b.Start = fmt.Sprint(i), time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC)
		r.Finish(time.Second, nil, ShowDoc{Title: "Show"}, func(b *Build) {
			b.Episodes = []Episode{{S: 1, E: i}}
		})
		s.AddBuild("anidb-1", r)
		if i == 3 {
			s.Flush()
		}
	}
	for i := range MaxRequests + 5 {
		s.AddRequest(Request{Key: "anidb-1", URL: "/r", Status: i})
	}
	s.Flush()

	s, err = Open(dir, "test") // a restart keeps the index
	if err != nil {
		t.Fatal(err)
	}
	s.AddRequest(Request{Key: "anidb-1", URL: "/last", Status: 999})
	s.Flush()

	var doc ShowDoc
	readDoc(t, dir, "anidb-1", &doc)
	if len(doc.Builds) != MaxBuilds || doc.Builds[0].Start.Minute() != MaxBuilds+1 {
		t.Fatalf("%d builds, newest at minute %d", len(doc.Builds), doc.Builds[0].Start.Minute())
	}
	if len(doc.Builds[0].Episodes) != 1 || len(doc.Builds[1].Episodes) != 0 {
		t.Error("only the newest build keeps its episodes")
	}
	if len(doc.Requests) != MaxRequests || doc.Requests[0].Status != 999 || doc.Requests[1].Status != MaxRequests+4 {
		t.Errorf("%d requests, newest %d then %d", len(doc.Requests), doc.Requests[0].Status, doc.Requests[1].Status)
	}
	var rows []IndexRow
	readDoc(t, dir, "index", &rows)
	if len(rows) != 1 || rows[0].Key != "anidb-1" || rows[0].Builds != MaxBuilds {
		t.Errorf("index %+v", rows)
	}
}

// An unmatched title's page goes away once the title matches.
func TestStoreUnmatchedThenMatched(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	key := s.Unmatched(Match{Mode: "anidb", Title: "Some Show", Year: 2020, Min: 80, Candidates: []Candidate{{AID: 9, Title: "Some Other Show", Score: 60}}})
	s.AddRequest(Request{Key: key, URL: "/anidb/library/metadata/matches", Status: 200})
	s.Flush()
	if _, err := os.Stat(filepath.Join(dir, key+".html")); err != nil {
		t.Fatal(err)
	}
	s.Matched("Some Show [anidb-9]")
	s.Flush()
	if _, err := os.Stat(filepath.Join(dir, key+".html")); !os.IsNotExist(err) {
		t.Errorf("page still there: %v", err)
	}
	var rows []IndexRow
	readDoc(t, dir, "index", &rows)
	if len(rows) != 0 {
		t.Errorf("index %+v, want empty", rows)
	}
}

func TestUnmatchedKey(t *testing.T) {
	safe := regexp.MustCompile(`^unmatched-[a-z0-9-]+-[0-9a-f]{8}$`)
	for _, title := range []string{"Hoshizora Ramen Patrol", "進撃の巨人", "../../etc/passwd", "", strings.Repeat("a", 300)} {
		k := UnmatchedKey(title)
		if !safe.MatchString(k) || len(k) > 100 {
			t.Errorf("%q: key %q", title, k)
		}
	}
	if UnmatchedKey("進撃") == UnmatchedKey("巨人") || UnmatchedKey("Show A") == UnmatchedKey("Show B") {
		t.Error("different titles share a key")
	}
	// The page goes when the title matches: tags (which make it match),
	// case and punctuation don't make it another title.
	for _, same := range []string{"Hoshizora Ramen Patrol [anidb-14111]", "hoshizora  ramen patrol", "Hoshizora Ramen Patrol {tvdb2-5}", "Hoshizora: Ramen Patrol"} {
		if UnmatchedKey(same) != UnmatchedKey("Hoshizora Ramen Patrol") {
			t.Errorf("%q has another key than the title without tags", same)
		}
	}
	if got := UnmatchedKey("Hoshizora Ramen Patrol"); !strings.HasPrefix(got, "unmatched-hoshizora-ramen-patrol-") {
		t.Errorf("key %q", got)
	}
}

// Text from a source can't end the page's script or add markup.
func TestRenderEscapes(t *testing.T) {
	data, _ := json.Marshal(ShowDoc{Key: "anidb-1", Title: `</script><img src=x onerror=alert(1)> & co`})
	html, err := Render(Page{Kind: "show", Version: "test", Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(html, []byte("<img src=x")) || bytes.Count(html, []byte("</script>")) != 1 || bytes.Contains(html, []byte(" & co")) {
		t.Error("source text reached the HTML unescaped")
	}
	if bytes.Contains(html, dataSlot) {
		t.Error("data slot not filled")
	}
	start := bytes.Index(html, []byte("const DATA = ")) + len("const DATA = ")
	end := start + bytes.Index(html[start:], []byte(";\n"))
	var page struct {
		Kind    string
		Version string
		Show    ShowDoc
	}
	if err := json.Unmarshal(html[start:end], &page); err != nil || page.Kind != "show" || page.Version != "test" || page.Show.Key != "anidb-1" {
		t.Errorf("page data %+v (%v)", page, err)
	}
}

// Two builds at the same instant (of two roots: one build per root runs at
// a time) get their own ids, safe in a URL.
func TestBuildIDs(t *testing.T) {
	a, b := NewRecorder("anidb-1", "anidb"), NewRecorder("tvdb-2", "tvdb")
	b.b.ID = strings.Replace(b.b.ID, b.b.ID[:strings.IndexByte(b.b.ID, '-')], a.b.ID[:strings.IndexByte(a.b.ID, '-')], 1) // same instant
	if a.ID() == b.ID() {
		t.Errorf("ids %q and %q", a.ID(), b.ID())
	}
	for _, id := range []string{a.ID(), b.ID()} {
		if !regexp.MustCompile(`^[0-9T.]+-[a-z0-9-]+$`).MatchString(id) {
			t.Errorf("id %q isn't URL-safe", id)
		}
	}
}

func finished(root string) *Recorder {
	r := NewRecorder(root, "anidb")
	r.Finish(time.Second, nil, ShowDoc{Title: "Show"}, nil)
	return r
}

// A page that can't be written keeps its updates for the next flush, ahead
// of newer ones, and gives them up after maxFails tries.
func TestFlushRetries(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	blocker := filepath.Join(dir, "anidb-1.html")
	if err := os.Mkdir(blocker, 0o755); err != nil { // the page can't replace a folder
		t.Fatal(err)
	}
	s.AddBuild("anidb-1", finished("anidb-1"))
	s.AddRequest(Request{Key: "anidb-1", URL: "/first"})
	s.Flush()
	s.AddRequest(Request{Key: "anidb-1", URL: "/second"})
	s.Flush()
	if p := s.pending["anidb-1"]; p == nil || len(p.builds) != 1 || len(p.requests) != 2 || p.requests[0].URL != "/first" || p.fails != 2 {
		t.Fatalf("pending after two failed writes: %+v", p)
	}
	if err := os.Rename(blocker, blocker+".moved"); err != nil {
		t.Fatal(err)
	}
	s.Flush()
	var doc ShowDoc
	readDoc(t, dir, "anidb-1", &doc)
	if len(doc.Builds) != 1 || len(doc.Requests) != 2 || doc.Requests[0].URL != "/second" || len(s.pending) != 0 {
		t.Errorf("%d builds, requests %+v, %d pending", len(doc.Builds), doc.Requests, len(s.pending))
	}

	if err := os.Mkdir(filepath.Join(dir, "anidb-2.html"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.AddBuild("anidb-2", finished("anidb-2"))
	for range maxFails {
		s.Flush()
	}
	if s.pending["anidb-2"] != nil {
		t.Errorf("still retrying after %d failures", maxFails)
	}
}

// A download that finishes after the page was written queues the build
// again, which replaces its copy on the page; after LateFor it is ignored.
func TestLateSpan(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	r := NewRecorder("anidb-1", "anidb")
	r.Finish(time.Second, nil, ShowDoc{Title: "Show"}, func(b *Build) { b.Episodes = []Episode{{S: 1, E: 1}} })
	s.AddBuild("anidb-1", r)
	s.Flush()
	r.Span("tvdb", "tvdb/late.json", "", SpanNet, time.Now(), time.Millisecond)
	if s.pending["anidb-1"] == nil {
		t.Fatal("a late download didn't queue the build again")
	}
	s.Flush()
	var doc ShowDoc
	readDoc(t, dir, "anidb-1", &doc)
	if len(doc.Builds) != 1 || len(doc.Builds[0].Trace) != 1 || len(doc.Builds[0].Episodes) != 1 {
		t.Fatalf("%d builds, trace %+v: want the one build, with the late download and its episodes", len(doc.Builds), doc.Builds)
	}

	r.finished = time.Now().Add(-LateFor - time.Second)
	r.Span("tvdb", "tvdb/later.json", "", SpanNet, time.Now(), time.Millisecond)
	if s.pending["anidb-1"] != nil || len(r.b.Trace) != 1 {
		t.Error("a download long after the build was still added")
	}
}

// Updates queued while a write failed go after the failed ones; a match
// in between drops them all.
func TestPendingMerge(t *testing.T) {
	p := &pending{requests: []Request{{URL: "/a"}}, fails: 1}
	p.merge(&pending{requests: []Request{{URL: "/b"}}})
	if len(p.requests) != 2 || p.requests[0].URL != "/a" || p.fails != 1 {
		t.Errorf("merged %+v", p)
	}
	p.merge(&pending{drop: true})
	if !p.drop || len(p.requests) != 0 {
		t.Errorf("after a match: %+v", p)
	}
}

// Unmatched pages nobody asked about for UnmatchedMaxAge go at start-up.
func TestUnmatchedAging(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	oldKey := s.Unmatched(Match{Mode: "anidb", Title: "Old Title"})
	newKey := s.Unmatched(Match{Mode: "anidb", Title: "New Title"})
	s.Flush()
	var rows []IndexRow // as a restart finds it a month later
	readDoc(t, dir, "index", &rows)
	for i := range rows {
		if rows[i].Key == oldKey {
			rows[i].Last = time.Now().Add(-UnmatchedMaxAge - time.Hour)
		}
	}
	data, _ := json.Marshal(rows)
	if err := os.WriteFile(filepath.Join(dir, "data", "index.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	s, _ = Open(dir, "test")
	if _, ok := s.index[oldKey]; ok {
		t.Error("old unmatched page still listed")
	}
	if _, err := os.Stat(filepath.Join(dir, oldKey+".html")); !os.IsNotExist(err) {
		t.Errorf("old unmatched page still there: %v", err)
	}
	if _, ok := s.index[newKey]; !ok {
		t.Error("recent unmatched page gone")
	}
}

// The same request again only counts up; LastMatch keeps the newest match.
func TestRequestsCollapseAndLastMatch(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	t0 := time.Now()
	first, second := &Match{Time: t0, Title: "first"}, &Match{Time: t0.Add(time.Minute), Title: "second"}
	s.AddRequest(Request{Key: "anidb-1", Method: "POST", URL: "/m", Status: 200, Answer: "new build", Match: first})
	s.AddRequest(Request{Key: "anidb-1", Method: "GET", URL: "/a", Status: 200, Answer: "memory, 1s", Time: t0})
	s.AddRequest(Request{Key: "anidb-1", Method: "GET", URL: "/a", Status: 200, Answer: "memory, 5s", Time: t0.Add(time.Second)})
	s.Flush()
	s.AddRequest(Request{Key: "anidb-1", Method: "GET", URL: "/a", Status: 200, Answer: "memory, 9s", Time: t0.Add(2 * time.Second)})
	s.AddRequest(Request{Key: "anidb-1", Method: "POST", URL: "/m", Status: 200, Answer: "new build", Match: second})
	s.AddRequest(Request{Key: "anidb-1", Method: "POST", URL: "/m", Status: 200, Answer: "Fix Match list"})
	s.Flush()
	var doc ShowDoc
	readDoc(t, dir, "anidb-1", &doc)
	if len(doc.Requests) != 4 || doc.Requests[2].Count != 3 || doc.Requests[2].Answer != "memory, 9s" || doc.Requests[0].Count != 0 {
		t.Errorf("requests %+v: want the three GETs as one, counted 3, with the last answer", doc.Requests)
	}
	if doc.LastMatch == nil || doc.LastMatch.Title != "second" {
		t.Errorf("last match %+v, want the second", doc.LastMatch)
	}
}

// Pages are written at every flush, the index at most every IndexEvery,
// except by Flush, which writes everything.
func TestIndexEvery(t *testing.T) {
	defer func(d time.Duration) { IndexEvery = d }(IndexEvery)
	IndexEvery = time.Hour
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	listed := func() int {
		var rows []IndexRow
		readDoc(t, dir, "index", &rows)
		return len(rows)
	}
	s.AddBuild("anidb-1", finished("anidb-1"))
	s.flush(false) // the first write is due
	s.AddBuild("anidb-2", finished("anidb-2"))
	s.flush(false)
	if _, err := os.Stat(filepath.Join(dir, "anidb-2.html")); err != nil {
		t.Fatal(err)
	}
	if n := listed(); n != 1 {
		t.Errorf("index lists %d pages before IndexEvery, want 1", n)
	}
	s.Flush()
	if n := listed(); n != 2 {
		t.Errorf("index lists %d pages after Flush, want 2", n)
	}
}

// A title that matches while flush writes its first page (not in the index
// yet) still has the page removed.
func TestStoreMatchedWhileWriting(t *testing.T) {
	s, _ := Open(t.TempDir(), "test")
	key := s.Unmatched(Match{Mode: "anidb", Title: "Some Show"})
	s.mu.Lock()
	s.pending, s.writing = map[string]*pending{}, s.pending // as flush does
	s.mu.Unlock()
	s.Matched("Some Show")
	if p := s.pending[key]; p == nil || !p.drop {
		t.Errorf("pending %+v, want the page dropped", p)
	}
}

// Recording on a nil recorder or store is a no-op, so code records unconditionally.
func TestNilIsOff(t *testing.T) {
	var r *Recorder
	r.Span("anidb", "x", "", SpanNet, time.Now(), 0)
	r.Step("x")("")
	r.Field(Field{})
	r.Check(Check{})
	r.Finish(0, nil, ShowDoc{}, nil)
	var s *Store
	s.AddBuild("k", NewRecorder("anidb-1", "anidb"))
	s.AddRequest(Request{Key: "k"})
	s.Matched("x")
	s.Flush()
	if s.Unmatched(Match{Title: "x"}) == "" {
		t.Error("Unmatched on a nil store still names the page")
	}
}

// Past MaxTraceRows, rows are counted into one summary row per source and
// kind instead of growing the trace without end.
func TestTraceCap(t *testing.T) {
	r := NewRecorder("tvdb-1", "tvdb")
	now := time.Now()
	for i := range MaxTraceRows + 300 {
		r.Span("tvdb", fmt.Sprintf("crew/%d.json", i), "", SpanCache, now, 0)
	}
	r.Span("tmdb", "tmdb/x.json", "", SpanNet, now, 0)
	b, _ := r.snapshot(func() {})
	if len(b.Trace) != MaxTraceRows+2 {
		t.Fatalf("%d rows, want %d", len(b.Trace), MaxTraceRows+2)
	}
	var sum *Span
	for i := range b.Trace {
		if strings.HasSuffix(b.Trace[i].Label, "more like this") {
			sum = &b.Trace[i]
		}
	}
	last := fmt.Sprintf("last: crew/%d.json", MaxTraceRows+299)
	if sum == nil || sum.Label != "300 more like this" || sum.Note != last || sum.Src != "tvdb" {
		t.Errorf("summary row %+v", sum)
	}
}
