package sources

import (
	"context"
	"strings"
	"testing"

	"kizuna/internal/fetch"
)

// A season's overview comes from the season file the episode list reads:
// one request for both. A season the show doesn't list gives no text and
// isn't asked (a 404 isn't cached); one it lists but doesn't have, its 404.
func TestTMDBSeasonOverview(t *testing.T) {
	const q = "?api_key=key&language=en-US"
	const sq = q + "&append_to_response=images&include_image_language=en,ja,null"
	f := fetch.New(t.TempDir())
	n := &tvdbNet{answers: map[string]string{ // tvdbNet answers any host
		"api.themoviedb.org/3/tv/77" + q:           `{"name":"Show","seasons":[{"season_number":1},{"season_number":2},{"season_number":4}]}`,
		"api.themoviedb.org/3/tv/77/season/1" + sq: `{"overview":"","episodes":[{"episode_number":1,"name":"One"}]}`,
		"api.themoviedb.org/3/tv/77/season/2" + sq: `{"overview":"The second cour.","episodes":[{"episode_number":1,"name":"Two"}]}`,
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
	if got, err := tm.SeasonOverview(ctx, 77, 3); got != "" || err != nil {
		t.Errorf("season 3 (not listed): %q %v, want none", got, err)
	}
	if c := n.count("api.themoviedb.org/3/tv/77/season/3" + sq); c != 0 {
		t.Errorf("season 3, not listed, asked %d times", c)
	}
	if got, err := tm.SeasonOverview(ctx, 77, 4); got != "" || !fetch.IsNotFound(err) {
		t.Errorf("season 4 (listed, missing): %q %v, want its 404", got, err)
	}
	if got, err := tm.SeasonOverview(ctx, 78, 1); got != "" || !fetch.IsNotFound(err) {
		t.Errorf("unknown show: %q %v, want its 404", got, err)
	}
	if eps, _, err := tm.EpisodesAndPosters(ctx, 77); err != nil || len(eps) != 2 {
		t.Fatalf("episodes %+v %v", eps, err)
	}
	for _, s := range []string{"1", "2"} {
		if c := n.count("api.themoviedb.org/3/tv/77/season/" + s + sq); c != 1 {
			t.Errorf("season %s asked %d times, want 1 (one cached file for both)", s, c)
		}
	}
}

// A season's posters come appended to the season file the episode list
// reads (append_to_response=images): no request of their own, and the
// episode list has them too. A season the show doesn't list isn't asked.
func TestTMDBSeasonArt(t *testing.T) {
	const q = "?api_key=key&language=en-US&append_to_response=images&include_image_language=en,ja,null"
	f := fetch.New(t.TempDir())
	n := &tvdbNet{answers: map[string]string{
		"api.themoviedb.org/3/tv/77?api_key=key&language=en-US": `{"name":"Show","seasons":[{"season_number":2}]}`,
		"api.themoviedb.org/3/tv/77/season/2" + q: `{"overview":"","episodes":[{"episode_number":1,"name":"Two"}],
			"images":{"id":2,"posters":[{"aspect_ratio":0.667,"height":3000,"iso_639_1":"en","file_path":"/s2a.jpg","vote_average":5.3,"vote_count":2,"width":2000},
			{"aspect_ratio":0.667,"height":3000,"iso_639_1":null,"file_path":"/s2b.jpg","vote_average":0,"vote_count":0,"width":2000}]}}`,
	}}
	f.SetTransport(n)
	tm := NewTMDB(f, "key")
	ctx := context.Background()
	_, posters, err := tm.EpisodesAndPosters(ctx, 77)
	if err != nil || strings.Join(posters[2], " ") != tmdbImg+"/s2a.jpg "+tmdbImg+"/s2b.jpg" || len(posters) != 1 {
		t.Fatalf("episode list's posters %v %v", posters, err)
	}
	a, err := tm.SeasonArt(ctx, 77, 2)
	if err != nil || strings.Join(a.Posters, " ") != tmdbImg+"/s2a.jpg "+tmdbImg+"/s2b.jpg" || len(a.Backgrounds)+len(a.Logos) != 0 {
		t.Errorf("season 2 art %+v %v", a, err)
	}
	if c := n.count("api.themoviedb.org/3/tv/77/season/2" + q); c != 1 {
		t.Errorf("season 2 asked %d times, want 1 (the episode list's file)", c)
	}
	if a, err := tm.SeasonArt(ctx, 77, 0); len(a.Posters) != 0 || err != nil {
		t.Errorf("season 0: %+v %v, want none", a, err)
	}
	if c := n.count("api.themoviedb.org/3/tv/77/season/0" + q); c != 0 {
		t.Errorf("season 0, not listed, asked %d times", c)
	}
	if a, err := tm.SeasonArt(ctx, 78, 1); len(a.Posters) != 0 || !fetch.IsNotFound(err) {
		t.Errorf("unknown show: %+v %v, want its 404", a, err)
	}
}

// A movie's title and overview come from its details request: one request
// and one cached file for Text and Details.
func TestTMDBMovieTextFromDetails(t *testing.T) {
	const u = "api.themoviedb.org/3/movie/99?api_key=key&language=en-US&append_to_response=credits,release_dates"
	f := fetch.New(t.TempDir())
	n := &tvdbNet{answers: map[string]string{
		u: `{"title":"Movie","overview":"About it.","release_date":"2001-07-20","genres":[{"id":16,"name":"Animation"}]}`,
	}}
	f.SetTransport(n)
	tm := NewTMDB(f, "key")
	ctx := context.Background()
	if name, overview, err := tm.Text(ctx, 99, true); err != nil || name != "Movie" || overview != "About it." {
		t.Fatalf("text %q %q %v", name, overview, err)
	}
	if d, err := tm.Details(ctx, 99, true); err != nil || d.Date != "2001-07-20" || len(d.Genres) != 1 {
		t.Fatalf("details %+v %v", d, err)
	}
	if len(n.asked) != 1 {
		t.Errorf("asked %v, want only the details request", n.asked)
	}
}
