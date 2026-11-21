package mapping

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

// notModified answers each list's URL with one list and an ETag, and 304
// when asked with that ETag. Requests only come from the Load under test.
type notModified struct{}

func (notModified) RoundTrip(r *http.Request) (*http.Response, error) {
	h := http.Header{"Etag": {`"v1"`}}
	if r.Header.Get("If-None-Match") == `"v1"` {
		return &http.Response{StatusCode: http.StatusNotModified, Body: io.NopCloser(strings.NewReader("")), Header: h, Request: r}, nil
	}
	body := `<anime-list><anime anidbid="1" tvdbid="100" defaulttvdbseason="1"/></anime-list>`
	if r.URL.String() == fribbURL {
		body = goodFribb
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: h, Request: r}, nil
}

// An unchanged daily reload keeps the loaded mapping; a changed custom
// file still reloads it.
func TestLoadSkipsUnchanged(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(notModified{})
	l := &Lists{}
	load := func() {
		t.Helper()
		if err := l.Load(context.Background(), f, dir); err != nil {
			t.Fatal(err)
		}
	}
	expire := func() { // the daily reload: the cached copies have expired
		t.Helper()
		old := time.Now().Add(-48 * time.Hour)
		for _, name := range []string{"anime-list-master.xml", "fribb-anime-list-mini.json"} {
			if err := os.Chtimes(filepath.Join(dir, "cache", "mapping", name), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	load()
	first := l.cur.Load()
	expire()
	load() // 304
	if l.cur.Load() != first {
		t.Error("unchanged list was parsed and merged again")
	}
	if l.ByAID(1).TVDB.ID != 100 || l.ByAID(2).MAL != 7 {
		t.Fatalf("entry 1: %+v, entry 2: %+v", l.ByAID(1), l.ByAID(2))
	}

	custom := `<anime-list><anime anidbid="1" tvdbid="200" defaulttvdbseason="1"/></anime-list>`
	if err := os.WriteFile(filepath.Join(dir, "anime-list-custom.xml"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	expire()
	load()
	if l.ByAID(1).TVDB.ID != 200 {
		t.Fatalf("custom file changed, but the mapping was kept: %+v", l.ByAID(1))
	}
}

// down answers every request with a 500: no list downloads.
type down struct{}

func (down) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
}

// anime-list-custom.xml applies even while no list has loaded (a first start
// without network): its entries are used, but the mapping isn't Ready, so
// /health stays degraded and the lists are retried soon. Once a list loads,
// the custom entries still win over it.
func TestLoadCustomWithoutLists(t *testing.T) {
	dir := t.TempDir()
	custom := `<anime-list><anime anidbid="1" tvdbid="200" defaulttvdbseason="1"/></anime-list>`
	if err := os.WriteFile(filepath.Join(dir, "anime-list-custom.xml"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(down{})
	l := &Lists{}
	if err := l.Load(context.Background(), f, dir); err == nil {
		t.Error("no list loaded: want an error to log")
	}
	if l.ByAID(1).TVDB.ID != 200 || l.AIDFor("tvdb", "200") != 1 {
		t.Errorf("custom entry not applied without a list: %+v", l.ByAID(1))
	}
	if l.Ready() {
		t.Error("custom entries alone must not make the mapping Ready")
	}

	f.SetTransport(notModified{})
	if err := l.Load(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	if !l.Ready() || l.ByAID(1).TVDB.ID != 200 {
		t.Errorf("after the list loaded: ready %v, entry 1 %+v (custom wins)", l.Ready(), l.ByAID(1))
	}
}

// lists answers each list URL with its body (no ETag); a missing one is a 404.
type lists map[string]string

func (b lists) RoundTrip(r *http.Request) (*http.Response, error) {
	body, ok := b[r.URL.String()]
	code := 200
	if !ok {
		code = 404
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

const (
	goodAnimeLists = `<anime-list><anime anidbid="1" tvdbid="100" defaulttvdbseason="1">
<mapping-list><mapping anidbseason="1" tvdbseason="1">;5-0;</mapping></mapping-list></anime></anime-list>`
	brokenAnimeLists = `<anime-list><anime anidbid="1" tvdbid="100" & defaulttvdbseason="1"/></anime-list>`
	goodFribb        = `[{"anidb_id": 2, "tvdb_id": 200, "mal_id": 7}]`
)

// Load merges Anime-Lists first: where both lists
// place an anime, Anime-Lists' series, season and exceptions stay, and Fribb
// only adds the ids that entry lacks.
func TestLoadAnimeListsFirst(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(lists{animeListsURL: goodAnimeLists,
		fribbURL: `[{"anidb_id": 1, "tvdb_id": 999, "season": {"tvdb": 4}, "mal_id": 7}]`})
	l := &Lists{}
	if err := l.Load(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	e := l.ByAID(1)
	if e.TVDB.ID != 100 || e.TVDB.Season != 1 || !e.TVDB.HasException(1, 5) || e.MAL != 7 {
		t.Errorf("entry 1: %+v; want Anime-Lists' placement with Fribb's MAL id", e)
	}
	if l.AIDFor("tvdb", "999") != 0 {
		t.Error("Fribb's TVDB id replaced Anime-Lists'")
	}
}

// A download that doesn't parse is rejected before it's cached: the good
// cached copy stays, and so does the mapping. With no copy at all, the list
// counts as failed.
func TestLoadRejectsBrokenDownload(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(lists{animeListsURL: goodAnimeLists, fribbURL: goodFribb})
	l := &Lists{}
	if err := l.Load(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(dir, "cache", "mapping", "anime-list-master.xml")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(cached, old, old); err != nil {
		t.Fatal(err)
	}
	f.SetTransport(lists{animeListsURL: brokenAnimeLists, fribbURL: goodFribb})
	if err := l.Load(context.Background(), f, dir); err != nil {
		t.Errorf("broken download with a good cached copy: %v, want the copy used", err)
	}
	if b, _ := os.ReadFile(cached); string(b) != goodAnimeLists {
		t.Errorf("broken download replaced the cached copy: %q", b)
	}
	if !l.ByAID(1).TVDB.HasException(1, 5) || !l.Ready() {
		t.Errorf("mapping lost its exception: %+v", l.ByAID(1))
	}

	empty := t.TempDir()
	f2 := fetch.New(filepath.Join(empty, "cache"))
	// Fribb loads in both cases: the error is Anime-Lists'.
	f2.SetTransport(lists{animeListsURL: brokenAnimeLists, fribbURL: goodFribb})
	if err := (&Lists{}).Load(context.Background(), f2, empty); err == nil || !strings.Contains(err.Error(), "animelists") {
		t.Errorf("broken download and no cached copy: %v, want an error", err)
	}
	empty = t.TempDir()
	f2 = fetch.New(filepath.Join(empty, "cache"))
	f2.SetTransport(lists{animeListsURL: `<anime-list></anime-list>`, fribbURL: goodFribb})
	if err := (&Lists{}).Load(context.Background(), f2, empty); err == nil || !strings.Contains(err.Error(), "animelists") {
		t.Errorf("a list without entries: %v, want an error", err)
	}
}

// Once Ready, a list that fails (here a cached copy that doesn't parse) keeps
// the loaded mapping instead of replacing it with the other list alone. On a
// first load the other list is still used.
func TestLoadKeepsMappingWhenAListFails(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(lists{animeListsURL: goodAnimeLists, fribbURL: goodFribb})
	l := &Lists{}
	if err := l.Load(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(dir, "cache", "mapping", "anime-list-master.xml")
	if err := os.WriteFile(cached, []byte(brokenAnimeLists), 0o644); err != nil { // fresh: no download
		t.Fatal(err)
	}
	err := l.Load(context.Background(), f, dir)
	if err == nil || !strings.Contains(err.Error(), "keeping") {
		t.Errorf("a list failed: %v, want an error saying the mapping is kept", err)
	}
	if !l.ByAID(1).TVDB.HasException(1, 5) || l.ByAID(2).MAL != 7 || !l.Ready() {
		t.Errorf("mapping not kept: 1 %+v, 2 %+v", l.ByAID(1), l.ByAID(2))
	}

	first := &Lists{}
	if err := first.Load(context.Background(), f, dir); err == nil {
		t.Error("first load with a broken list: want an error to log")
	}
	if !first.Ready() || first.ByAID(2).MAL != 7 || first.ByAID(1).AID != 0 {
		t.Errorf("first load: ready %v, 1 %+v, 2 %+v; want fribb alone", first.Ready(), first.ByAID(1), first.ByAID(2))
	}
}

// A list that has never loaded (Fribb down on a fresh install, nothing
// cached) doesn't freeze the mapping the other list made: its updates and
// custom file edits still apply on the daily reload.
func TestLoadAppliesChangesWhileANeverLoadedListFails(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(lists{animeListsURL: goodAnimeLists}) // fribb: 404
	l := &Lists{}
	if err := l.Load(context.Background(), f, dir); err == nil {
		t.Error("fribb failed: want an error to log")
	}
	if !l.Ready() || l.ByAID(1).TVDB.ID != 100 {
		t.Fatalf("first load: ready %v, 1 %+v", l.Ready(), l.ByAID(1))
	}

	cached := filepath.Join(dir, "cache", "mapping", "anime-list-master.xml")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(cached, old, old); err != nil {
		t.Fatal(err)
	}
	f.SetTransport(lists{animeListsURL: `<anime-list><anime anidbid="1" tvdbid="300" defaulttvdbseason="1"/>` +
		`<anime anidbid="3" tvdbid="400" defaulttvdbseason="1"/></anime-list>`})
	custom := `<anime-list><anime anidbid="5" tvdbid="500" defaulttvdbseason="1"/></anime-list>`
	if err := os.WriteFile(filepath.Join(dir, "anime-list-custom.xml"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	err := l.Load(context.Background(), f, dir)
	if err == nil || strings.Contains(err.Error(), "keeping") {
		t.Errorf("fribb still fails: %v, want its error but not the mapping kept", err)
	}
	if l.ByAID(1).TVDB.ID != 300 || l.ByAID(3).TVDB.ID != 400 || l.ByAID(5).TVDB.ID != 500 || !l.Ready() {
		t.Errorf("changes not applied: 1 %+v, 3 %+v, 5 %+v", l.ByAID(1), l.ByAID(3), l.ByAID(5))
	}
}

// Missing names the lists no mapping has had yet: all of them before the
// first load (custom entries alone don't count), the one that failed on a
// first start while the other loaded, none once both have. A list that fails
// after it loaded once isn't missing: the mapping kept still has it.
func TestMissingUntilEveryListLoaded(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	l := &Lists{}
	if got := l.Missing(); !slices.Equal(got, []string{"animelists", "fribb"}) {
		t.Errorf("before any load: missing %v, want both lists", got)
	}
	custom := `<anime-list><anime anidbid="5" tvdbid="500" defaulttvdbseason="1"/></anime-list>`
	if err := os.WriteFile(filepath.Join(dir, "anime-list-custom.xml"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	f.SetTransport(lists{}) // both 404
	_ = l.Load(context.Background(), f, dir)
	if got := l.Missing(); l.Ready() || !slices.Equal(got, []string{"animelists", "fribb"}) {
		t.Errorf("custom entries alone: ready %v, missing %v, want both lists", l.Ready(), got)
	}

	f.SetTransport(lists{animeListsURL: goodAnimeLists}) // fribb: 404
	_ = l.Load(context.Background(), f, dir)
	if got := l.Missing(); !l.Ready() || !slices.Equal(got, []string{"fribb"}) {
		t.Errorf("fribb failed on the first start: ready %v, missing %v, want fribb", l.Ready(), got)
	}

	f.SetTransport(lists{animeListsURL: goodAnimeLists, fribbURL: goodFribb})
	if err := l.Load(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	if got := l.Missing(); len(got) > 0 {
		t.Errorf("both loaded: missing %v", got)
	}

	cached := filepath.Join(dir, "cache", "mapping", "fribb-anime-list-mini.json")
	if err := os.WriteFile(cached, []byte("[broken"), 0o644); err != nil { // fresh: no download
		t.Fatal(err)
	}
	if err := l.Load(context.Background(), f, dir); err == nil {
		t.Error("broken fribb: want an error to log")
	}
	if got := l.Missing(); len(got) > 0 || l.ByAID(2).MAL != 7 {
		t.Errorf("fribb failed after it loaded: missing %v, entry 2 %+v; want the mapping kept", got, l.ByAID(2))
	}
}

// A custom file broken during a reload is reported, but the entries of its
// last good version stay, also on the reloads after; fixed, it applies again.
func TestLoadKeepsCustomEntriesWhileTheFileIsBroken(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(lists{animeListsURL: goodAnimeLists, fribbURL: goodFribb})
	path := filepath.Join(dir, "anime-list-custom.xml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := &Lists{}
	write(`<anime-list><anime anidbid="1" tvdbid="200" defaulttvdbseason="1"/></anime-list>`)
	if err := l.Load(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	write(`<anime-list><anime anidbid="1" tvdbid="300" & defaulttvdbseason="1"/></anime-list>`)
	for i := range 2 {
		err := l.Load(context.Background(), f, dir)
		if err == nil || !strings.Contains(err.Error(), "anime-list-custom.xml ignored") {
			t.Errorf("reload %d with a broken custom file: %v, want it reported", i, err)
		}
		if l.ByAID(1).TVDB.ID != 200 || !l.Ready() {
			t.Errorf("reload %d: the custom entry was dropped: %+v", i, l.ByAID(1))
		}
	}
	write(`<anime-list><anime anidbid="1" tvdbid="400" defaulttvdbseason="1"/></anime-list>`)
	if err := l.Load(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	if l.ByAID(1).TVDB.ID != 400 {
		t.Errorf("fixed custom file not applied: %+v", l.ByAID(1))
	}
}

// captureWarnings sends slog's lines to a buffer for the test.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// Custom entries without a usable anidbid are skipped with one warning per
// load that counts them and names the first few, by place and value. The
// other entries apply. An odd entry in the downloaded list stays quiet: it
// is not the user's to fix.
func TestLoadWarnsAboutCustomEntriesWithoutAniDBID(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(lists{animeListsURL: `<anime-list><anime anidbid="1" tvdbid="100" defaulttvdbseason="1"/>` +
		`<anime tvdbid="101"/></anime-list>`, fribbURL: goodFribb})
	custom := `<anime-list>
<anime anidbid="5" tvdbid="500" defaulttvdbseason="1"/>
<anime tvdbid="600" defaulttvdbseason="1"/>
<anime anidbid="12a" tvdbid="700" defaulttvdbseason="1"/>
<anime anidbid="-3" tvdbid="800" defaulttvdbseason="1"/>
</anime-list>`
	if err := os.WriteFile(filepath.Join(dir, "anime-list-custom.xml"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	logs := captureWarnings(t)
	l := &Lists{}
	if err := l.Load(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	if l.ByAID(5).TVDB.ID != 500 || l.ByAID(-3).AID != 0 || l.AIDFor("tvdb", "800") != 0 {
		t.Errorf("entry 5 %+v, entry -3 %+v: want only the good custom entry", l.ByAID(5), l.ByAID(-3))
	}
	log := logs.String()
	if strings.Count(log, "level=WARN") != 1 || !strings.Contains(log, "usable anidbid") ||
		!strings.Contains(log, "file=anime-list-custom.xml") || !strings.Contains(log, "entries=3") ||
		!strings.Contains(log, `first="anime 2: no anidbid; anime 3: anidbid=12a; anime 4: anidbid=-3"`) {
		t.Errorf("want one warning counting and naming the three custom entries, got:\n%s", log)
	}
}

// An anime-list-custom.xml without any element (blanks, a BOM, only <?xml?>
// or a comment) has no entries: no error at each load, and the entries of an
// earlier version are gone. A document cut off is still an error. Zero bytes
// is TestLoadZeroByteCustomFile.
func TestLoadEmptyCustomFile(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(lists{animeListsURL: goodAnimeLists, fribbURL: goodFribb})
	path := filepath.Join(dir, "anime-list-custom.xml")
	if err := os.WriteFile(path, []byte(`<anime-list><anime anidbid="5" tvdbid="500" defaulttvdbseason="1"/></anime-list>`), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &Lists{}
	if err := l.Load(context.Background(), f, dir); err != nil || l.ByAID(5).TVDB.ID != 500 {
		t.Fatalf("custom file: %v, entry 5 %+v", err, l.ByAID(5))
	}
	for _, content := range []string{" \r\n\t", "\xef\xbb\xbf", "\xef\xbb\xbf\r\n",
		`<?xml version="1.0" encoding="UTF-8"?>` + "\n", "<!-- nothing yet -->\n",
		"\xef\xbb\xbf" + `<?xml version="1.0"?>` + "\n<!-- nothing yet -->\n"} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := l.Load(context.Background(), f, dir); err != nil {
			t.Errorf("custom file %q: %v, want no error", content, err)
		}
		if l.ByAID(5).AID != 0 || l.ByAID(1).TVDB.ID != 100 || !l.Ready() {
			t.Errorf("custom file %q: entry 5 %+v, entry 1 %+v; want the lists alone", content, l.ByAID(5), l.ByAID(1))
		}
	}

	// A document cut off is still broken: reported, and the entries of the
	// last good version stay.
	if err := os.WriteFile(path, []byte(`<anime-list><anime anidbid="5" tvdbid="500" defaulttvdbseason="1"/></anime-list>`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Load(context.Background(), f, dir); err != nil || l.ByAID(5).TVDB.ID != 500 {
		t.Fatalf("custom file again: %v, entry 5 %+v", err, l.ByAID(5))
	}
	for _, cut := range []string{"<anime-list>", `<anime-list><anime anidbid="5"`, "\xef\xbb\xbf<?xml version=\"1.0\"?>\n<anime-list>\n<anime"} {
		if err := os.WriteFile(path, []byte(cut), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := l.Load(context.Background(), f, dir); err == nil || !strings.Contains(err.Error(), "anime-list-custom.xml ignored") {
			t.Errorf("custom file %q: %v, want it reported as broken", cut, err)
		}
		if l.ByAID(5).TVDB.ID != 500 {
			t.Errorf("custom file %q: entry 5 %+v, want the last good version kept", cut, l.ByAID(5))
		}
	}
}

// A zero-byte anime-list-custom.xml (an editor that truncates before it
// writes, caught in between) keeps the custom entries of the load before,
// with one warning per load, and never the lists' entries. With nothing to
// keep (the first load after a start, or a load before that had none) it is
// no entries and no warning. Deleting the file or leaving <anime-list/> in it
// turns the corrections off; a broken file is handled as before.
func TestLoadZeroByteCustomFile(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(lists{animeListsURL: goodAnimeLists, fribbURL: goodFribb})
	path := filepath.Join(dir, "anime-list-custom.xml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const good = `<anime-list><anime anidbid="1" tvdbid="200" defaulttvdbseason="1"/>` +
		`<anime anidbid="5" tvdbid="500" defaulttvdbseason="1"/></anime-list>`
	logs := captureWarnings(t)
	// load runs a load that must not fail and returns how many warnings
	// about an empty file it logged.
	load := func(l *Lists, what string) int {
		t.Helper()
		logs.Reset()
		if err := l.Load(context.Background(), f, dir); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return strings.Count(logs.String(), "anime-list-custom.xml is empty")
	}
	listsAlone := func(l *Lists, what string) {
		t.Helper()
		if l.ByAID(5).AID != 0 || l.ByAID(1).TVDB.ID != 100 || l.ByAID(2).MAL != 7 {
			t.Errorf("%s: entry 1 %+v, entry 5 %+v, entry 2 %+v; want the lists alone", what, l.ByAID(1), l.ByAID(5), l.ByAID(2))
		}
	}
	kept := func(l *Lists, what string) {
		t.Helper()
		if l.ByAID(1).TVDB.ID != 200 || l.ByAID(5).TVDB.ID != 500 || l.ByAID(2).MAL != 7 || !l.Ready() {
			t.Errorf("%s: entry 1 %+v, entry 5 %+v, entry 2 %+v; want the custom entries over the lists", what, l.ByAID(1), l.ByAID(5), l.ByAID(2))
		}
		// Kept as custom entries are the custom file's alone: aid 2 comes
		// from Fribb, aid 1's list entry is replaced, not merged.
		if c := l.cur.Load().custom; len(c) != 2 || c[1].TVDB.ID != 200 || c[5].TVDB.ID != 500 {
			t.Errorf("%s: custom entries %+v, want the file's two", what, c)
		}
	}

	// The first load after a start: nothing to keep.
	write("")
	l := &Lists{}
	if n := load(l, "first load, zero bytes"); n != 0 {
		t.Errorf("first load, zero bytes: %d warnings, want none\n%s", n, logs)
	}
	listsAlone(l, "first load, zero bytes")

	write(good)
	load(l, "custom file")
	kept(l, "custom file")
	write("")
	for i := range 2 { // kept on each load while it stays empty, warned each time
		if n := load(l, "zero bytes"); n != 1 || strings.Count(logs.String(), "level=WARN") != 1 ||
			!strings.Contains(logs.String(), "entries=2") {
			t.Errorf("zero bytes, load %d: want one warning naming 2 entries, got\n%s", i, logs)
		}
		kept(l, fmt.Sprintf("zero bytes, load %d", i))
	}
	// The next load decides again: a file written after a keep applies, as
	// it is, without a warning.
	write(`<anime-list><anime anidbid="5" tvdbid="600" defaulttvdbseason="1"/></anime-list>`)
	if n := load(l, "new file after zero bytes"); n != 0 {
		t.Errorf("new file after zero bytes: %d warnings, want none", n)
	}
	if l.ByAID(5).TVDB.ID != 600 || l.ByAID(1).TVDB.ID != 100 {
		t.Errorf("new file after zero bytes: entry 5 %+v, entry 1 %+v; want 600 and the lists' 100", l.ByAID(5), l.ByAID(1))
	}
	// And the first version written back after another keep.
	write("")
	if n := load(l, "zero bytes again"); n != 1 {
		t.Errorf("zero bytes again: %d warnings, want one", n)
	}
	write(good)
	if n := load(l, "good file back after zero bytes"); n != 0 {
		t.Errorf("good file back after zero bytes: %d warnings, want none", n)
	}
	kept(l, "good file back after zero bytes")
	// A broken file after a keep is reported, and the kept entries stay.
	write("")
	load(l, "zero bytes before broken")
	write(`<anime-list><anime anidbid="1"`)
	if err := l.Load(context.Background(), f, dir); err == nil || !strings.Contains(err.Error(), "anime-list-custom.xml ignored") {
		t.Errorf("broken after zero bytes: %v, want it reported", err)
	}
	kept(l, "broken after zero bytes")

	// A restart with the file still empty: nothing to keep.
	write("")
	l = &Lists{}
	if n := load(l, "restart, zero bytes"); n != 0 {
		t.Errorf("restart, zero bytes: %d warnings, want none", n)
	}
	listsAlone(l, "restart, zero bytes")

	// Turning the corrections off: delete the file, or leave <anime-list/>.
	// Zero bytes after either has nothing to keep.
	for _, off := range []string{"delete", "<anime-list/>"} {
		write(good)
		load(l, off+": custom file")
		kept(l, off+": custom file")
		if off == "delete" {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		} else {
			write(off)
		}
		if n := load(l, off); n != 0 {
			t.Errorf("%s: %d warnings, want none", off, n)
		}
		listsAlone(l, off)
		write("")
		if n := load(l, off+", then zero bytes"); n != 0 {
			t.Errorf("%s, then zero bytes: %d warnings, want none", off, n)
		}
		listsAlone(l, off+", then zero bytes")
	}
}

// fribbRows is a Fribb list of n good rows (AniDB ids 1 to n) and the given
// rows after them.
func fribbRows(n int, more ...string) string {
	var b strings.Builder
	b.WriteString("[")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `{"anidb_id": %d, "mal_id": %d},`, i, i+1000)
	}
	b.WriteString(strings.Join(more, ","))
	return strings.TrimSuffix(b.String(), ",") + "]"
}

// A Fribb row with a field of the wrong type is skipped, with one warning
// counting such rows; the rest of the list is used. Rows that a format change
// would break (more than 5, and more than 1 in 100) refuse the download
// instead, so the cached copy and the mapping stay. Broken JSON still fails
// the whole list.
func TestFribbSkipsBadRows(t *testing.T) {
	bad := `{"anidb_id": 500, "tvdb_id": "81797", "mal_id": 9}`
	logs := captureWarnings(t)
	list, err := parseFribb([]byte(fribbRows(199, bad, `{"anidb_id": 600, "mal_id": 6}`)))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 200 || list[600].MAL != 6 || list[199].MAL != 1199 || list[500].AID != 0 {
		t.Errorf("got %d entries, 600 %+v, 199 %+v, 500 %+v; want every row but the bad one", len(list), list[600], list[199], list[500])
	}
	if log := logs.String(); strings.Count(log, "level=WARN") != 1 || !strings.Contains(log, "list=fribb") ||
		!strings.Contains(log, "rows=1 of=201") || strings.Contains(log, "81797") {
		t.Errorf("want one warning counting the bad row, got:\n%s", log)
	}

	// A few bad rows never refuse a list by themselves, however small it is.
	five := []string{bad, bad, bad, bad, bad}
	for _, c := range []struct {
		name string
		list string
	}{{"1 bad of 50", fribbRows(49, bad)}, {"5 bad of 6", fribbRows(1, five...)}} {
		if list, err := parseFribb([]byte(c.list)); err != nil || list[1].MAL != 1001 {
			t.Errorf("%s: %v, entry 1 %+v; want the list used", c.name, err, list[1])
		}
	}
	// Refused: more than 5 bad rows and more than 1 in 100.
	six := append(five, bad)
	for _, c := range []struct {
		list, want string
	}{{fribbRows(1, six...), "6 of 7 rows"}, {fribbRows(593, six...), "6 of 599 rows"}} {
		if _, err := parseFribb([]byte(c.list)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s bad: %v, want the list refused", c.want, err)
		}
	}
	for _, c := range []struct {
		name string
		list string
	}{{"5 bad of 599 (at the floor)", fribbRows(594, five...)}, {"6 bad of 600 (1 in 100)", fribbRows(594, six...)}} {
		if _, err := parseFribb([]byte(c.list)); err != nil {
			t.Errorf("%s: %v, want the list used", c.name, err)
		}
	}
	for _, broken := range []string{fribbRows(5)[:20], fribbRows(5) + "x", `{"anidb_id": 1}`} {
		if _, err := parseFribb([]byte(broken)); err == nil {
			t.Errorf("%q: want an error", broken)
		}
	}

	// Refused while downloading: the cached copy stays, and so does the mapping.
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(lists{animeListsURL: goodAnimeLists, fribbURL: goodFribb})
	l := &Lists{}
	if err := l.Load(context.Background(), f, dir); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(dir, "cache", "mapping", "fribb-anime-list-mini.json")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(cached, old, old); err != nil {
		t.Fatal(err)
	}
	f.SetTransport(lists{animeListsURL: goodAnimeLists, fribbURL: fribbRows(1, six...)})
	_ = l.Load(context.Background(), f, dir)
	if b, _ := os.ReadFile(cached); string(b) != goodFribb || l.ByAID(2).MAL != 7 {
		t.Errorf("mostly broken download: cached %q, entry 2 %+v; want the good copy kept", b, l.ByAID(2))
	}
}

// A load cancelled while Kizuna stops doesn't parse or swap in what fetch
// answered with (here the fresh cached copy).
func TestLoadCancelledKeepsMapping(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(lists{animeListsURL: goodAnimeLists, fribbURL: goodFribb})
	if err := (&Lists{}).Load(context.Background(), f, dir); err != nil { // caches the lists
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := &Lists{}
	if err := l.Load(ctx, f, dir); err == nil {
		t.Error("cancelled load: want its error")
	}
	if l.cur.Load() != nil {
		t.Error("cancelled load swapped in a mapping")
	}
}
