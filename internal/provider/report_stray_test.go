package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kizuna/internal/report"
)

// reportPages lists what the reports folder holds besides the index: the
// pages written (by their HTML or their data) and the keys the index lists,
// both sorted.
func reportPages(t *testing.T, dir string) (pages, listed []string) {
	t.Helper()
	for _, pattern := range []string{filepath.Join(dir, "*.html"), filepath.Join(dir, "data", "*.json")} {
		files, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			name := filepath.Base(f)
			key := strings.TrimSuffix(name, filepath.Ext(name))
			if key != "index" && !slices.Contains(pages, key) {
				pages = append(pages, key)
			}
		}
	}
	slices.Sort(pages)
	var rows []report.IndexRow
	if data, err := os.ReadFile(filepath.Join(dir, "data", "index.json")); err == nil { // written only once a page was
		if err := json.Unmarshal(data, &rows); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range rows {
		listed = append(listed, r.Key)
	}
	slices.Sort(listed)
	return pages, listed
}

// knownOnly answers as testSeries does, and AniDB knows only anime 100.
func knownOnly(path string, q map[string][]string) (int, string) {
	if path == "api.anidb.net/httpapi" && !slices.Equal(q["aid"], []string{"100"}) {
		return 0, "" // 404
	}
	return testSeries(path, q)
}

// A Fix Match search is on no page: not one made for its best candidate,
// which was never built, and not that show's own page either.
func TestFixMatchListOnNoPage(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: knownOnly})
	store, dir, serve := reportsOn(t, p)
	const search = `{"type":2,"title":"Kizuna Tes","manual":1}`
	if code := serve("POST", "/anidb/library/metadata/matches", search); code != 200 {
		t.Fatalf("Fix Match: HTTP %d", code)
	}
	store.Flush()
	if pages, listed := reportPages(t, dir); len(pages) != 0 || len(listed) != 0 {
		t.Errorf("after a Fix Match search: pages %v, index %v; want none", pages, listed)
	}

	serve("GET", "/anidb/library/metadata/anidb-100", "")
	serve("POST", "/anidb/library/metadata/matches", search)
	store.Flush()
	var doc report.ShowDoc
	readJSON(t, filepath.Join(dir, "data", "anidb-100.json"), &doc)
	if len(doc.Builds) != 1 || len(doc.Requests) != 1 || doc.Requests[0].Method != "GET" {
		t.Errorf("the candidate's own page: %d builds, requests %+v; want its build and its metadata request only", len(doc.Builds), doc.Requests)
	}
}

// A key of a show that doesn't exist answers 404 and leaves no page and no
// index row, however often it is asked for. An id tag naming such a show is
// a title without a match: it gets an unmatched page that says so.
func TestNoPageForUnknownShow(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: knownOnly})
	store, dir, serve := reportsOn(t, p)
	var logs strings.Builder
	restore := captureLogs(&logs) // the failed builds' warnings
	defer restore()
	for _, path := range []string{"/anidb/library/metadata/anidb-999999991", "/tvdb/library/metadata/tvdb-999999993", "/auto/library/metadata/tvdb-999999993-s1/children"} {
		for _, how := range []string{"built", "the failure remembered"} {
			if code := serve("GET", path, ""); code != 404 {
				t.Errorf("%s (%s): HTTP %d, want 404", path, how, code)
			}
		}
	}
	store.Flush()
	if pages, listed := reportPages(t, dir); len(pages) != 0 || len(listed) != 0 {
		t.Errorf("after asking for shows that don't exist: pages %v, index %v; want none", pages, listed)
	}

	// The reason is the true one: AniDB may well have a new entry the daily
	// title list doesn't know yet, and TheTVDB a series without episodes.
	tags := []struct{ mode, title, how string }{
		{"anidb", "Wrong Tag [anidb-999999992]", "tag [anidb-999999992]; anidb-999999992 isn't in AniDB's title list (a new entry appears with the next daily list)"},
		{"tvdb", "Other Tag [tvdb-999999994]", "tag [tvdb-999999994]; tvdb-999999994 has no episodes on TheTVDB"},
	}
	var keys []string
	for _, c := range tags {
		for _, body := range []string{`{"type":2,"title":"` + c.title + `"}`, `{"type":3,"parentTitle":"` + c.title + `","index":1,"manual":1}`} {
			if code := serve("POST", "/"+c.mode+"/library/metadata/matches", body); code != 200 {
				t.Errorf("/%s match %s: HTTP %d, want 200 (no match)", c.mode, body, code)
			}
		}
		keys = append(keys, report.UnmatchedKey(c.title))
	}
	store.Flush()
	slices.Sort(keys)
	if pages, listed := reportPages(t, dir); !slices.Equal(pages, keys) || !slices.Equal(listed, keys) {
		t.Fatalf("after tags for shows that don't exist: pages %v, index %v; want only %v", pages, listed, keys)
	}
	for _, c := range tags {
		var un report.UnmatchedDoc
		readJSON(t, filepath.Join(dir, "data", report.UnmatchedKey(c.title)+".json"), &un)
		if un.Match.How != c.how || un.Match.Root != "" || un.Match.By.Search != "" || len(un.Requests) != 1 || un.Requests[0].Note != c.how ||
			un.Requests[0].Answer != (report.Answer{Kind: report.AnswerNoMatch}) || un.Requests[0].Build != "" {
			t.Errorf("unmatched page %+v: want the automatic match's request, no match: %s", un, c.how)
		}
	}
	if strings.Contains(logs.String(), "next daily list") || strings.Contains(logs.String(), "has no episodes on TheTVDB") {
		t.Errorf("the report's reason is in the log:\n%s", logs.String())
	}
}

