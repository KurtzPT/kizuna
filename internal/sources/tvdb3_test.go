package sources

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

// tvdbNet answers TheTVDB requests from a map of "host/path?query" to JSON
// and records what was asked.
type tvdbNet struct {
	mu      sync.Mutex
	answers map[string]string
	asked   []string
	lang    []string // Accept-Language of each non-login request
}

func (n *tvdbNet) RoundTrip(r *http.Request) (*http.Response, error) {
	u := r.URL.Host + r.URL.Path
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	n.mu.Lock()
	n.asked = append(n.asked, u)
	if !strings.HasSuffix(r.URL.Path, "/login") {
		n.lang = append(n.lang, r.Header.Get("Accept-Language"))
	}
	body, ok := n.answers[u]
	n.mu.Unlock()
	code := http.StatusOK
	if c, rest, found := strings.Cut(body, " "); found && len(c) == 3 && c[0] >= '1' && c[0] <= '5' { // "400 {...}"
		code, _ = strconv.Atoi(c)
		body = rest
	}
	if !ok {
		code, body = http.StatusNotFound, `{"Error":"Resource not found"}`
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

func newTVDBTest(t *testing.T, answers map[string]string, key, pin, legacy string) (*TVDB, *tvdbNet) {
	t.Helper()
	f := fetch.New(t.TempDir())
	n := &tvdbNet{answers: answers}
	f.SetTransport(n)
	return NewTVDB(f, key, pin, legacy), n
}

const v3 = "api.thetvdb.com"

func TestTVDBLegacyKey(t *testing.T) {
	tv, n := newTVDBTest(t, map[string]string{
		v3 + "/login": `{"token":"t3"}`,
		v3 + "/series/9": `{"data":{"seriesName":"Show","overview":"About it.","firstAired":"2023-09-29",
			"genre":["Anime","Fantasy"],"rating":"TV-14","runtime":"25",
			"poster":"v4/series/9/posters/main.jpg","fanart":"v4/series/9/backgrounds/main.jpg"}}`,
		v3 + "/series/9/episodes?page=1": `{"links":{"next":2},"data":[
			{"airedSeason":1,"airedEpisodeNumber":2,"absoluteNumber":2,"episodeName":"Two","firstAired":"2023-10-06","filename":"v4/episode/2/screencap/a.jpg"},
			{"airedSeason":1,"airedEpisodeNumber":1,"absoluteNumber":1,"episodeName":"One","overview":"First.","firstAired":"2023-09-29",
				"directors":["Dir A"," ","Dir A"],"writers":["Writer A","Writer B"]}]}`,
		v3 + "/series/9/episodes?page=2": `{"links":{"next":null},"data":[
			{"airedSeason":0,"airedEpisodeNumber":1,"absoluteNumber":null,"episodeName":"Special"},
			{"airedSeason":null,"airedEpisodeNumber":5,"episodeName":"unplaced"}]}`,
		v3 + "/series/9/actors": `{"data":[{"name":"B","role":"Second","sortOrder":2},{"name":"A","role":"First","sortOrder":0,"image":"v4/actor/1/photo/a.jpg"}]}`,
		v3 + "/series/9/images/query?keyType=poster": `{"data":[
			{"fileName":"v4/series/9/posters/low.jpg","ratingsInfo":{"average":2}},
			{"fileName":"v4/series/9/posters/main.jpg","ratingsInfo":{"average":0}},
			{"fileName":"v4/series/9/posters/high.jpg","ratingsInfo":{"average":9}}]}`,
		// no fanart query answer: only the series' own background
	}, "", "", "legacy")
	ctx := context.Background()
	if tv.API() != "v3" || !tv.Enabled() {
		t.Fatalf("API %q enabled %v, want v3", tv.API(), tv.Enabled())
	}

	eps, err := tv.Episodes(ctx, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 3 {
		t.Fatalf("got %d episodes, want 3: %+v", len(eps), eps)
	}
	if e := eps[0]; e.Season != 1 || e.Number != 1 || e.Absolute != 1 || e.Title != "One" || e.Summary != "First." || e.Minutes != 25 {
		t.Errorf("first episode %+v", e)
	}
	if e := eps[0]; strings.Join(e.Directors, ",") != "Dir A" || strings.Join(e.Writers, ",") != "Writer A,Writer B" {
		t.Errorf("crew: directors %q writers %q", e.Directors, e.Writers)
	}
	if crew, pending := tv.EpisodeCrew(ctx, []int{1, 2}); crew != nil || pending {
		t.Errorf("legacy key: EpisodeCrew %v %v, want nothing (v3 lists carry the crew)", crew, pending)
	}
	if e := eps[1]; e.Thumb != "https://artworks.thetvdb.com/banners/v4/episode/2/screencap/a.jpg" {
		t.Errorf("thumb %q", e.Thumb)
	}
	if e := eps[2]; e.Season != 0 || e.Absolute != 0 || e.Title != "Special" { // specials last
		t.Errorf("special %+v", e)
	}

	if name, ov, err := tv.Overview(ctx, 9); name != "Show" || ov != "About it." || err != nil {
		t.Errorf("overview %q %q %v", name, ov, err)
	}

	d, err := tv.Details(ctx, 9)
	switch {
	case err != nil:
		t.Errorf("details: %v", err)
	case d.Date != "2023-09-29" || len(d.Genres) != 2:
		t.Errorf("date %q genres %v", d.Date, d.Genres)
	case d.Rating([]string{"US"}) != "TV-14":
		t.Errorf("rating %v", d.Ratings)
	case len(d.Cast) != 2 || d.Cast[0].Name != "A" || d.Cast[0].Photo != "https://artworks.thetvdb.com/banners/v4/actor/1/photo/a.jpg":
		t.Errorf("cast %+v", d.Cast)
	}

	a, err := tv.Art(ctx, 9)
	if err != nil {
		t.Errorf("art: %v", err)
	}
	const banners = "https://artworks.thetvdb.com/banners/v4/series/9/"
	wantPosters := []string{banners + "posters/main.jpg", banners + "posters/high.jpg", banners + "posters/low.jpg"}
	if strings.Join(a.Posters, " ") != strings.Join(wantPosters, " ") {
		t.Errorf("posters %v, want %v", a.Posters, wantPosters)
	}
	if len(a.Backgrounds) != 1 || a.Backgrounds[0] != banners+"backgrounds/main.jpg" || len(a.Logos) != 0 {
		t.Errorf("backgrounds %v logos %v", a.Backgrounds, a.Logos)
	}

	for i, l := range n.lang {
		if l != "en" {
			t.Errorf("request %d: Accept-Language %q, want en", i, l)
		}
	}
	logins := 0
	for _, u := range n.asked {
		if strings.HasPrefix(u, "api4.") {
			t.Errorf("legacy key called v4: %s", u)
		}
		if u == v3+"/login" {
			logins++
		}
	}
	if logins != 1 {
		t.Errorf("%d v3 logins, want 1", logins)
	}
}

// v4 episode crew: records past the time budget are fetched in the
// background (pending), and the next call has them from the cache.
func TestTVDBEpisodeCrewBackground(t *testing.T) {
	const v4 = "api4.thetvdb.com/v4"
	tv, n := newTVDBTest(t, map[string]string{
		v4 + "/login": `{"data":{"token":"t4"}}`,
		v4 + "/episodes/1/extended": `{"data":{"characters":[{"personName":"D2","peopleType":"Director","sort":2},
			{"personName":"D1","peopleType":"Director","sort":1},{"personName":"W","peopleType":"Writer"}]}}`,
		v4 + "/episodes/2/extended": `{"data":{"characters":[]}}`,
	}, "v4key", "", "")
	ctx := context.Background()
	old := crewBudget
	crewBudget = 0 // everything goes to the background
	defer func() { crewBudget = old }()
	if crew, pending := tv.EpisodeCrew(ctx, []int{1, 2, 3, 1}); !pending || len(crew) != 0 {
		t.Fatalf("over budget: crew %v pending %v, want none and pending", crew, pending)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		tv.crewMu.Lock()
		left := len(tv.crewQueued)
		tv.crewMu.Unlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background queue never drained")
		}
	}
	crew, pending := tv.EpisodeCrew(ctx, []int{1, 2, 3})
	if pending || strings.Join(crew[1].Directors, ",") != "D1,D2" || strings.Join(crew[1].Writers, ",") != "W" {
		t.Errorf("after the background: crew %+v pending %v", crew, pending)
	}
	if _, ok := crew[3]; ok {
		t.Error("episode 3 has no record (404): want no crew")
	}
	for _, u := range []string{v4 + "/episodes/1/extended", v4 + "/episodes/3/extended"} {
		if c := n.count(u); c != 1 {
			t.Errorf("%s asked %d times, want 1 (cached / failure remembered)", u, c)
		}
	}
}

// With both keys set, the v4 key is used and the legacy key never is.
func TestTVDBV4KeyWins(t *testing.T) {
	tv, n := newTVDBTest(t, map[string]string{
		"api4.thetvdb.com/v4/login":                     `{"data":{"token":"t4"}}`,
		"api4.thetvdb.com/v4/series/9/translations/eng": `{"data":{"name":"Show v4","overview":"v4 text"}}`,
	}, "v4key", "1234", "legacy")
	if tv.API() != "v4" {
		t.Fatalf("API %q, want v4", tv.API())
	}
	if name, _, _ := tv.Overview(context.Background(), 9); name != "Show v4" {
		t.Errorf("name %q", name)
	}
	for _, u := range n.asked {
		if strings.HasPrefix(u, v3) {
			t.Errorf("v4 key set, but v3 was called: %s", u)
		}
	}
}

func TestTVDBNoKey(t *testing.T) {
	tv, _ := newTVDBTest(t, nil, "", "", "")
	if tv.Enabled() || tv.API() != "" {
		t.Errorf("enabled %v API %q, want off", tv.Enabled(), tv.API())
	}
}

func (n *tvdbNet) count(u string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, a := range n.asked {
		if a == u {
			c++
		}
	}
	return c
}

// A rejected v4 key (here: no PIN) hands over to the legacy key, once.
func TestTVDBFallsBackToLegacyKey(t *testing.T) {
	tv, n := newTVDBTest(t, map[string]string{
		"api4.thetvdb.com/v4/login": `400 {"status":"failure","message":"InvalidValueType: pin required"}`,
		v3 + "/login":               `{"token":"t3"}`,
		v3 + "/series/9":            `{"data":{"seriesName":"Show v3","overview":"v3 text"}}`,
	}, "v4key", "", "legacy")
	if tv.API() != "v4" {
		t.Fatalf("before the first call: API %q, want v4", tv.API())
	}
	for range 3 {
		if name, _, _ := tv.Overview(context.Background(), 9); name != "Show v3" {
			t.Fatalf("name %q, want the legacy API's", name)
		}
	}
	if tv.API() != "v3" {
		t.Errorf("API %q after the fallback, want v3", tv.API())
	}
	if c := n.count("api4.thetvdb.com/v4/login"); c != 1 {
		t.Errorf("v4 login tried %d times, want 1", c)
	}
	if c := n.count(v3 + "/login"); c != 1 {
		t.Errorf("v3 login tried %d times, want 1", c)
	}
}

// A v4 login that fails for another reason (TheTVDB down) is no reason to
// give up the v4 key.
func TestTVDBNoFallbackOnServerError(t *testing.T) {
	tv, n := newTVDBTest(t, map[string]string{
		"api4.thetvdb.com/v4/login": `503 {}`,
		v3 + "/login":               `{"token":"t3"}`,
	}, "v4key", "", "legacy")
	_, _, _ = tv.Overview(context.Background(), 9)
	if tv.API() != "v4" {
		t.Errorf("API %q, want v4", tv.API())
	}
	if c := n.count(v3 + "/login"); c != 0 {
		t.Errorf("v3 login tried %d times, want 0", c)
	}
}
