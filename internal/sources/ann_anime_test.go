package sources

import (
	"context"
	"testing"

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
