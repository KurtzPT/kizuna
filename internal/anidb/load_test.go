package anidb

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

// dump answers every request with its body (no ETag).
type dump string

func (d dump) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(d))), Header: http.Header{}, Request: r}, nil
}

const goodDump = `<animetitles><anime aid="1"><title type="main" xml:lang="x-jat">Cowboy Bebop</title></anime></animetitles>`

// A title dump that doesn't parse (or has no anime) is rejected before it's
// cached: the good cached copy and the loaded titles stay.
func TestLoadRejectsBrokenDump(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(dir)
	f.SetTransport(dump(goodDump))
	ts := &Titles{}
	if err := ts.Load(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(dir, "anidb", "animetitles.xml")
	for _, bad := range []string{`<animetitles><anime aid="1" & /></animetitles>`, `<animetitles></animetitles>`} {
		old := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(cached, old, old); err != nil {
			t.Fatal(err)
		}
		f.SetTransport(dump(bad))
		if err := ts.Load(context.Background(), f); err != nil {
			t.Errorf("%s: %v, want the cached copy used", bad, err)
		}
		if b, _ := os.ReadFile(cached); string(b) != goodDump {
			t.Errorf("%s replaced the cached copy: %q", bad, b)
		}
		if got := ts.Of(1); len(got) != 1 || got[0].Text != "Cowboy Bebop" {
			t.Errorf("%s: titles %v", bad, got)
		}
	}
	empty := fetch.New(t.TempDir())
	empty.SetTransport(dump(`<animetitles><anime aid="1" & /></animetitles>`))
	if err := (&Titles{}).Load(context.Background(), empty); err == nil {
		t.Error("broken dump, no cached copy: want an error")
	}
}

// A load cancelled while Kizuna stops doesn't parse or swap in what fetch
// answered with (here the fresh cached copy).
func TestLoadCancelledKeepsTitles(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(dir)
	f.SetTransport(dump(goodDump))
	if err := (&Titles{}).Load(context.Background(), f); err != nil { // caches the dump
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ts := &Titles{}
	if err := ts.Load(ctx, f); err == nil {
		t.Error("cancelled load: want its error")
	}
	if ts.Ready() {
		t.Error("cancelled load swapped in a title list")
	}
}
