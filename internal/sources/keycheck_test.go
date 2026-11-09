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
	if _, err := tm.TV(ctx, 5); !fetch.IsNotFound(err) { // not in the answers: 404
		t.Fatalf("tv 5: %v, want its 404", err)
	}
	if _, err := tm.TV(ctx, 1); !unauthorized(err) {
		t.Fatalf("tv 1: %v, want the 401", err)
	}
	if _, err := tm.TV(ctx, 2); !unauthorized(err) {
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
	if s, err := tm.TV(ctx, 3); err != nil || s.Name != "Stale" {
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
	if _, err := tm.TV(ctx, 2); !unauthorized(err) || n.count("api.themoviedb.org/3/tv/2"+q) != 1 {
		t.Errorf("after the pause: %v, %d requests, want the 401 asked again", err, n.count("api.themoviedb.org/3/tv/2"+q))
	}
}

// MAL's rejected client id is remembered the same way.
func TestMALRejectedClientRemembered(t *testing.T) {
	f := fetch.New(t.TempDir())
	n := &tvdbNet{answers: map[string]string{
		fmt.Sprintf(malURL, 1): `403 {"error":"forbidden"}`,
		fmt.Sprintf(malURL, 2): `403 {"error":"forbidden"}`,
	}}
	f.SetTransport(n)
	m := NewMAL(f, "client")
	ctx := context.Background()
	for id := 1; id <= 2; id++ {
		if _, err := m.Anime(ctx, id); err == nil {
			t.Errorf("anime %d: no error, want the 403", id)
		}
	}
	if c := n.count(fmt.Sprintf(malURL, 2)); c != 0 {
		t.Errorf("anime 2 asked %d times, want 0 (the client id was rejected)", c)
	}
}
