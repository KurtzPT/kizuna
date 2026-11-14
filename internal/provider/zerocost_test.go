package provider

import (
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"testing"
)

// With the match reports off, their hooks must cost nothing: a request
// allocates no more than it did before the reports existed. The budgets
// were those counts (Go 1.27), lowered since by changes that saved
// allocations, and raised only where the maintainer agreed a change needs
// more (the builds: TheTVDB's and TheMovieDb's show values fetched ahead),
// to the measured counts and no higher. A Go upgrade may move them: then run
// this test under the new Go on the commit that last set the budgets (git
// log -L on the budget lines below finds it) and take its counts. Everything
// is cached (a download's timers vary) and the GC is off (it empties the
// sync.Pools): the counts are steady, except that a build's parallel
// fetches now and then cost one more. Take the highest count of a few
// hundred runs (-count).
func TestReportsOffAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are off under the race detector")
	}
	p := newTestProvider(t, &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		switch path {
		case "api4.thetvdb.com/v4/series/500/extended", "api4.thetvdb.com/v4/series/500/artworks",
			"api4.thetvdb.com/v4/series/500/translations/eng": // the series text, fetched ahead with the defaults
			return 200, `{"data":{}}` // cached, unlike a 404: no download, no throttle wait
		}
		return testSeries(path, q)
	}})
	serve := func(method, path, body string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr, req.Host = "127.0.0.1:1234", "kizuna:32600"
		p.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}
	show := func() { serve("GET", "/tvdb/library/metadata/tvdb-500", "") }
	anidbShow := func() { serve("GET", "/anidb/library/metadata/anidb-100", "") }
	show() // fills the cache
	anidbShow()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	cases := []struct {
		name   string
		budget float64
		run    func()
	}{
		{"show from memory", 135, show},
		{"match", 180, func() { serve("POST", "/tvdb/library/metadata/matches", `{"type":2,"title":"Kizuna Test"}`) }},
		{"show build", 538, func() {
			p.mu.Lock()
			clear(p.shows)
			p.mu.Unlock()
			show()
		}},
		{"anidb show build", 515, func() {
			p.mu.Lock()
			clear(p.shows)
			p.mu.Unlock()
			anidbShow()
		}},
	}
	for _, c := range cases {
		if got := testing.AllocsPerRun(5, c.run); got > c.budget {
			t.Errorf("%s: %.0f allocations, want at most %.0f", c.name, got, c.budget)
		}
	}
}
