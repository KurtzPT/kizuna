package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"kizuna/internal/fetch"
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

// A page keeps the newest maxBuilds builds (episodes for the newest only)
// and maxRequests requests, across flushes and a restart.
func TestStoreKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	for i := range maxBuilds + 2 {
		r := NewRecorder("anidb-1", "anidb")
		r.b.ID, r.b.Start = fmt.Sprint(i), time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC)
		r.Finish(time.Second, nil, Result{Head: ShowHead{Title: "Show"}, Episodes: []Episode{{Season: 1, Index: i}}})
		s.AddBuild("anidb-1", r)
		if i == 3 {
			s.Flush()
		}
	}
	for i := range maxRequests + 5 {
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
	if len(doc.Builds) != maxBuilds || doc.Builds[0].Start.Minute() != maxBuilds+1 {
		t.Fatalf("%d builds, newest at minute %d", len(doc.Builds), doc.Builds[0].Start.Minute())
	}
	if len(doc.Builds[0].Episodes) != 1 || len(doc.Builds[1].Episodes) != 0 {
		t.Error("only the newest build keeps its episodes")
	}
	if len(doc.Requests) != maxRequests || doc.Requests[0].Status != 999 || doc.Requests[1].Status != maxRequests+4 {
		t.Errorf("%d requests, newest %d then %d", len(doc.Requests), doc.Requests[0].Status, doc.Requests[1].Status)
	}
	var rows []IndexRow
	readDoc(t, dir, "index", &rows)
	if len(rows) != 1 || rows[0].Key != "anidb-1" || rows[0].Builds != maxBuilds {
		t.Errorf("index %+v", rows)
	}
}

// An unmatched title's page goes away once the title matches.
func TestStoreUnmatchedThenMatched(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	key := s.Unmatched(Match{Mode: "anidb", Title: "Some Show", Year: 2020, MinScore: 80, Candidates: []Candidate{{AID: 9, Title: "Some Other Show", Score: 60}}})
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
	data, _ := json.Marshal(ShowDoc{Key: "anidb-1", ShowHead: ShowHead{Title: `</script><img src=x onerror=alert(1)> & co`}})
	html, err := render(page{Kind: "show", Version: "test", Data: data})
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
	var got struct {
		Kind    string
		Version string
		Show    ShowDoc
	}
	if err := json.Unmarshal(html[start:end], &got); err != nil || got.Kind != "show" || got.Version != "test" || got.Show.Key != "anidb-1" {
		t.Errorf("page data %+v (%v)", got, err)
	}
}

