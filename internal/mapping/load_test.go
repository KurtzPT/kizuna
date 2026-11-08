package mapping

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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
	first := reflect.ValueOf(l.byAID).UnsafePointer()
	expire()
	load() // 304
	if reflect.ValueOf(l.byAID).UnsafePointer() != first {
		t.Error("unchanged list was parsed and merged again")
	}
	if l.ByAID(1).TVDB != 100 {
		t.Fatalf("entry 1: %+v", l.ByAID(1))
	}

	custom := `<anime-list><anime anidbid="1" tvdbid="200" defaulttvdbseason="1"/></anime-list>`
	if err := os.WriteFile(filepath.Join(dir, "anime-list-custom.xml"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	expire()
	load()
	if l.ByAID(1).TVDB != 200 {
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
	if l.ByAID(1).TVDB != 200 || l.AIDFor("tvdb", "200") != 1 {
		t.Errorf("custom entry not applied without a list: %+v", l.ByAID(1))
	}
	if l.Ready() {
		t.Error("custom entries alone must not make the mapping Ready")
	}

	f.SetTransport(notModified{})
	if err := l.Load(context.Background(), f, dir, []string{"animelists"}); err != nil {
		t.Fatal(err)
	}
	if !l.Ready() || l.ByAID(1).TVDB != 200 {
		t.Errorf("after the list loaded: ready %v, entry 1 %+v (custom wins)", l.Ready(), l.ByAID(1))
	}
}
