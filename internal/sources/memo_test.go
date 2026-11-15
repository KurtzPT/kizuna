package sources

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

// scramble replaces a cache file's bytes with JSON cut short, keeping its
// time: a read that decodes the file again finds it broken and downloads it
// (one more request), so the answers say whether it was decoded.
func scramble(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"cut`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
}

// rewrite writes a cache file with new bytes and a new time, a minute
// earlier: still fresh, and apart on a file system that keeps 2 s steps.
func rewrite(t *testing.T, path, body string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	at := st.ModTime().Add(-time.Minute)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// What a later /anidb entry's build reads of TheMovieDb (the show's season
// list for its text, the list, the season's text and art; the season file
// for the last three) decodes each file once: a read after the first finds
// it decoded, and later builds decode nothing while the files keep their
// time. A file written since is read again.
func TestTMDBFilesDecodedOnce(t *testing.T) {
	const q = "?api_key=key&language=en-US"
	const sq = q + "&append_to_response=images&include_image_language=en,ja,null"
	dir := t.TempDir()
	f := fetch.New(dir)
	n := &tvdbNet{answers: map[string]string{
		"api.themoviedb.org/3/tv/77" + q:           `{"name":"Show","seasons":[{"season_number":1},{"season_number":2}]}`,
		"api.themoviedb.org/3/tv/77/season/1" + sq: `{"episodes":[{"episode_number":1,"name":"One"}]}`,
		"api.themoviedb.org/3/tv/77/season/2" + sq: `{"overview":"Second cour.","episodes":[{"episode_number":1,"name":"Two",
			"crew":[{"job":"Director","department":"Directing","name":"A Director"}]}],"images":{"posters":[{"file_path":"/s2.jpg"}]}}`,
	}}
	f.SetTransport(n)
	tm := NewTMDB(f, "key")
	ctx := context.Background()
	show := filepath.Join(dir, "tmdb", "tv-77.json")
	s1, s2 := filepath.Join(dir, "tmdb", "tv-77-s1-images.json"), filepath.Join(dir, "tmdb", "tv-77-s2-images.json")
	check := func(when string) {
		t.Helper()
		if name, _, err := tm.Text(ctx, 77, false); err != nil || name != "Show" {
			t.Errorf("%s: text %q %v", when, name, err)
		}
		if when == "first read" {
			scramble(t, show) // decoded: the reads after this one must not decode it again
		}
		eps, posters, err := tm.EpisodesAndPosters(ctx, 77)
		if err != nil || len(eps) != 2 || eps[1].Title != "Two" || strings.Join(eps[1].Directors, ",") != "A Director" ||
			strings.Join(posters[2], " ") != tmdbImg+"/s2.jpg" {
			t.Errorf("%s: episodes %+v posters %v %v", when, eps, posters, err)
		}
		if when == "first read" {
			scramble(t, s1)
			scramble(t, s2)
		}
		if o, err := tm.SeasonOverview(ctx, 77, 2); err != nil || o != "Second cour." {
			t.Errorf("%s: season overview %q %v", when, o, err)
		}
		if a, err := tm.SeasonArt(ctx, 77, 2); err != nil || strings.Join(a.Posters, " ") != tmdbImg+"/s2.jpg" {
			t.Errorf("%s: season art %v %v", when, a.Posters, err)
		}
	}
	check("download") // downloaded, then read from the cache
	asked := len(n.asked)
	tm = NewTMDB(f, "key") // nothing decoded yet: a restart
	check("first read")
	check("next build")
	if len(n.asked) != asked {
		t.Errorf("%d requests after the download, want none: a scrambled file was decoded again", len(n.asked)-asked)
	}

	rewrite(t, s2, `{"overview":"Rewritten.","episodes":[{"episode_number":1,"name":"New Two"}]}`)
	if o, err := tm.SeasonOverview(ctx, 77, 2); err != nil || o != "Rewritten." {
		t.Errorf("season file written since: overview %q %v, want the new one", o, err)
	}
	if eps, _, _ := tm.EpisodesAndPosters(ctx, 77); len(eps) != 2 || eps[0].Title != "One" || eps[1].Title != "New Two" {
		// season 1 is still scrambled with its old time: kept, so it isn't read.
		t.Errorf("season file written since: episodes %+v, want season 1's kept and season 2's new one", eps)
	}
}

// A v4 episode list page is decoded once while its file keeps its time:
// every /anidb entry of a series reads the whole list, one build after the
// other. A page written since is read again.
func TestTVDBEpisodePagesDecodedOnce(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(dir)
	n := &tvdbNet{answers: map[string]string{
		v4 + "/login": `{"data": {"token": "t4"}, "status": "success"}`,
		v4 + "/series/500/episodes/official/eng?page=0": `{"data":{"episodes":[{"id":11,"seasonNumber":1,"number":1,"name":"One"},
			{"id":12,"seasonNumber":1,"number":2,"name":"Two"}]},"links":{"next":null}}`,
	}}
	f.SetTransport(n)
	tv := NewTVDB(f, "v4key", "", "")
	ctx := context.Background()
	page := filepath.Join(dir, "tvdb", "500", "episodes-0.json")
	titles := func() string {
		eps, err := tv.Episodes(ctx, 500)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range eps {
			out = append(out, e.Title)
		}
		return strings.Join(out, ",")
	}
	if got := titles(); got != "One,Two" { // downloaded
		t.Fatalf("episodes %q", got)
	}
	asked := len(n.asked)
	if got := titles(); got != "One,Two" { // read from the cache, decoded and kept
		t.Fatalf("from the cache: %q", got)
	}
	scramble(t, page)
	for _, entry := range []string{"second entry", "third entry"} {
		if got := titles(); got != "One,Two" {
			t.Errorf("%s: %q, want the page decoded before", entry, got)
		}
	}
	if len(n.asked) != asked {
		t.Errorf("%d requests after the download, want none: the scrambled page was decoded again", len(n.asked)-asked)
	}
	rewrite(t, page, `{"data":{"episodes":[{"id":11,"seasonNumber":1,"number":1,"name":"New One"}]},"links":{"next":null}}`)
	if got := titles(); got != "New One" {
		t.Errorf("page written since: %q, want the new one", got)
	}
}

// Each memo keeps its values as long as decided: AniDB's entries an hour,
// swept every 5 minutes; the others a minute, swept at each value kept.
// (How a life and a sweep interval work is fetch.Memo's test.)
func TestMemoLives(t *testing.T) {
	f := fetch.New(t.TempDir())
	tv, tm := NewTVDB(f, "key", "", ""), NewTMDB(f, "key")
	type lives interface {
		Life() (time.Duration, time.Duration)
	}
	for _, c := range []struct {
		name        string
		m           lives
		life, every time.Duration
	}{
		{"anidb", newAniDBMemo(), time.Hour, 5 * time.Minute},
		{"tvdb series", tv.series, time.Minute, 0},
		{"tvdb pages", tv.pages, time.Minute, 0},
		{"tmdb shows", tm.shows, time.Minute, 0},
		{"tmdb seasons", tm.seasons, time.Minute, 0},
	} {
		if life, every := c.m.Life(); life != c.life || every != c.every {
			t.Errorf("%s: life %v, sweep every %v; want %v, %v", c.name, life, every, c.life, c.every)
		}
	}
}