// Two builds of a root at the same instant (the clock didn't move between
// them) get their own ids, safe in a URL.
func TestBuildIDs(t *testing.T) {
	now := time.Now().Add(time.Hour) // later than every id so far
	at1, at2 := idTime(now), idTime(now)
	if !at2.After(at1) {
		t.Errorf("id times %v then %v: not later", at1, at2)
	}
	a, b := NewRecorder("anidb-1", "anidb"), NewRecorder("anidb-1", "anidb")
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
	r.Finish(time.Second, nil, Result{Head: ShowHead{Title: "Show"}})
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
// again, which replaces its copy on the page; after lateFor it is ignored.
func TestLateSpan(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	r := NewRecorder("anidb-1", "anidb")
	r.Finish(time.Second, nil, Result{Head: ShowHead{Title: "Show"}, Episodes: []Episode{{Season: 1, Index: 1}}})
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

	r.finished = time.Now().Add(-lateFor - time.Second)
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

// Unmatched pages nobody asked about for unmatchedMaxAge go at start-up.
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
			rows[i].Last = time.Now().Add(-unmatchedMaxAge - time.Hour)
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
	memory := func(age int64) Answer { return Answer{Kind: AnswerMemory, Age: age} }
	s.AddRequest(Request{Key: "anidb-1", Method: "POST", URL: "/m", Status: 200, Answer: Answer{Kind: AnswerNewBuild}, Match: first})
	s.AddRequest(Request{Key: "anidb-1", Method: "GET", URL: "/a", Status: 200, Answer: memory(1), Time: t0})
	s.AddRequest(Request{Key: "anidb-1", Method: "GET", URL: "/a", Status: 200, Answer: memory(5), Time: t0.Add(time.Second)})
	s.Flush()
	s.AddRequest(Request{Key: "anidb-1", Method: "GET", URL: "/a", Status: 200, Answer: memory(9), Time: t0.Add(2 * time.Second)})
	s.AddRequest(Request{Key: "anidb-1", Method: "POST", URL: "/m", Status: 200, Answer: Answer{Kind: AnswerNewBuild}, Match: second})
	s.AddRequest(Request{Key: "anidb-1", Method: "POST", URL: "/m", Status: 200, Answer: Answer{Kind: AnswerFixMatch}})
	s.Flush()
	var doc ShowDoc
	readDoc(t, dir, "anidb-1", &doc)
	if len(doc.Requests) != 4 || doc.Requests[2].Count != 3 || doc.Requests[2].Answer != memory(9) || doc.Requests[0].Count != 0 {
		t.Errorf("requests %+v: want the three GETs as one, counted 3, with the last answer", doc.Requests)
	}
	if doc.LastMatch == nil || doc.LastMatch.Title != "second" {
		t.Errorf("last match %+v, want the second", doc.LastMatch)
	}
	// The page's script calls .length and .map on the lists: a page with
	// requests and no build has an empty list of builds, not null.
	data, err := os.ReadFile(filepath.Join(dir, "data", "anidb-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"builds":[]`)) || bytes.Contains(data, []byte(`null`)) {
		t.Errorf("page data %s: want builds [] and no null", data)
	}
	// A request without an answer has none in the page's data.
	if n := bytes.Count(data, []byte(`"answer":`)); n != 4 || !bytes.Contains(data, []byte(`"answer":{"kind":"memory","age":9}`)) {
		t.Errorf("page data %s: want the four answers, the one from memory with its age", data)
	}
	s.AddRequest(Request{Key: "anidb-1", Method: "GET", URL: "/plain", Status: 200})
	s.Flush()
	if data, err = os.ReadFile(filepath.Join(dir, "data", "anidb-1.json")); err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(data, []byte(`"answer":`)); n != 4 {
		t.Errorf("page data %s: want no answer for the request that has none", data)
	}
	key := s.Unmatched(Match{Mode: "anidb", Title: "Asked Once"})
	s.Flush()
	if data, err = os.ReadFile(filepath.Join(dir, "data", key+".json")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"requests":[]`)) {
		t.Errorf("unmatched page data %s: want requests []", data)
	}
}

// Requests are the same when the same thing answered them, however old it
// was: the kind is compared, not the age.
func TestSameRequestByKind(t *testing.T) {
	rq := func(a Answer) Request { return Request{Method: "GET", URL: "/a", Status: 200, Build: "b1", Answer: a} }
	if !sameRequest(rq(Answer{Kind: AnswerMemory, Age: 6}), rq(Answer{Kind: AnswerMemory, Age: 9})) {
		t.Error("from memory 6 s and 9 s old: want the same request")
	}
	if !sameRequest(rq(Answer{Kind: AnswerFailed, Age: 6, After: 120}), rq(Answer{Kind: AnswerFailed, Age: 9, After: 120})) {
		t.Error("a failure 6 s and 9 s old: want the same request")
	}
	if sameRequest(rq(Answer{Kind: AnswerMemory, Age: 6}), rq(Answer{Kind: AnswerNewBuild})) {
		t.Error("from memory and a new build: want different requests")
	}
	if sameRequest(rq(Answer{Kind: AnswerNewBuild}), rq(Answer{Kind: AnswerJoined})) || sameRequest(rq(Answer{}), rq(Answer{Kind: AnswerNoMatch})) {
		t.Error("different kinds: want different requests")
	}
	if !sameRequest(rq(Answer{}), rq(Answer{})) {
		t.Error("no answer twice: want the same request")
	}
	other := rq(Answer{Kind: AnswerMemory})
	other.Build = "b2"
	if sameRequest(rq(Answer{Kind: AnswerMemory}), other) {
		t.Error("another build answered: want different requests")
	}
}

// MatchBy's facts say what the reason's words said.
func TestMatchBy(t *testing.T) {
	name := &ByName{AID: 16983, Sure: "[anidb-16983] or [tmdb-900667]"}
	for _, c := range []struct {
		name            string
		by              MatchBy
		tagRule, search bool
	}{
		{"nothing", MatchBy{}, false, false},
		{"tag", MatchBy{ID: MatchTag, Text: "[anidb-69]"}, false, false},
		{"tag, by name", MatchBy{ID: MatchTag, Text: "[tvdb-81797]", ByName: name}, false, false},
		{"guid", MatchBy{ID: MatchGUID, Text: "tvdb://81797"}, false, false},
		{"two tags", MatchBy{ID: MatchManyTags, Text: "[anidb-69] {mal-21}"}, true, false},
		{"tag mode off", MatchBy{ID: MatchTagOff, Text: "[anidb-69]", Off: "anidb", Need: "ANIDB_CLIENT"}, true, false},
		{"title search", MatchBy{Search: SearchFound, Best: 100}, false, true},
		{"title search, nothing", MatchBy{Search: SearchNone}, false, false},
		{"title search, below min", MatchBy{Search: SearchLow, Best: 62}, false, false},
		{"title search, no such season", MatchBy{Search: SearchFound, Best: 100, Missing: "season 3"}, false, false},
		// Fix Match lists: the title search ran after the id
		{"list: tag", MatchBy{ID: MatchTag, Text: "[anidb-69]", Search: SearchFound, Best: 90}, false, false},
		{"list: two tags", MatchBy{ID: MatchManyTags, Text: "[anidb-69] {mal-21}", Search: SearchFound, Best: 90}, false, false},
		{"list: tag mode off, no title", MatchBy{ID: MatchTagOff, Text: "[anidb-69]", Off: "anidb", Need: "ANIDB_CLIENT", Search: SearchNone}, false, false},
	} {
		if got := c.by.TagRule(); got != c.tagRule {
			t.Errorf("%s: TagRule %v, want %v", c.name, got, c.tagRule)
		}
		if got := c.by.TitleSearch(); got != c.search {
			t.Errorf("%s: TitleSearch %v, want %v", c.name, got, c.search)
		}
	}
	if got, want := name.Note(), "AniDB 16983, may be wrong; use [anidb-16983] or [tmdb-900667] to be sure"; got != want {
		t.Errorf("note %q, want %q", got, want)
	}
	// Only what is set is in the page's data.
	if data, _ := json.Marshal(MatchBy{}); string(data) != `{}` {
		t.Errorf("an empty MatchBy is %s", data)
	}
	data, _ := json.Marshal(MatchBy{ID: MatchTag, Text: "[tvdb-81797]", ByName: name})
	if want := `{"id":"tag","text":"[tvdb-81797]","byName":{"aid":16983,"sure":"[anidb-16983] or [tmdb-900667]"}}`; string(data) != want {
		t.Errorf("by name: %s, want %s", data, want)
	}
	data, _ = json.Marshal(MatchBy{ID: MatchTagOff, Text: "[anidb-69]", Off: "anidb", Need: "ANIDB_CLIENT"})
	if want := `{"id":"off","text":"[anidb-69]","off":"anidb","need":"ANIDB_CLIENT"}`; string(data) != want {
		t.Errorf("tag mode off: %s, want %s", data, want)
	}
	data, _ = json.Marshal(MatchBy{Search: SearchLow, Best: 62, Missing: "episode S01E02"})
	if want := `{"search":"low","best":62,"missing":"episode S01E02"}`; string(data) != want {
		t.Errorf("below min: %s, want %s", data, want)
	}
}

// Facts is a copy of what the build recorded; Finish puts its checks after
// the ones recorded while the build ran.
func TestFactsAndFinish(t *testing.T) {
	r := NewRecorder("anidb-1", "anidb")
	if f := r.Facts(); f.Fields != nil || f.Match != nil || f.Cause {
		t.Errorf("facts of a new build: %+v", f)
	}
	r.Field(Field{Name: "Title", Order: []string{"anidb"}})
	r.Check(Check{Level: LevelInfo, Title: "first"})
	r.SetMatch(&Match{Title: "Show", Score: 90, By: MatchBy{Search: SearchFound, Best: 90}})
	f := r.Facts()
	if len(f.Fields) != 1 || f.Fields[0].Name != "Title" || f.Match == nil || f.Match.Score != 90 || !f.Match.By.TitleSearch() || f.Cause {
		t.Errorf("facts %+v, want the field, the match and no cause", f)
	}
	f.Fields[0].Name, f.Match.Score = "changed", 1 // a copy: the build keeps its own
	r.Check(Check{Level: LevelWarn, Title: "second", Cause: true})
	if f = r.Facts(); !f.Cause || f.Fields[0].Name != "Title" || f.Match.Score != 90 {
		t.Errorf("facts %+v, want a cause and the build's own field and match", f)
	}

	res := Result{Head: ShowHead{Title: "Show"}, Mapping: []MapRow{{AID: 1}}, Complete: true, Seasons: []Season{{Name: "Season 1", Count: 2}},
		Episodes: []Episode{{Season: 1, Index: 1}, {Season: 1, Index: 2}}, EpsTotal: 2, Output: [][2]string{{"title", "Show"}},
		Checks: []Check{{Level: LevelInfo, Title: "third"}, {Level: LevelInfo, Title: "fourth"}}}
	r.Finish(1500*time.Millisecond, nil, res)
	b, head := r.snapshot(func() {})
	var titles []string
	for _, c := range b.Checks {
		titles = append(titles, c.Title)
	}
	if got := strings.Join(titles, ","); got != "first,second,third,fourth" {
		t.Errorf("checks %s, want the recorded ones, then the result's, in order", got)
	}
	if head.Title != "Show" || b.TookMS != 1500 || b.Error != "" || !b.Complete || len(b.Mapping) != 1 || len(b.Seasons) != 1 ||
		len(b.Episodes) != 2 || b.EpsTotal != 2 || len(b.Output) != 1 || b.Match == nil || len(b.Fields) != 1 {
		t.Errorf("build %+v head %+v, want the result's values", b, head)
	}

	failed := NewRecorder("anidb-2", "anidb")
	failed.Finish(time.Second, errors.New("boom"), Result{Head: ShowHead{Title: "Second"}})
	if b, _ := failed.snapshot(func() {}); b.Error != "boom" || b.Complete || b.Checks != nil {
		t.Errorf("failed build %+v", b)
	}
}

// oldPages writes a show page and an unmatched page as a version before
// the schema version saved them (no "schema"; the match's reason only in
// words, the answer a text), with an index that lists both.
func oldPages(t *testing.T, dir string) (unmatched string) {
	t.Helper()
	unmatched = UnmatchedKey("Old Title")
	now := time.Now().UTC().Format(time.RFC3339)
	for name, data := range map[string]string{
		"data/anidb-1.json": `{"key":"anidb-1","title":"Old Show","builds":[{"id":"old","root":"anidb-1","mode":"anidb","start":"` + now + `","tookMs":5,"complete":true,
			"match":{"time":"` + now + `","mode":"anidb","type":"show","title":"Old Show","how":"title search","min":80,"root":"anidb-1","score":100},"trace":[],"epsTotal":0}],
			"requests":[{"time":"` + now + `","mode":"anidb","method":"GET","url":"/old","answer":"memory, 6s","status":200,"bytes":1,"tookMs":1}]}`,
		"data/" + unmatched + ".json": `{"key":"` + unmatched + `","match":{"time":"` + now + `","mode":"anidb","type":"show","title":"Old Title","how":"id tags [anidb-1] [tvdb-2]: only one is allowed","min":80,"tagRule":true},
			"last":"` + now + `","requests":[{"time":"` + now + `","mode":"anidb","method":"POST","url":"/old","answer":"no match","status":200,"bytes":1,"tookMs":1}]}`,
		"data/index.json": `[{"key":"anidb-1","kind":"show","title":"Old Show","last":"` + now + `","complete":true,"checks":0,"builds":1,"requests":1},
			{"key":"` + unmatched + `","kind":"unmatched","title":"Old Title","last":"` + now + `","complete":false,"checks":0,"builds":0,"requests":1,"min":80}]`,
		"anidb-1.html":      "old show page",
		unmatched + ".html": "old unmatched page",
	} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return unmatched
}

// Pages saved before the schema version are not listed, whether the saved
// index lists them or not, and the next write of each starts it over. Their
// files stay until then.
func TestOldSchemaIgnored(t *testing.T) {
	for _, indexed := range []bool{true, false} {
		dir := t.TempDir()
		un := oldPages(t, dir)
		if !indexed { // as after a crash: the pages, and no index
			if err := os.Rename(filepath.Join(dir, "data", "index.json"), filepath.Join(dir, "index.json.gone")); err != nil {
				t.Fatal(err)
			}
		}
		var logs strings.Builder
		restore := captureLogs(&logs)
		s, err := Open(dir, "test")
		restore()
		if err != nil {
			t.Fatal(err)
		}
		if len(s.index) != 0 {
			t.Errorf("indexed %v: index %+v, want no old page listed", indexed, s.index)
		}
		if indexed && !s.indexDirty {
			t.Errorf("the saved index listed old pages: want it written again without them")
		}
		if got := logs.String(); strings.Count(got, "level=INFO") != 1 || !strings.Contains(got, "pages=2") || strings.Contains(got, "level=WARN") {
			t.Errorf("indexed %v: logged %q, want one info line counting the 2 old pages", indexed, got)
		}
		for _, page := range []string{"anidb-1", un} { // nothing is deleted
			if data, err := os.ReadFile(filepath.Join(dir, page+".html")); err != nil || !strings.HasPrefix(string(data), "old ") {
				t.Errorf("%s.html: %q (%v), want the old page left as it is", page, data, err)
			}
		}
		s.Flush()
		if indexed {
			var rows []IndexRow
			readDoc(t, dir, "index", &rows)
			if len(rows) != 0 {
				t.Errorf("index %+v after a flush, want it empty", rows)
			}
		}

		// A title that matches has no listed page: nothing to remove, no error.
		s.Matched("Old Title")
		// Requests alone don't start an unmatched page over: there is no match to show.
		s.AddRequest(Request{Key: un, Method: "POST", URL: "/alone", Status: 200})
		s.Flush()
		if data, _ := os.ReadFile(filepath.Join(dir, un+".html")); string(data) != "old unmatched page" || len(s.index) != 0 || len(s.pending) != 0 {
			t.Errorf("indexed %v: requests alone: page %q, index %+v, pending %+v; want nothing written", indexed, data, s.index, s.pending)
		}

		// The next write of each page starts it over.
		s.AddBuild("anidb-1", finished("anidb-1"))
		s.AddRequest(Request{Key: "anidb-1", Method: "GET", URL: "/new", Status: 200, Answer: Answer{Kind: AnswerMemory, Age: 6}})
		if key := s.Unmatched(Match{Mode: "anidb", Title: "Old Title", By: MatchBy{Search: SearchNone}}); key != un {
			t.Fatalf("unmatched key %q, want %q", key, un)
		}
		s.AddRequest(Request{Key: un, Method: "POST", URL: "/new", Status: 200, Answer: Answer{Kind: AnswerNoMatch}})
		s.Flush()
		var doc ShowDoc
		readDoc(t, dir, "anidb-1", &doc)
		if doc.Schema != schemaVersion || len(doc.Builds) != 1 || doc.Builds[0].ID == "old" || doc.Title != "Show" ||
			len(doc.Requests) != 1 || doc.Requests[0].URL != "/new" || doc.Requests[0].Answer != (Answer{Kind: AnswerMemory, Age: 6}) {
			t.Errorf("indexed %v: show page %+v, want it started over: the new build and request only", indexed, doc)
		}
		var u UnmatchedDoc
		readDoc(t, dir, un, &u)
		if u.Schema != schemaVersion || u.Match.By.Search != SearchNone || len(u.Requests) != 1 || u.Requests[0].URL != "/new" {
			t.Errorf("indexed %v: unmatched page %+v, want it started over: the new match and request only", indexed, u)
		}
		for _, page := range []string{"anidb-1", un} {
			if data, err := os.ReadFile(filepath.Join(dir, page+".html")); err != nil || !bytes.Contains(data, []byte(`"schema":2`)) {
				t.Errorf("%s.html (%v): want the new page", page, err)
			}
		}

		// Current pages are read back: listed at the next start (from the
		// index, or from their data without it), and added to.
		if !indexed {
			if err := os.Rename(filepath.Join(dir, "data", "index.json"), filepath.Join(dir, "index.json.gone2")); err != nil {
				t.Fatal(err)
			}
		}
		logs.Reset()
		restore = captureLogs(&logs)
		s, err = Open(dir, "test")
		restore()
		if err != nil {
			t.Fatal(err)
		}
		if r := s.index["anidb-1"]; len(s.index) != 2 || r.Schema != schemaVersion || r.Builds != 1 || r.Requests != 1 || s.index[un].Title != "Old Title" {
			t.Errorf("indexed %v: index %+v after a restart, want both current pages", indexed, s.index)
		}
		if logs.Len() != 0 {
			t.Errorf("indexed %v: logged %q at a start with current pages only, want nothing", indexed, logs.String())
		}
		s.AddRequest(Request{Key: "anidb-1", Method: "GET", URL: "/later", Status: 200})
		s.AddRequest(Request{Key: un, Method: "POST", URL: "/later", Status: 200})
		s.Flush()
		var doc2 ShowDoc
		var u2 UnmatchedDoc
		readDoc(t, dir, "anidb-1", &doc2)
		readDoc(t, dir, un, &u2)
		if len(doc2.Builds) != 1 || doc2.Builds[0].ID != doc.Builds[0].ID || len(doc2.Requests) != 2 || doc2.Requests[1] != doc.Requests[0] ||
			len(u2.Requests) != 2 || u2.Requests[1].URL != "/new" || u2.Match.Title != "Old Title" || u2.Match.By.Search != SearchNone {
			t.Errorf("indexed %v: pages %+v and %+v, want the saved build, match and requests kept and one request added to each", indexed, doc2, u2)
		}
		// And a title that matches now removes its page.
		s.Matched("Old Title")
		s.Flush()
		if _, err := os.Stat(filepath.Join(dir, un+".html")); !os.IsNotExist(err) {
			t.Errorf("indexed %v: unmatched page still there after its title matched: %v", indexed, err)
		}
	}
}

// An old unmatched page nobody asks about again is not removed when its
// 30 days are over either: it is not listed, so nothing looks at it, and a
// start with it fails nothing.
func TestOldSchemaUnmatchedNotExpired(t *testing.T) {
	dir := t.TempDir()
	un := oldPages(t, dir)
	var rows []IndexRow
	readDoc(t, dir, "index", &rows)
	for i := range rows {
		rows[i].Last = time.Now().Add(-unmatchedMaxAge - time.Hour)
	}
	data, _ := json.Marshal(rows)
	data = bytes.ReplaceAll(data, []byte(`"schema":0,`), nil) // as the old version wrote it
	if err := os.WriteFile(filepath.Join(dir, "data", "index.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	s.Flush()
	if _, err := os.Stat(filepath.Join(dir, un+".html")); err != nil || len(s.index) != 0 {
		t.Errorf("old unmatched page: %v, index %+v; want it left alone and unlisted", err, s.index)
	}
}

// captureLogs sends slog's output to w until the returned func is called.
func captureLogs(w *strings.Builder) (restore func()) {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(old) }
}

// Pages are written at every flush, the index at most every indexEvery,
// except by Flush, which writes everything.
func TestIndexEvery(t *testing.T) {
	defer func(d time.Duration) { indexEvery = d }(indexEvery)
	indexEvery = time.Hour
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
		t.Errorf("index lists %d pages before indexEvery, want 1", n)
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
	r.Finish(0, nil, Result{})
	if f := r.Facts(); f.Fields != nil || f.Match != nil || f.Cause {
		t.Errorf("facts of a nil recorder: %+v", f)
	}
	var s *Store
	s.AddBuild("k", NewRecorder("anidb-1", "anidb"))
	s.AddRequest(Request{Key: "k"})
	s.Matched("x")
	s.Flush()
	if s.Unmatched(Match{Title: "x"}) == "" {
		t.Error("Unmatched on a nil store still names the page")
	}
}

// Past maxTraceRows, rows are counted into one summary row per source and
// kind instead of growing the trace without end.
func TestTraceCap(t *testing.T) {
	r := NewRecorder("tvdb-1", "tvdb")
	now := time.Now()
	for i := range maxTraceRows + 300 {
		r.Span("tvdb", fmt.Sprintf("crew/%d.json", i), "", SpanCache, now, 0)
	}
	r.Span("tmdb", "tmdb/x.json", "", SpanNet, now, 0)
	b, _ := r.snapshot(func() {})
	if len(b.Trace) != maxTraceRows+2 {
		t.Fatalf("%d rows, want %d", len(b.Trace), maxTraceRows+2)
	}
	var sum *Span
	for i := range b.Trace {
		if strings.HasSuffix(b.Trace[i].Label, "more like this") {
			sum = &b.Trace[i]
		}
	}
	last := fmt.Sprintf("last: crew/%d.json", maxTraceRows+299)
	if sum == nil || sum.Label != "300 more like this" || sum.Note != last || sum.Src != "tvdb" {
		t.Errorf("summary row %+v", sum)
	}
}

// Failed and stale rows are what the trace is read for: past maxTraceRows
// they stay whole rows with their error, up to maxFailRows; past that their
// summary row keeps the last one's error.
func TestTraceCapKeepsFailures(t *testing.T) {
	r := NewRecorder("tvdb-1", "tvdb")
	now := time.Now()
	for i := range maxTraceRows {
		r.Span("tvdb", fmt.Sprintf("crew/%d.json", i), "", SpanCache, now, 0)
	}
	r.Span("tvdb", "tvdb/stale.json", "HTTP 503", SpanStale, now, 0)
	for i := range maxFailRows + 9 { // the stale row counts too: the last 10 are summed up
		r.Span("tmdb", fmt.Sprintf("tmdb/%d.json", i), fmt.Sprintf("HTTP 500 #%d", i), SpanFail, now, 0)
	}
	b, _ := r.snapshot(func() {})
	rows := map[string]Span{}
	for _, sp := range b.Trace {
		rows[sp.Label] = sp
	}
	if sp := rows["tvdb/stale.json"]; sp.Kind != SpanStale || sp.Note != "HTTP 503" {
		t.Errorf("stale row past maxTraceRows %+v, want it whole with its error", sp)
	}
	lastWhole := maxFailRows - 2
	if sp := rows[fmt.Sprintf("tmdb/%d.json", lastWhole)]; sp.Note != fmt.Sprintf("HTTP 500 #%d", lastWhole) {
		t.Errorf("failure %d %+v, want it whole with its error", lastWhole, sp)
	}
	sum := rows["10 more like this"]
	if want := fmt.Sprintf("last: tmdb/%d.json: HTTP 500 #%d", maxFailRows+8, maxFailRows+8); sum.Kind != SpanFail || sum.Note != want {
		t.Errorf("failures past maxFailRows: summary %+v, want note %q", sum, want)
	}
	if len(b.Trace) != maxTraceRows+maxFailRows+1 {
		t.Errorf("%d rows, want %d", len(b.Trace), maxTraceRows+maxFailRows+1)
	}
}

// Pages written less than indexEvery before a crash, or listed in an index
// that can't be read, are listed again at the next start, so they still
// expire and go once their title matches.
func TestOpenListsUnindexedPages(t *testing.T) {
	defer func(d time.Duration) { indexEvery = d }(indexEvery)
	indexEvery = time.Hour
	dir := t.TempDir()
	s, _ := Open(dir, "test")
	s.AddBuild("anidb-1", finished("anidb-1"))
	s.flush(false) // the first index write is due: it lists anidb-1
	failed := NewRecorder("anidb-2", "anidb")
	failed.Finish(time.Second, errors.New("AniDB is down"), Result{Head: ShowHead{Title: "Second"}})
	s.AddBuild("anidb-2", failed)
	newKey := s.Unmatched(Match{Mode: "anidb", Title: "New Title", MinScore: 80, Candidates: []Candidate{{AID: 9, Title: "Close", Score: 60}}})
	oldKey := s.Unmatched(Match{Mode: "anidb", Title: "Old Title"})
	s.flush(false) // the pages, not the index: then the process dies
	var old UnmatchedDoc
	readDoc(t, dir, oldKey, &old)
	old.Last = time.Now().Add(-unmatchedMaxAge - time.Hour) // and its title isn't asked about for a month
	data, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, "data", oldKey+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	s, _ = Open(dir, "test")
	if r := s.index["anidb-2"]; r.Title != "Second" || !r.Failed || r.Complete || r.Builds != 1 {
		t.Errorf("unlisted show row %+v, want it rebuilt from its data, failed", r)
	}
	if r := s.index[newKey]; r.Title != "New Title" || r.Best != "anidb-9 Close" || r.Score != 60 || r.MinScore != 80 {
		t.Errorf("unlisted unmatched row %+v, want it rebuilt from its data", r)
	}
	if _, ok := s.index[oldKey]; ok {
		t.Error("unlisted old unmatched page listed")
	}
	if _, err := os.Stat(filepath.Join(dir, oldKey+".html")); !os.IsNotExist(err) {
		t.Errorf("unlisted old unmatched page still there: %v", err)
	}
	s.Matched("New Title")
	s.Flush()
	if _, err := os.Stat(filepath.Join(dir, newKey+".html")); !os.IsNotExist(err) {
		t.Errorf("unlisted unmatched page still there after its title matched: %v", err)
	}
	var rows []IndexRow
	readDoc(t, dir, "index", &rows)
	if len(rows) != 2 {
		t.Errorf("index %+v, want both shows", rows)
	}

	// An unreadable index: every page is listed again from its data.
	if err := os.WriteFile(filepath.Join(dir, "data", "index.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ = Open(dir, "test")
	if len(s.index) != 2 || !s.indexDirty {
		t.Errorf("index %+v (dirty %v), want both shows and a rewrite", s.index, s.indexDirty)
	}
	s.Flush()
	if s, _ = Open(dir, "test"); s.indexDirty || len(s.index) != 2 {
		t.Errorf("index %+v (dirty %v): an index that lists every page is kept as it is", s.index, s.indexDirty)
	}
}

// The index says a failed newest build failed, as the show page does.
func TestIndexRowFailed(t *testing.T) {
	ok := showRow(ShowDoc{Builds: []Build{{Complete: true}}})
	failed := showRow(ShowDoc{Builds: []Build{{Complete: true, Error: "boom"}}})
	if !ok.Complete || ok.Failed || failed.Complete || !failed.Failed {
		t.Errorf("rows %+v and %+v", ok, failed)
	}
}

// Trace kinds are fetch's kinds converted as they are.
func TestSpanKindsAreFetchKinds(t *testing.T) {
	for k, sk := range map[fetch.Kind]SpanKind{fetch.KindNet: SpanNet, fetch.KindCache: SpanCache, fetch.KindStale: SpanStale,
		fetch.KindFail: SpanFail, fetch.KindMemory: SpanMemory} {
		if SpanKind(k) != sk {
			t.Errorf("fetch kind %q, span kind %q", k, sk)
		}
	}
}

// page.html says the limits in words.
func TestPageRepeatsLimits(t *testing.T) {
	for _, want := range []string{fmt.Sprintf("up to %d are kept", maxRequests), fmt.Sprintf("after %d days without a request", unmatchedMaxAge/(24*time.Hour))} {
		if !bytes.Contains(pageHTML, []byte(want)) {
			t.Errorf("page.html doesn't say %q", want)
		}
	}
}
