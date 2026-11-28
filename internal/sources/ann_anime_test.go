package sources

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

const annURL = "cdn.animenewsnetwork.com/encyclopedia/api.xml?anime="

// ANN.Anime end to end: it asks ANN's encyclopedia for the id, decodes the
// entry and caches it for a week, so a second call sends nothing. ANN's
// "no result" answer is an empty entry, not an error: the show just has
// nothing from ANN. It is cached like any answer. An answer that isn't
// ANN's XML (an error page) is an error and isn't cached. No id: no request.
func TestANNAnime(t *testing.T) {
	f := fetch.New(t.TempDir())
	n := &tvdbNet{answers: map[string]string{
		annURL + "13": annSample,
		annURL + "9":  `<ann><warning>no result for anime=9</warning></ann>`,
		annURL + "5":  `<html><body>Service Unavailable</body></html>`,
	}}
	f.SetTransport(n)
	ann := NewANN(f)
	f.ThrottleAll(0)
	ctx := context.Background()

	for range 2 {
		a, err := ann.Anime(ctx, 13)
		if err != nil {
			t.Fatal(err)
		}
		if a.Title != "Cowboy Bebop" || a.Poster != "https://www.animenewsnetwork.com/images/encyc/A13-15.jpg" ||
			a.Details.Studio != "Sunrise" || a.Details.Date != "1998-04-03" {
			t.Errorf("entry 13: %+v", a)
		}
	}
	if c := n.count(annURL + "13"); c != 1 {
		t.Errorf("entry 13 asked %d times, want 1 (then the cache)", c)
	}
	if _, _, ok := f.Cached("ann/13.xml"); !ok {
		t.Error("entry 13 not cached under ann/13.xml")
	}

	for range 2 {
		a, err := ann.Anime(ctx, 9)
		if err != nil || a.Title != "" || a.Summary != "" || a.Poster != "" {
			t.Errorf("no result: %+v %v, want an empty entry and no error", a, err)
		}
	}
	if c := n.count(annURL + "9"); c != 1 {
		t.Errorf("no result asked %d times, want 1 (then the cache)", c)
	}

	if a, err := ann.Anime(ctx, 5); err == nil {
		t.Errorf("an error page: %+v, want an error", a)
	}
	if _, _, ok := f.Cached("ann/5.xml"); ok {
		t.Error("an error page was cached")
	}

	if _, err := ann.Anime(ctx, 0); err == nil {
		t.Error("id 0: want an error")
	}
	if c := n.count(annURL + "0"); c != 0 {
		t.Errorf("id 0 sent %d requests, want none", c)
	}
}

// A truncated or malformed answer that still starts with "<ann" is rejected
// before caching, so a later request fetches fresh data instead of failing
// from the cache.
func TestANNBrokenXMLNotCached(t *testing.T) {
	f := fetch.New(t.TempDir())
	n := &tvdbNet{answers: map[string]string{
		annURL + "13": `<ann><anime id="13"><info type="Main title">Cowboy`,
		annURL + "14": `<ann><anime id="14"><info type="Main title">A & B</info></anime></ann>`,
	}}
	f.SetTransport(n)
	ann := NewANN(f)
	f.ThrottleAll(0)
	ctx := context.Background()

	if a, err := ann.Anime(ctx, 13); err == nil {
		t.Fatalf("truncated XML: got %+v, want an error", a)
	}
	if _, _, ok := f.Cached("ann/13.xml"); ok {
		t.Error("truncated XML was cached")
	}

	if a, err := ann.Anime(ctx, 14); err == nil {
		t.Fatalf("malformed XML: got %+v, want an error", a)
	}
	if _, _, ok := f.Cached("ann/14.xml"); ok {
		t.Error("malformed XML was cached")
	}

	n.answers[annURL+"13"] = annSample
	a, err := ann.Anime(ctx, 13)
	if err != nil {
		t.Fatalf("retry after truncated XML: %v", err)
	}
	if a.Title != "Cowboy Bebop" {
		t.Errorf("retry title = %q, want %q", a.Title, "Cowboy Bebop")
	}
	if c := n.count(annURL + "13"); c != 2 {
		t.Errorf("entry 13 asked %d times, want 2", c)
	}
}

// When a refresh returns truncated XML starting with "<ann", an expired
// cached copy stays on disk and is served.
func TestANNBrokenRefreshKeepsExpiredCache(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(dir)
	n := &tvdbNet{answers: map[string]string{
		annURL + "13": annSample,
	}}
	f.SetTransport(n)
	ann := NewANN(f)
	f.ThrottleAll(0)
	ctx := context.Background()

	a, err := ann.Anime(ctx, 13)
	if err != nil {
		t.Fatal(err)
	}
	if a.Title != "Cowboy Bebop" {
		t.Fatalf("initial title = %q, want %q", a.Title, "Cowboy Bebop")
	}

	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "ann", "13.xml"), old, old); err != nil {
		t.Fatal(err)
	}
	n.answers[annURL+"13"] = `<ann><anime id="13"><info type="Main title">Cut`

	a, err = ann.Anime(ctx, 13)
	if err != nil {
		t.Fatalf("refresh with truncated XML: %v", err)
	}
	if a.Title != "Cowboy Bebop" {
		t.Errorf("stale fallback title = %q, want %q", a.Title, "Cowboy Bebop")
	}
	if c := n.count(annURL + "13"); c != 2 {
		t.Errorf("entry 13 asked %d times, want 2", c)
	}
	cached, _, ok := f.Cached("ann/13.xml")
	if !ok || string(cached) != annSample {
		t.Errorf("cached copy was overwritten: ok=%v len=%d", ok, len(cached))
	}
}
