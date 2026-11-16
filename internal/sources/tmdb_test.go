package sources

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

// A season's overview comes from the season file the episode list reads:
// one request for both. A season the show doesn't list gives no text and
// isn't asked; one it lists but doesn't have, its 404 (remembered a day).
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

// A movie or show TheMovieDb has no record of (404) is asked once a day,
// not by each read of a build (a movie's text and details share one
// address) nor at every build. Its images likewise.
func TestTMDBMissingRecordRemembered(t *testing.T) {
	const (
		movie  = "api.themoviedb.org/3/movie/98?api_key=key&language=en-US&append_to_response=credits,release_dates"
		images = "api.themoviedb.org/3/movie/98/images?api_key=key&include_image_language=en,ja,null"
		show   = "api.themoviedb.org/3/tv/78?api_key=key&language=en-US"
	)
	dir := t.TempDir()
	f := fetch.New(dir)
	n := &tvdbNet{answers: map[string]string{}} // all three: 404
	f.SetTransport(n)
	tm := NewTMDB(f, "key")
	f.ThrottleAll(0)
	ctx := context.Background()
	build := func() {
		t.Helper()
		if _, _, err := tm.Text(ctx, 98, true); !fetch.IsNotFound(err) {
			t.Errorf("movie text: %v, want its 404", err)
		}
		if _, err := tm.Details(ctx, 98, true); !fetch.IsNotFound(err) {
			t.Errorf("movie details: %v, want its 404", err)
		}
		if _, err := tm.Art(ctx, 98, true); !fetch.IsNotFound(err) {
			t.Errorf("movie art: %v, want its 404", err)
		}
		if _, _, err := tm.Text(ctx, 78, false); !fetch.IsNotFound(err) {
			t.Errorf("show text: %v, want its 404", err)
		}
		if _, _, err := tm.EpisodesAndPosters(ctx, 78); !fetch.IsNotFound(err) {
			t.Errorf("show episodes: %v, want its 404", err)
		}
	}
	for b := 1; b <= 2; b++ {
		build()
		for _, u := range []string{movie, images, show} {
			if c := n.count(u); c != 1 {
				t.Errorf("build %d: %s asked %d times, want 1", b, u, c)
			}
		}
	}
	backdate(t, filepath.Join(dir, "tmdb", "movie-98-details.json.notfound"), noneTTL)
	build()
	if c := n.count(movie); c != 2 {
		t.Errorf("after a day: the movie asked %d times in all, want 2", c)
	}
}

// A movie whose cached copy expired still gets that copy when TheMovieDb now
// answers 404: the remembered 404 never hides it, so it is asked at each
// build. A 401 (a wrong key) is not remembered as "no such movie".
func TestTMDBMissingRecordStaleCopy(t *testing.T) {
	const movie = "api.themoviedb.org/3/movie/99?api_key=key&language=en-US&append_to_response=credits,release_dates"
	dir := t.TempDir()
	f := fetch.New(dir)
	n := &tvdbNet{answers: map[string]string{movie: `{"title":"Movie","overview":"About it."}`}}
	f.SetTransport(n)
	tm := NewTMDB(f, "key")
	f.ThrottleAll(0)
	ctx := context.Background()
	if name, _, err := tm.Text(ctx, 99, true); err != nil || name != "Movie" {
		t.Fatalf("text %q %v", name, err)
	}
	backdate(t, filepath.Join(dir, "tmdb", "movie-99-details.json"), detailsTTL)
	n.mu.Lock()
	delete(n.answers, movie) // deleted on TheMovieDb: 404
	n.mu.Unlock()
	for b := 1; b <= 2; b++ {
		if name, _, err := tm.Text(ctx, 99, true); err != nil || name != "Movie" {
			t.Errorf("build %d: %q %v, want the stale copy", b, name, err)
		}
		if c := n.count(movie); c != 1+b {
			t.Errorf("build %d: asked %d times in all, want %d", b, c, 1+b)
		}
	}

	captureLogs(t) // the rejected key's error
	n.mu.Lock()
	n.answers["api.themoviedb.org/3/movie/97?api_key=key&language=en-US&append_to_response=credits,release_dates"] =
		`401 {"status_code": 7, "status_message": "Invalid API key: You must be granted a valid key."}`
	n.mu.Unlock()
	if _, _, err := tm.Text(ctx, 97, true); err == nil || fetch.IsNotFound(err) {
		t.Errorf("wrong key: %v, want the 401", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tmdb", "movie-97-details.json.notfound")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the 401 is remembered (stat: %v), want nothing kept", err)
	}
}

// findTest is a TMDB client whose find answers come from n, with helpers
// to age a cached find answer and to check what TVFromTVDB gives.
type findTest struct {
	t   *testing.T
	tm  *TMDB
	n   *tvdbNet
	dir string
}

func newFindTest(t *testing.T, answers map[string]string) *findTest {
	dir := t.TempDir()
	f := fetch.New(dir)
	n := &tvdbNet{answers: answers}
	f.SetTransport(n)
	tm := NewTMDB(f, "key")
	f.ThrottleAll(0)
	return &findTest{t, tm, n, dir}
}

func findURL(tvdb int) string {
	return fmt.Sprintf("api.themoviedb.org/3/find/%d?api_key=key&external_source=tvdb_id", tvdb)
}

func (ft *findTest) answer(tvdb int, body string) {
	ft.n.mu.Lock()
	ft.n.answers[findURL(tvdb)] = body
	ft.n.mu.Unlock()
}

func (ft *findTest) age(tvdb int, d time.Duration) {
	ft.t.Helper()
	at := time.Now().Add(-d)
	if err := os.Chtimes(ft.path(tvdb), at, at); err != nil {
		ft.t.Fatal(err)
	}
}

