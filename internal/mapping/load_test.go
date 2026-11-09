package mapping

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

// notModified answers Anime-Lists' URL with one list and an ETag, and 304
// when asked with that ETag. Requests only come from the Load under test.
type notModified struct{}

func (notModified) RoundTrip(r *http.Request) (*http.Response, error) {
	h := http.Header{"Etag": {`"v1"`}}
	if r.Header.Get("If-None-Match") == `"v1"` {
		return &http.Response{StatusCode: http.StatusNotModified, Body: io.NopCloser(strings.NewReader("")), Header: h, Request: r}, nil
	}
	body := `<anime-list><anime anidbid="1" tvdbid="100" defaulttvdbseason="1"/></anime-list>`
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
		if err := l.Load(context.Background(), f, dir, []string{"animelists"}); err != nil {
			t.Fatal(err)
		}
	}
	expire := func() { // the daily reload: the cached copy has expired
		old := time.Now().Add(-48 * time.Hour)
		_ = os.Chtimes(filepath.Join(dir, "cache", "mapping", "anime-list-master.xml"), old, old)
	}
	load()
	first := l.cur.Load()
	expire()
	load() // 304
	if l.cur.Load() != first {
		t.Error("unchanged list was parsed and merged again")
	}
	if l.ByAID(1).TVDB.ID != 100 {
		t.Fatalf("entry 1: %+v", l.ByAID(1))
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
	if err := l.Load(context.Background(), f, dir, []string{"animelists"}); err == nil {
		t.Error("no list loaded: want an error to log")
	}
	if l.ByAID(1).TVDB.ID != 200 || l.AIDFor("tvdb", "200") != 1 {
		t.Errorf("custom entry not applied without a list: %+v", l.ByAID(1))
	}
	if l.Ready() {
		t.Error("custom entries alone must not make the mapping Ready")
	}

	f.SetTransport(notModified{})
	if err := l.Load(context.Background(), f, dir, []string{"animelists"}); err != nil {
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

// A download that doesn't parse is rejected before it's cached: the good
// cached copy stays, and so does the mapping. With no copy at all, the list
// counts as failed.
func TestLoadRejectsBrokenDownload(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(lists{animeListsURL: goodAnimeLists})
	l := &Lists{}
	if err := l.Load(context.Background(), f, dir, []string{"animelists"}); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(dir, "cache", "mapping", "anime-list-master.xml")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(cached, old, old); err != nil {
		t.Fatal(err)
	}
	f.SetTransport(lists{animeListsURL: brokenAnimeLists})
	if err := l.Load(context.Background(), f, dir, []string{"animelists"}); err != nil {
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
	f2.SetTransport(lists{animeListsURL: brokenAnimeLists})
	if err := (&Lists{}).Load(context.Background(), f2, empty, []string{"animelists"}); err == nil {
		t.Error("broken download and no cached copy: want an error")
	}
	f2.SetTransport(lists{animeListsURL: `<anime-list></anime-list>`})
	if err := (&Lists{}).Load(context.Background(), f2, empty, []string{"animelists"}); err == nil {
		t.Error("a list without entries: want an error")
	}
}

// Once Ready, a list that fails (here a cached copy that doesn't parse) keeps
// the loaded mapping instead of replacing it with the other list alone. On a
// first load the other list is still used.
func TestLoadKeepsMappingWhenAListFails(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(lists{animeListsURL: goodAnimeLists, fribbURL: goodFribb})
	order := []string{"animelists", "fribb"}
	l := &Lists{}
	if err := l.Load(context.Background(), f, dir, order); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(dir, "cache", "mapping", "anime-list-master.xml")
	if err := os.WriteFile(cached, []byte(brokenAnimeLists), 0o644); err != nil { // fresh: no download
		t.Fatal(err)
	}
	err := l.Load(context.Background(), f, dir, order)
	if err == nil || !strings.Contains(err.Error(), "keeping") {
		t.Errorf("a list failed: %v, want an error saying the mapping is kept", err)
	}
	if !l.ByAID(1).TVDB.HasException(1, 5) || l.ByAID(2).MAL != 7 || !l.Ready() {
		t.Errorf("mapping not kept: 1 %+v, 2 %+v", l.ByAID(1), l.ByAID(2))
	}

	first := &Lists{}
	if err := first.Load(context.Background(), f, dir, order); err == nil {
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
	order := []string{"animelists", "fribb"}
	l := &Lists{}
	if err := l.Load(context.Background(), f, dir, order); err == nil {
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
	err := l.Load(context.Background(), f, dir, order)
	if err == nil || strings.Contains(err.Error(), "keeping") {
		t.Errorf("fribb still fails: %v, want its error but not the mapping kept", err)
	}
	if l.ByAID(1).TVDB.ID != 300 || l.ByAID(3).TVDB.ID != 400 || l.ByAID(5).TVDB.ID != 500 || !l.Ready() {
		t.Errorf("changes not applied: 1 %+v, 3 %+v, 5 %+v", l.ByAID(1), l.ByAID(3), l.ByAID(5))
	}
}

// A load cancelled while Kizuna stops doesn't parse or swap in what fetch
// answered with (here the fresh cached copy).
func TestLoadCancelledKeepsMapping(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(filepath.Join(dir, "cache"))
	f.SetTransport(lists{animeListsURL: goodAnimeLists})
	order := []string{"animelists"}
	if err := (&Lists{}).Load(context.Background(), f, dir, order); err != nil { // caches the list
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := &Lists{}
	if err := l.Load(ctx, f, dir, order); err == nil {
		t.Error("cancelled load: want its error")
	}
	if l.cur.Load() != nil {
		t.Error("cancelled load swapped in a mapping")
	}
}
