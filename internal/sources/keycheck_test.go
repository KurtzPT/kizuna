package sources

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

// A rejected TMDB key (401) is remembered: the next calls send nothing and
// fail the same way, a cached copy still answers, and a 404 blocks nothing.
func TestTMDBRejectedKeyRemembered(t *testing.T) {
	const q = "?api_key=key&language=en-US"
	dir := t.TempDir()
	f := fetch.New(dir)
	n := &tvdbNet{answers: map[string]string{
		"api.themoviedb.org/3/tv/1" + q: `401 {"status_code":7}`,
		"api.themoviedb.org/3/tv/2" + q: `401 {"status_code":7}`,
		"api.themoviedb.org/3/tv/3" + q: `401 {"status_code":7}`,
	}}
	f.SetTransport(n)
	tm := NewTMDB(f, "key")
	ctx := context.Background()
	if _, err := tm.tv(ctx, 5); !fetch.IsNotFound(err) { // not in the answers: 404
		t.Fatalf("tv 5: %v, want its 404", err)
	}
	if _, err := tm.tv(ctx, 1); !unauthorized(err) {
		t.Fatalf("tv 1: %v, want the 401", err)
	}
	if _, err := tm.tv(ctx, 2); !unauthorized(err) {
		t.Fatalf("tv 2: %v, want the remembered 401", err)
	}
	p := filepath.Join(dir, "tmdb", "tv-3.json")
	if err := fetch.WriteFile(p, []byte(`{"name":"Stale"}`)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if s, err := tm.tv(ctx, 3); err != nil || s.Name != "Stale" {
		t.Fatalf("tv 3: %q %v, want the stale copy", s.Name, err)
	}
	for id, want := range map[int]int{1: 1, 2: 0, 3: 0} {
		if c := n.count(fmt.Sprintf("api.themoviedb.org/3/tv/%d", id) + q); c != want {
			t.Errorf("tv %d asked %d times, want %d", id, c, want)
		}
	}

	tm.keys.mu.Lock()
	tm.keys.until = time.Now() // the pause is over
	tm.keys.mu.Unlock()
	if _, err := tm.tv(ctx, 2); !unauthorized(err) || n.count("api.themoviedb.org/3/tv/2"+q) != 1 {
		t.Errorf("after the pause: %v, %d requests, want the 401 asked again", err, n.count("api.themoviedb.org/3/tv/2"+q))
	}
}

// MAL's rejected client id is remembered the same way. MyAnimeList answers
// a wrong client id with 400 and this body (seen live), not 401/403.
func TestMALRejectedClientRemembered(t *testing.T) {
	const bad = `400 {"message":"Invalid client id","error":"bad_request"}`
	f := fetch.New(t.TempDir())
	n := &tvdbNet{answers: map[string]string{
		fmt.Sprintf(malURL, 1): bad,
		fmt.Sprintf(malURL, 2): bad,
	}}
	f.SetTransport(n)
	m := NewMAL(f, "client")
	ctx := context.Background()
	for id := 1; id <= 2; id++ {
		if _, err := m.Anime(ctx, id); err == nil {
			t.Errorf("anime %d: no error, want the 400", id)
		}
	}
	if c := n.count(fmt.Sprintf(malURL, 2)); c != 0 {
		t.Errorf("anime 2 asked %d times, want 0 (the client id was rejected)", c)
	}
}

// Another 400 from MyAnimeList says nothing about the client id: the next
// call still asks.
func TestMALOtherBadRequestNotRemembered(t *testing.T) {
	const other = `400 {"message":"invalid q","error":"bad_request"}`
	f := fetch.New(t.TempDir())
	n := &tvdbNet{answers: map[string]string{
		fmt.Sprintf(malURL, 1): other,
		fmt.Sprintf(malURL, 2): other,
	}}
	f.SetTransport(n)
	m := NewMAL(f, "client")
	ctx := context.Background()
	for id := 1; id <= 2; id++ {
		if _, err := m.Anime(ctx, id); err == nil {
			t.Errorf("anime %d: no error, want the 400", id)
		}
	}
	if c := n.count(fmt.Sprintf(malURL, 2)); c != 1 {
		t.Errorf("anime 2 asked %d times, want 1 (not a rejected client id)", c)
	}
}
