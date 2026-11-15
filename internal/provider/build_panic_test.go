package provider

import (
	"context"
	"strings"
	"testing"

	"kizuna/internal/fetch"
)

// A group's Wait waits for every goroutine, the panicking one's siblings
// too, then panics once with the first panic and the stack it had on its
// own goroutine, also after passing through another group's Wait.
func TestGroupWaitPanicsWithTheGoroutinesPanic(t *testing.T) {
	waitPanic := func(g *group) (p *goroutinePanic) {
		defer func() { p, _ = recover().(*goroutinePanic) }()
		g.Wait()
		return nil
	}
	var inner, outer group
	release, sibling := make(chan struct{}), false
	inner.Go(func() { <-release; sibling = true })
	inner.Go(panicsInGroup)
	outer.Go(func() {
		close(release) // the sibling ends only now: Wait can't have skipped it
		inner.Wait()
	})
	p := waitPanic(&outer)
	if p == nil || p.value != "boom" || !strings.Contains(string(p.stack), "panicsInGroup") {
		t.Fatalf("got %+v, want the goroutine's panic and stack", p)
	}
	if !sibling {
		t.Error("the panic was thrown before the other goroutine ended")
	}
	if p := waitPanic(&outer); p != nil {
		t.Errorf("second Wait panicked again: %v", p.value)
	}
	if p := waitPanic(&inner); p != nil {
		t.Errorf("the inner group kept its panic: %v", p.value)
	}
}

func panicsInGroup() { panic("boom") }

// A panic in a goroutine a build starts (a show value fetched ahead, an
// episode list, the episode fill) must end as that build's failure, like a
// panic on the build's own goroutine (TestPanickingBuildIsReleased): a
// recover only covers its own goroutine, so without the build's group the
// whole process would end. The panic comes from the request's fetch
// observer, which runs on the goroutine that reads the value: each key
// below is read on a child goroutine of a real build, never on the build's
// own (the stack check tells them apart), and never on a goroutine of
// internal/sources or internal/fetch.
func TestPanickingBuildGoroutineFailsTheBuild(t *testing.T) {
	defaultLists(t)
	cases := []struct {
		name, path, root string
		key              string // the read that panics
		cached           bool   // built once before, so the key is read from the cache, on the build's goroutines
	}{
		{"tvdb: AniDB's details", "/tvdb/library/metadata/tvdb-500", "tvdb-500", "anidb/anime/100.xml", false},
		{"tvdb: a show value fetched ahead", "/tvdb/library/metadata/tvdb-500", "tvdb-500", "tvdb/500/artworks.json", false},
		{"tvdb: TheMovieDb's show", "/tvdb/library/metadata/tvdb-500", "tvdb-500", "tmdb/tv-77.json", false},
		{"tvdb: TheTVDB's crew records", "/tvdb/library/metadata/tvdb-500", "tvdb-500", "tvdb/episodes/11.json", true},
		{"anidb: TheTVDB's episode list", "/anidb/library/metadata/anidb-100", "anidb-100", "tvdb/500/episodes-0.json", false},
		{"anidb: a show value", "/anidb/library/metadata/anidb-100", "anidb-100", "tvdb/500/artworks.json", false},
		{"anidb: the episode fill's crew", "/anidb/library/metadata/anidb-100", "anidb-100", "tvdb/episodes/11.json", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newTestProvider(t, &fakeNet{answer: testSeries})
			forget := func() {
				p.mu.Lock()
				clear(p.shows)
				clear(p.failed)
				p.mu.Unlock()
			}
			plain := context.Background()
			if c.cached {
				if rec := plexRequest(plain, p, "GET", c.path, ""); rec.Code != 200 {
					t.Fatalf("first build: HTTP %d %s", rec.Code, rec.Body)
				}
				forget()
			}
			var logs strings.Builder
			restore := captureLogs(&logs) // the panic's error line and stack
			defer restore()
			panicking := fetch.WithObserver(plain, func(e fetch.Event) {
				if e.Key == c.key {
					panic("boom: " + e.Key)
				}
			})
			// The second request is answered by the failed build, not a new one.
			for range 2 {
				rec := plexRequest(panicking, p, "GET", c.path, "")
				if rec.Code != 503 || !strings.Contains(rec.Body.String(), "build panicked: boom: "+c.key) {
					t.Fatalf("HTTP %d %s, want 503 and the panic", rec.Code, rec.Body)
				}
			}
			if !p.WaitBuilds(plain) {
				t.Fatal("WaitBuilds gave up")
			}
			p.mu.Lock()
			builds, running := len(p.builds), p.running
			p.mu.Unlock()
			if builds != 0 || running != 0 {
				t.Errorf("left behind: builds %d running %d", builds, running)
			}
			// The logged stack is the panicking goroutine's (the read), not
			// the one of the build's goroutine waiting for it.
			logged := false
			for line := range strings.Lines(logs.String()) {
				if strings.Contains(line, `level=ERROR msg="show build panicked" root=`+c.root) {
					logged = true
					if !strings.Contains(line, "fetch.Observe") || strings.Contains(line, "runBuild") {
						t.Errorf("stack:\n%s\nwant the child goroutine's", line)
					}
				}
			}
			if !logged {
				t.Errorf("logs:\n%s\nwant the panic logged", logs.String())
			}
			// Nothing of the dead build is in the way of the next one.
			forget()
			if rec := plexRequest(plain, p, "GET", c.path, ""); rec.Code != 200 {
				t.Errorf("next build: HTTP %d %s", rec.Code, rec.Body)
			}
		})
	}
}