// A failed build still gets its page when the page is how the user finds
// out what went wrong: a source that is down, a TheTVDB series of the
// mapping without episodes there (the error says to use /anidb or /auto),
// and a show with a page that doesn't exist any more.
func TestFailedBuildKeepsPage(t *testing.T) {
	episodes := func(status int) func(string, map[string][]string) (int, string) {
		return func(path string, q map[string][]string) (int, string) {
			if strings.HasPrefix(path, "api4.thetvdb.com/v4/series/500/episodes") {
				return status, ""
			}
			return knownOnly(path, q)
		}
	}
	for _, c := range []struct {
		name   string
		answer func(string, map[string][]string) (int, string)
		gone   bool // built once, then AniDB doesn't know it
		path   string
		code   int
		builds int
		err    string // in the newest build's error
	}{
		{"TheTVDB down", episodes(500), false, "/tvdb/library/metadata/tvdb-500", 503, 1, "no episode list from TheTVDB"},
		{"no episodes on TheTVDB", episodes(404), false, "/tvdb/library/metadata/tvdb-500", 404, 1, "use /anidb for it (or /auto)"},
		{"a show with a page is gone", knownOnly, true, "/anidb/library/metadata/anidb-100", 404, 2, "unknown"},
	} {
		p := newTestProvider(t, &fakeNet{answer: c.answer})
		store, dir, serve := reportsOn(t, p)
		requests := 1
		if c.gone {
			if code := serve("GET", c.path, ""); code != 200 {
				t.Fatalf("%s: the first build: HTTP %d", c.name, code)
			}
			requests++
			p.mu.Lock()
			clear(p.shows)
			p.mu.Unlock()
			p.buildFn = func(context.Context, string) (*Show, error) { return nil, noShowError{errors.New("anidb 100 unknown")} }
		}
		var logs strings.Builder
		restore := captureLogs(&logs) // the failed build's warning
		code := serve("GET", c.path, "")
		restore()
		store.Flush()
		if code != c.code {
			t.Errorf("%s: HTTP %d, want %d", c.name, code, c.code)
		}
		if pages, listed := reportPages(t, dir); !slices.Equal(pages, []string{"anidb-100"}) || !slices.Equal(listed, []string{"anidb-100"}) {
			t.Errorf("%s: pages %v, index %v; want the show's page", c.name, pages, listed)
			continue
		}
		var doc report.ShowDoc
		readJSON(t, filepath.Join(dir, "data", "anidb-100.json"), &doc)
		if len(doc.Builds) != c.builds || !strings.Contains(doc.Builds[0].Error, c.err) || len(doc.Requests) != requests {
			t.Errorf("%s: %d builds, newest error %q, %d requests; want %d builds, %q, %d requests",
				c.name, len(doc.Builds), doc.Builds[0].Error, len(doc.Requests), c.builds, c.err, requests)
		}
	}
}