func (ft *findTest) path(tvdb int) string {
	return filepath.Join(ft.dir, "tmdb", fmt.Sprintf("find-tvdb-%d.json", tvdb))
}

// find checks TVFromTVDB gives want and no error (the show stays complete),
// and that find has been asked asked times in all.
func (ft *findTest) find(tvdb, want, asked int, why string) {
	ft.t.Helper()
	if id, err := ft.tm.TVFromTVDB(context.Background(), tvdb); id != want || err != nil {
		ft.t.Fatalf("%s: %d %v, want %d and no error", why, id, err, want)
	}
	if c := ft.n.count(findURL(tvdb)); c != asked {
		ft.t.Fatalf("%s: asked %d times in all, want %d", why, c, asked)
	}
}

// TMDB's find for a TVDB id: an empty answer (the show isn't linked on TMDB
// yet) is asked again once it is a day old; the id TMDB gives then is used
// and kept 30 days. An empty copy past 30 days that answers stale for a
// failed request isn't asked for twice.
func TestTMDBFindNoneAskedAgainAfterADay(t *testing.T) {
	ft := newFindTest(t, map[string]string{findURL(1): `{"tv_results":[]}`, findURL(2): `{"tv_results":[]}`})
	ft.find(1, 0, 1, "empty answer")
	ft.find(1, 0, 1, "empty answer, cached")
	ft.age(1, 25*time.Hour)
	ft.find(1, 0, 2, "empty answer a day old")
	ft.answer(1, `{"tv_results":[{"id":77}]}`) // linked on TMDB since
	ft.age(1, 25*time.Hour)
	ft.find(1, 77, 3, "linked since")
	ft.age(1, 25*time.Hour)
	ft.find(1, 77, 3, "found id a day old (kept 30 days)")

	ft.find(2, 0, 1, "empty answer")
	ft.answer(2, `500 {}`)
	ft.age(2, 31*24*time.Hour)
	ft.find(2, 0, 2, "empty answer past 30 days, TMDB down (the stale copy answers, asked once)")
}

// A re-ask of a day-old empty answer that fails leaves the empty answer and
// no error: TMDB said it has no such show, so the show stays complete.
func TestTMDBFindNoneReaskFailing(t *testing.T) {
	ft := newFindTest(t, map[string]string{findURL(1): `{"tv_results":[]}`})
	ft.find(1, 0, 1, "empty answer")
	ft.answer(1, `500 {}`)
	ft.age(1, 25*time.Hour)
	ft.find(1, 0, 2, "TMDB down (the copy answers stale)")

	ft.tm.keys.err, ft.tm.keys.until = errors.New("rejected"), time.Now().Add(time.Hour)
	ft.age(1, 25*time.Hour)
	ft.find(1, 0, 2, "key rejected lately (no request)")
	ft.tm.keys.err, ft.tm.keys.until = nil, time.Time{}

	// The copy goes (the cache cleanup) between the two reads: the download
	// fails with no copy to answer for it. Test code removing its own file.
	ft.age(1, 25*time.Hour)
	ft.tm.f.SetTransport(pruneThenFail{ft, ft.path(1)})
	ft.find(1, 0, 3, "copy pruned, TMDB down")
}

// pruneThenFail removes a cache file, then answers as the fake does.
type pruneThenFail struct {
	ft   *findTest
	path string
}

func (p pruneThenFail) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := os.Remove(p.path); err != nil {
		p.ft.t.Error(err)
	}
	return p.ft.n.RoundTrip(r)
}

// A season the show lists but TMDB answers 404 for is asked once a day, not
// by each read of a build (episode list, season text, season posters) nor
// at every build.
func TestTMDBMissingSeasonRemembered(t *testing.T) {
	const q = "?api_key=key&language=en-US"
	const sq = q + "&append_to_response=images&include_image_language=en,ja,null"
	const s0 = "api.themoviedb.org/3/tv/77/season/0" + sq
	dir := t.TempDir()
	f := fetch.New(dir)
	n := &tvdbNet{answers: map[string]string{
		"api.themoviedb.org/3/tv/77" + q:           `{"name":"Show","seasons":[{"season_number":0},{"season_number":1}]}`,
		"api.themoviedb.org/3/tv/77/season/1" + sq: `{"episodes":[{"episode_number":1,"name":"One"}]}`,
		// season 0: 404
	}}
	f.SetTransport(n)
	tm := NewTMDB(f, "key")
	f.ThrottleAll(0)
	ctx := context.Background()
	build := func() {
		t.Helper()
		if eps, _, err := tm.EpisodesAndPosters(ctx, 77); err != nil || len(eps) != 1 {
			t.Errorf("episodes %+v %v, want season 1's and no error", eps, err)
		}
		if a, err := tm.SeasonArt(ctx, 77, 0); len(a.Posters) != 0 || !fetch.IsNotFound(err) {
			t.Errorf("season 0 art %+v %v, want its 404", a, err)
		}
		if got, err := tm.SeasonOverview(ctx, 77, 0); got != "" || !fetch.IsNotFound(err) {
			t.Errorf("season 0 text %q %v, want its 404", got, err)
		}
	}
	for b := 1; b <= 3; b++ {
		build()
		if c := n.count(s0); c != 1 {
			t.Errorf("build %d: season 0 asked %d times, want 1", b, c)
		}
	}
	backdate(t, filepath.Join(dir, "tmdb", "tv-77-s0-images.json.notfound"), 24*time.Hour)
	build()
	if c := n.count(s0); c != 2 {
		t.Errorf("after a day: season 0 asked %d times in all, want 2", c)
	}
}
