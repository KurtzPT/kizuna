package sources

import (
	"context"
	"testing"

	"kizuna/internal/fetch"
)

// A season's overview comes from the season file the episode list reads:
// one request for both. A season TMDB doesn't have (404) gives no text.
func TestTMDBSeasonOverview(t *testing.T) {
	const q = "?api_key=key&language=en-US"
	f := fetch.New(t.TempDir())
	n := &tvdbNet{answers: map[string]string{ // tvdbNet answers any host
		"api.themoviedb.org/3/tv/77" + q:          `{"name":"Show","seasons":[{"season_number":1},{"season_number":2}]}`,
		"api.themoviedb.org/3/tv/77/season/1" + q: `{"overview":"","episodes":[{"episode_number":1,"name":"One"}]}`,
		"api.themoviedb.org/3/tv/77/season/2" + q: `{"overview":"The second cour.","episodes":[{"episode_number":1,"name":"Two"}]}`,
	}}
	f.SetTransport(n)
	tm := NewTMDB(f, "key")
	ctx := context.Background()
	if got, err := tm.SeasonOverview(ctx, 77, 2); err != nil || got != "The second cour." {
		t.Errorf("season 2: %q %v", got, err)
	}
	if got, err := tm.SeasonOverview(ctx, 77, 1); err != nil || got != "" {
		t.Errorf("season 1 (empty overview): %q %v", got, err)
	}
	if got, err := tm.SeasonOverview(ctx, 77, 3); got != "" || !fetch.IsNotFound(err) {
		t.Errorf("season 3: %q %v, want its 404", got, err)
	}
	if eps, err := tm.Episodes(ctx, 77); err != nil || len(eps) != 2 {
		t.Fatalf("episodes %+v %v", eps, err)
	}
	for _, s := range []string{"1", "2"} {
		if c := n.count("api.themoviedb.org/3/tv/77/season/" + s + q); c != 1 {
			t.Errorf("season %s asked %d times, want 1 (one cached file for both)", s, c)
		}
	}
}
