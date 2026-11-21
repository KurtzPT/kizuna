package sources

import (
	"context"
	"fmt"
	"testing"

	"kizuna/internal/fetch"
)

// A MyAnimeList 404 is the id's "not there": the call fails with the 404
// (so the fields that wanted it see a missing entry, not a failed source),
// nothing is cached, and the client id is not taken as rejected: the next
// call, for that id or another, is sent.
func TestMALNotFound(t *testing.T) {
	f := fetch.New(t.TempDir())
	n := &tvdbNet{answers: map[string]string{
		fmt.Sprintf(malURL, 2): `{"id": 2, "synopsis": "Found."}`,
		// no answer for 1: tvdbNet answers 404
	}}
	f.SetTransport(n)
	m := NewMAL(f, "client")
	f.ThrottleAll(0)
	ctx := context.Background()

	for range 2 {
		if a, err := m.Anime(ctx, 1); !fetch.IsNotFound(err) {
			t.Fatalf("id 1: %+v %v, want its 404", a, err)
		}
	}
	if c := n.count(fmt.Sprintf(malURL, 1)); c != 2 {
		t.Errorf("id 1 asked %d times, want 2: a 404 is neither cached nor a rejected key", c)
	}
	if _, _, ok := f.Cached("mal/v2-1-full.json"); ok {
		t.Error("the 404 was cached")
	}
	if a, err := m.Anime(ctx, 2); err != nil || a.Synopsis != "Found." {
		t.Errorf("id 2 after the 404: %+v %v, want its answer", a, err)
	}
}
