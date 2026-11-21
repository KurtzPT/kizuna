package provider

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"kizuna/internal/anidb"
	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/sources"
)

// readsOf builds root with every read observed, and returns the show and
// how many times each cache key was read, by kind.
func readsOf(t *testing.T, p *Provider, root string) (*Show, map[string]map[fetch.Kind]int) {
	t.Helper()
	var mu sync.Mutex
	reads := map[string]map[fetch.Kind]int{}
	ctx := fetch.WithObserver(context.Background(), func(e fetch.Event) {
		mu.Lock()
		defer mu.Unlock()
		if reads[e.Key] == nil {
			reads[e.Key] = map[fetch.Kind]int{}
		}
		reads[e.Key][e.Kind]++
	})
	s, err := p.buildShow(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	p.tvdb.WaitIdle()
	p.anidb.WaitIdle()
	return s, reads
}

// rewriteKeeping replaces a cache file's bytes and moves its time by shift
// (0: the same time; -time.Minute: another, still fresh, apart even on a
// file system that keeps 2 s steps).
func rewriteKeeping(t *testing.T, path, body string, shift time.Duration) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	at := st.ModTime().Add(shift)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// The /anidb entries of a series, built one after the other, decode
// TheMovieDb's season list and season files and TheTVDB's episode list
// once: a build while those files keep their time decodes none of them
// (each file is replaced by JSON cut short, which a decode would find
// broken and download again), and its trace still shows each read as the
// cache hit it was. A file written since is read again.
func TestSeriesListsDecodedOncePerFile(t *testing.T) {
	defaultLists(t)
	net := &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		if path == "api.anidb.net/httpapi" && q["aid"][0] == "200" {
			return 200, strings.Replace(secondXML, "<episodes>", `<titles><title type="main" xml:lang="x-jat">Kizuna Test 2</title></titles><episodes>`, 1)
		}
		return testSeries(path, q)
	}}
	p := newTestProvider(t, net)
	dir := t.TempDir()
	f := fetch.New(dir)
	f.SetTransport(net)
	p.tvdb, p.tmdb = sources.NewTVDB(f, "key", "", ""), sources.NewTMDB(f, "key")
	f.ThrottleAll(0)
	p.titles.SetForTest(map[int][]anidb.Title{100: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test"}},
		200: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test 2"}}})
	p.lists.SetForTest(map[int]mapping.Entry{
		100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}, TMDB: mapping.Placement{ID: 77, Season: 1, Mapped: true}},
		200: {AID: 200, TVDB: mapping.Placement{ID: 500, Season: 2, Mapped: true}, TMDB: mapping.Placement{ID: 77, Season: 2, Mapped: true}},
	})
	readsOf(t, p, "anidb-100") // downloads
	readsOf(t, p, "anidb-200")
	readsOf(t, p, "anidb-100") // reads the cached files, decodes and keeps them
	_, before := readsOf(t, p, "anidb-200")

	files := []string{"tmdb/tv-77.json", "tmdb/tv-77-s1-images.json", "tmdb/tv-77-s2-images.json", "tvdb/500/episodes-0.json"}
	for _, key := range files {
		rewriteKeeping(t, filepath.Join(dir, filepath.FromSlash(key)), `{"cut`, 0)
	}
	// The files' requests: a decode would find a file broken and ask again.
	// (TheTVDB's 404s aren't cached: those are asked at each build.)
	paths := []string{"api.themoviedb.org/3/tv/77", "api.themoviedb.org/3/tv/77/season/1", "api.themoviedb.org/3/tv/77/season/2",
		"api4.thetvdb.com/v4/series/500/episodes/official/eng"}
	asked := func() []int {
		var n []int
		for _, path := range paths {
			n = append(n, net.count(path))
		}
		return n
	}
	hits := asked()
	for _, root := range []string{"anidb-100", "anidb-200"} {
		s, reads := readsOf(t, p, root)
		if ep := s.Episode(1, 1); ep == nil || ep.titles["tvdb"] == "" || ep.titles["tmdb"] == "" {
			t.Errorf("%s: S01E01 %+v, want TheTVDB's and TheMovieDb's titles", root, ep)
		}
		if root != "anidb-200" {
			continue
		}
		for _, key := range files {
			if !maps.Equal(reads[key], before[key]) {
				t.Errorf("%s: %s read %v, want %v as before", root, key, reads[key], before[key])
			}
		}
	}
	if after := asked(); !slices.Equal(after, hits) {
		t.Errorf("requests of %v: %v, want %v as before: a kept file was decoded again", paths, after, hits)
	}

	rewriteKeeping(t, filepath.Join(dir, "tmdb", "tv-77-s2-images.json"),
		`{"episodes":[{"episode_number":1,"name":"TMDB Three, rewritten"}]}`, -time.Minute)
	if s, _ := readsOf(t, p, "anidb-200"); s.Episode(1, 1).titles["tmdb"] != "TMDB Three, rewritten" {
		t.Errorf("season file written since: title %q, want the new one", s.Episode(1, 1).titles["tmdb"])
	}
}
