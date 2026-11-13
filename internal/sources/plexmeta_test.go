package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

// The answers in testdata/plex are trimmed copies of the service's real
// ones (2026-11-13): Frieren (TheTVDB 424536) and Your Name. (TheMovieDb
// movie 372058).
const (
	frierenShow = "631ccdfccbff4464978f9d53"
	frierenS0   = "65281ecb652e2431381cbf47"
	frierenS1   = "631ccdfccbff4464978f9d55"
	yourName    = "5d776bfffb0d55001f579a01"
	plexToken   = "plex-secret-token"
)

// plexNet is Plex's service: it answers from testdata by request, and
// records each request. A match answers by the guid in its body.
type plexNet struct {
	mu      sync.Mutex
	asked   []plexAsked
	answers map[string]string // "GET /library/metadata/<id>[@start]" or "POST tvdb://1" -> "file" or "<code> <body>"
}

type plexAsked struct {
	method, url, body, token, lang, start, size string
}

func (n *plexNet) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	a := plexAsked{r.Method, r.URL.String(), string(body), r.Header.Get("X-Plex-Token"), r.Header.Get("X-Plex-Language"),
		r.Header.Get("X-Plex-Container-Start"), r.Header.Get("X-Plex-Container-Size")}
	n.mu.Lock()
	n.asked = append(n.asked, a)
	n.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	if r.Method == http.MethodPost {
		var m struct {
			Type int    `json:"type"`
			GUID string `json:"guid"`
		}
		_ = json.Unmarshal(body, &m)
		key = fmt.Sprintf("POST %d %s", m.Type, m.GUID)
	}
	if a.start != "" && a.start != "0" {
		key += "@" + a.start
	}
	code, out := http.StatusNotFound, `{"Error":{"statusCode":404}}`
	if v, ok := n.answers[key]; ok {
		if c, rest, ok := strings.Cut(v, " "); ok && len(c) == 3 {
			code, _ = strconv.Atoi(c)
			out = rest
		} else {
			data, err := os.ReadFile(filepath.Join("testdata", "plex", v))
			if err != nil {
				return nil, err
			}
			code, out = http.StatusOK, string(data)
		}
	}
	h := http.Header{}
	if code/100 == 3 {
		h.Set("Location", "https://elsewhere.example/x")
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(out)), Header: h, Request: r}, nil
}

func (n *plexNet) requests() []plexAsked {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.asked)
}

func (n *plexNet) count(method, path string) int {
	c := 0
	for _, a := range n.requests() {
		if a.method == method && strings.HasSuffix(strings.SplitN(a.url, "?", 2)[0], path) {
			c++
		}
	}
	return c
}

// frieren is the service with Frieren and Your Name. in it.
func frieren() map[string]string {
	return map[string]string{
		"POST 2 tvdb://424536":                               "match-frieren.json",
		"POST 1 tmdb://372058":                               "match-yourname.json",
		"POST 1 imdb://tt5311514":                            "match-yourname.json",
		"GET /library/metadata/" + frierenShow:               "show-frieren.json",
		"GET /library/metadata/" + frierenShow + "/children": "children-frieren.json",
		"GET /library/metadata/" + frierenShow + "/images":   "images-frieren.json",
		"GET /library/metadata/" + frierenS1 + "/children":   "season-frieren-1.json",
		"GET /library/metadata/" + frierenS0 + "/children":   "season-frieren-0.json",
		"GET /library/metadata/" + yourName:                  "movie-yourname.json",
	}
}

func newPlexTest(t *testing.T, answers map[string]string) (*Plex, *plexNet, string) {
	t.Helper()
	dir := t.TempDir()
	f := fetch.New(dir)
	n := &plexNet{answers: answers}
	f.SetTransport(n)
	p := NewPlex(f, plexToken)
	f.ThrottleAll(0)
	return p, n, dir
}

// A series is found by its TheTVDB id, as a series (type 2), and one
// request and one cached file give its title, summary, details and pictures.
func TestPlexShow(t *testing.T) {
	p, n, _ := newPlexTest(t, frieren())
	ctx := context.Background()
	s, err := p.Show(ctx, PlexSeries(424536))
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "Frieren: Beyond Journey's End" || !strings.HasPrefix(s.Summary, "Elf mage Frieren") {
		t.Errorf("title %q summary %q", s.Title, s.Summary)
	}
	d := s.Details
	if len(d.Genres) < 3 || strings.Join(d.Genres[:3], ",") != "Drama,Animation,Adventure" {
		t.Errorf("genres %q", d.Genres)
	}
	if d.Studio != "Madhouse" || d.Date != "2023-09-29" || d.Ratings["US"] != "TV-14" {
		t.Errorf("studio %q date %q ratings %v", d.Studio, d.Date, d.Ratings)
	}
	if len(d.Cast) != 3 || d.Cast[0] != (Person{"Atsumi Tanezaki", "Frieren (voice)",
		"https://metadata-static.plex.tv/b/people/ba0810651155042dcb736b1cdb079d04.jpg"}) {
		t.Errorf("cast %+v", d.Cast)
	}
	if len(d.Directors) == 0 || d.Directors[0] != "Keiichiro Saito" || len(d.Writers) == 0 {
		t.Errorf("directors %q writers %q", d.Directors, d.Writers)
	}
	a := s.Art
	if strings.Join(a.Posters, " ") != "https://image.tmdb.org/t/p/original/dqZENchTd7lp5zht7BdlqM7RBhD.jpg" ||
		strings.Join(a.Backgrounds, " ") != "https://image.tmdb.org/t/p/original/rBOnrVlck7BIlGeWVlzYiZeg4l2.jpg" ||
		strings.Join(a.Logos, " ") != "https://metadata-static.plex.tv/1/009125463f/1a7237f40d55bb1a1db490f7b31ab36e.png" {
		t.Errorf("art %+v", a)
	}
	if _, err := p.Show(ctx, PlexSeries(424536)); err != nil {
		t.Fatal(err)
	}
	if c := n.count("POST", "/library/metadata/matches"); c != 1 {
		t.Errorf("matched %d times, want 1 (cached)", c)
	}
	if c := n.count("GET", "/library/metadata/"+frierenShow); c != 1 {
		t.Errorf("show read %d times, want 1 (cached)", c)
	}
	if body := n.requests()[0].body; body != `{"type":2,"title":"x","guid":"tvdb://424536"}` {
		t.Errorf("match body %s", body)
	}
}

// A movie is asked for as a movie (type 1): by its TheMovieDb movie id, or
// by its IMDb id. Nothing else becomes a request.
func TestPlexMovie(t *testing.T) {
	p, n, _ := newPlexTest(t, frieren())
	ctx := context.Background()
	for _, r := range []PlexRef{PlexMovie(372058), PlexIMDbMovie("tt5311514")} {
		s, err := p.Show(ctx, r)
		if err != nil || s.Title != "Your Name." || s.Details.Ratings["US"] != "PG" || len(s.Art.Posters) != 1 {
			t.Errorf("%s: %+v %v", r, s, err)
		}
	}
	for i, a := range n.requests() {
		if a.method == "POST" && !strings.HasPrefix(a.body, `{"type":1,`) {
			t.Errorf("request %d: %s, want a movie match", i, a.body)
		}
	}
	for _, r := range []PlexRef{PlexMovie(0), PlexIMDbMovie(""), PlexIMDbMovie("../x"), PlexSeries(0)} {
		if s, err := p.Show(ctx, r); err != nil || s.Title != "" {
			t.Errorf("%q: %+v %v, want nothing", r, s, err)
		}
	}
	if s, err := p.Seasons(ctx, PlexMovie(372058)); err != nil || len(s) != 0 {
		t.Errorf("a movie's seasons: %+v %v, want none", s, err)
	}
	if c := len(n.requests()); c != 3 {
		t.Errorf("%d requests, want 3 (two matches, the movie read once)", c)
	}
}

// An empty match means "no Plex show": nothing, no error. It is asked
// again once a day old, not before; a show found is kept 30 days.
func TestPlexNoMatch(t *testing.T) {
	answers := frieren()
	answers["POST 2 tvdb://1"] = "match-none.json"
	p, n, dir := newPlexTest(t, answers)
	ctx := context.Background()
	if s, err := p.Show(ctx, PlexSeries(1)); err != nil || s.Title != "" {
		t.Fatalf("%+v %v, want nothing", s, err)
	}
	if _, err := p.Show(ctx, PlexSeries(1)); err != nil {
		t.Fatal(err)
	}
	if c := n.count("POST", "/matches"); c != 1 {
		t.Fatalf("asked %d times, want 1 (cached for a day)", c)
	}
	age := func(name string, d time.Duration) {
		at := time.Now().Add(-d)
		if err := os.Chtimes(filepath.Join(dir, "plex", name), at, at); err != nil {
			t.Fatal(err)
		}
	}
	age("match-show-tvdb-1.json", 25*time.Hour)
	if _, err := p.Show(ctx, PlexSeries(1)); err != nil {
		t.Fatal(err)
	}
	if c := n.count("POST", "/matches"); c != 2 {
		t.Errorf("asked %d times, want 2 (a day old: asked again)", c)
	}
	if _, err := p.Show(ctx, PlexSeries(424536)); err != nil {
		t.Fatal(err)
	}
	age("match-show-tvdb-424536.json", 25*time.Hour)
	if _, err := p.Show(ctx, PlexSeries(424536)); err != nil {
		t.Fatal(err)
	}
	if c := n.count("POST", "/matches"); c != 3 {
		t.Errorf("asked %d times, want 3 (a show found is kept 30 days)", c)
	}
}

// The token goes on every request, as a header, in English; never in a URL,
// a log line (debug included) or a cache file.
func TestPlexTokenOnlyAsHeader(t *testing.T) {
	logs := &logBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	answers := frieren()
	answers["POST 2 tvdb://7"] = "500 {}" // a failure is logged too
	p, n, dir := newPlexTest(t, answers)
	ctx := context.Background()
	if _, err := p.Show(ctx, PlexSeries(424536)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Episodes(ctx, PlexSeries(424536)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Images(ctx, PlexSeries(424536)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Show(ctx, PlexSeries(7)); err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(logs.String(), "level=DEBUG") || strings.Contains(logs.String(), plexToken) {
		t.Errorf("logs (debug on) hold the token, or nothing was logged:\n%s", logs)
	}
	for _, a := range n.requests() {
		if a.token != plexToken || a.lang != "en-US" {
			t.Errorf("%s %s: token %q language %q", a.method, a.url, a.token, a.lang)
		}
		if !strings.HasPrefix(a.url, "https://metadata.provider.plex.tv/") || strings.Contains(a.url, plexToken) {
			t.Errorf("url %s", a.url)
		}
	}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.Contains(path, plexToken) {
			t.Errorf("cache file name %s has the token", path)
		}
		data, err := os.ReadFile(path)
		if strings.Contains(string(data), plexToken) {
			t.Errorf("cache file %s has the token", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A 401 is a rejected token: one error naming PLEX_TOKEN, and nothing is
// asked until the pause ends. A 403 (Cloudflare's) or a 429 is an ordinary
// failed request that doesn't blame the token.
func TestPlexRejectedToken(t *testing.T) {
	for _, code := range []int{401, 403, 429} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			logs := captureLogs(t)
			answers := frieren()
			answers["POST 2 tvdb://424536"] = strconv.Itoa(code) + ` {"Error":{"message":"You must provide a token!"}}`
			answers["POST 2 tvdb://2"] = "match-frieren.json" // matches, but its read fails too
			answers["GET /library/metadata/"+frierenShow] = strconv.Itoa(code) + ` {}`
			p, n, _ := newPlexTest(t, answers)
			ctx := context.Background()
			for range 2 {
				if _, err := p.Show(ctx, PlexSeries(424536)); err == nil {
					t.Error("no error")
				}
				if _, err := p.Show(ctx, PlexSeries(2)); err == nil {
					t.Error("read: no error")
				}
			}
			named := strings.Count(logs.String(), "PLEX_TOKEN")
			sent := len(n.requests())
			if code == 401 && (named != 1 || sent != 1) {
				t.Errorf("%d lines name PLEX_TOKEN, %d requests: want 1 and 1\n%s", named, sent, logs)
			}
			// Each call asks again: the failed match twice, the other match
			// once (then cached) and its failed read twice.
			if code != 401 && (named != 0 || sent != 5) {
				t.Errorf("%d lines name PLEX_TOKEN, %d requests: want 0 and 5\n%s", named, sent, logs)
			}
			if code != 401 && strings.Contains(logs.String(), "API key") {
				t.Errorf("a %d is logged as a rejected key (fetch's line, which keyCheck's requests leave out)\n%s", code, logs)
			}
		})
	}
}

// A 401 on a read (the match was fine) is a rejected token too: one line
// naming PLEX_TOKEN, and the next calls send nothing.
func TestPlexRejectedTokenOnRead(t *testing.T) {
	logs := captureLogs(t)
	answers := frieren()
	answers["GET /library/metadata/"+frierenShow] = `401 {"Error":{"message":"You must provide a token!"}}`
	p, n, _ := newPlexTest(t, answers)
	ctx := context.Background()
	for range 2 {
		if _, err := p.Show(ctx, PlexSeries(424536)); err == nil {
			t.Error("no error")
		}
	}
	if _, err := p.Show(ctx, PlexMovie(372058)); err == nil {
		t.Error("movie: no error")
	}
	if named, sent := strings.Count(logs.String(), "PLEX_TOKEN"), len(n.requests()); named != 1 || sent != 2 {
		t.Errorf("%d lines name PLEX_TOKEN, %d requests: want 1 and 2 (the match, the read)\n%s", named, sent, logs)
	}
}

// Without a totalSize, a full page means there may be more: the next page
// is read too, until a short one.
func TestPlexPagesWithoutTotal(t *testing.T) {
	page := func(start, n int) string {
		var items []string
		for i := start; i < start+n; i++ {
			items = append(items, fmt.Sprintf(`{"index":%d,"title":"E%d"}`, i, i))
		}
		return `200 {"MediaContainer":{"Metadata":[` + strings.Join(items, ",") + `]}}`
	}
	answers := frieren()
	answers["GET /library/metadata/"+frierenS1+"/children"] = page(1, 200)
	answers["GET /library/metadata/"+frierenS1+"/children@200"] = page(201, 5)
	p, _, _ := newPlexTest(t, answers)
	eps, err := p.Episodes(context.Background(), PlexSeries(424536))
	if err != nil || len(eps) != 205+3 {
		t.Errorf("%d episodes, err %v; want 205 in season 1, 3 specials", len(eps), err)
	}
}

// No request to Plex's service follows a redirect: the token would go along.
func TestPlexNoRedirect(t *testing.T) {
	answers := frieren()
	answers["POST 2 tvdb://424536"] = "302 "
	answers["POST 2 tvdb://2"] = "match-frieren.json" // matches: its reads are redirected
	for _, path := range []string{"", "/children", "/images"} {
		answers["GET /library/metadata/"+frierenShow+path] = "302 "
	}
	p, n, _ := newPlexTest(t, answers)
	ctx := context.Background()
	if _, err := p.Show(ctx, PlexSeries(424536)); err == nil {
		t.Error("match: no error")
	}
	if _, err := p.Show(ctx, PlexSeries(2)); err == nil {
		t.Error("show: no error")
	}
	if _, err := p.Episodes(ctx, PlexSeries(2)); err == nil {
		t.Error("seasons: no error")
	}
	if _, err := p.Images(ctx, PlexSeries(2)); err == nil {
		t.Error("images: no error")
	}
	if got := len(n.requests()); got != 5 {
		t.Errorf("%d requests, want 5", got)
	}
	for _, a := range n.requests() {
		if !strings.HasPrefix(a.url, plexAPI) {
			t.Errorf("followed to %s", a.url)
		}
	}
}

// Seasons come from one season list asked for 200 at once; each season's
// episodes from its own /children, 200 a page, a next page only when there
// are more.
func TestPlexSeasonsAndEpisodes(t *testing.T) {
	answers := frieren()
	// Season 1 with 250 episodes: two pages.
	long := func(start, n int) string {
		var items []string
		for i := start; i < start+n; i++ {
			items = append(items, fmt.Sprintf(`{"index":%d,"parentIndex":1,"title":"E%d","duration":1440000,
				"Director":[{"tag":"D"}],"Writer":[{"tag":" W "},{"tag":"W"}]}`, i, i))
		}
		return fmt.Sprintf(`200 {"MediaContainer":{"offset":%d,"size":%d,"totalSize":250,"Metadata":[%s]}}`, start-1, n, strings.Join(items, ","))
	}
	answers["GET /library/metadata/"+frierenS1+"/children"] = long(1, 200)
	answers["GET /library/metadata/"+frierenS1+"/children@200"] = long(201, 50)
	p, n, _ := newPlexTest(t, answers)
	ctx := context.Background()
	seasons, err := p.Seasons(ctx, PlexSeries(424536))
	if err != nil || len(seasons) != 2 || seasons[0].Number != 0 || seasons[1].Number != 1 || len(seasons[1].Art.Posters) != 1 {
		t.Fatalf("seasons %+v %v", seasons, err)
	}
	eps, err := p.Episodes(ctx, PlexSeries(424536))
	if err != nil {
		t.Fatal(err)
	}
	// Specials last, like TheTVDB's list.
	if len(eps) != 253 || eps[0].Season != 1 || eps[0].Number != 1 || eps[249].Number != 250 || eps[250].Season != 0 {
		t.Fatalf("%d episodes, first %+v", len(eps), eps[0])
	}
	if e := eps[0]; e.Minutes != 24 || strings.Join(e.Directors, ",") != "D" || strings.Join(e.Writers, ",") != "W" || e.ID != 0 {
		t.Errorf("episode %+v", e)
	}
	if e := eps[250]; e.Title != "OO Magic Episode 1: The Magic of Saying What You Are Thinking" || e.Thumb == "" {
		t.Errorf("special %+v", e)
	}
	if c := n.count("GET", frierenShow+"/children"); c != 1 {
		t.Errorf("season list asked %d times, want 1 (cached for both)", c)
	}
	for _, a := range n.requests() {
		if strings.Contains(a.url, "/children") && a.size != "200" {
			t.Errorf("%s asked for %q at once, want 200", a.url, a.size)
		}
	}
	if c := n.count("GET", frierenS1+"/children"); c != 2 {
		t.Errorf("season 1 asked %d times, want 2 pages", c)
	}
}

// An empty page before the totalSize Plex gave is a list cut short: the
// list is incomplete, not silently shorter.
func TestPlexEpisodesCutShort(t *testing.T) {
	answers := frieren()
	answers["GET /library/metadata/"+frierenS1+"/children"] = `200 {"MediaContainer":{"totalSize":300,"Metadata":[{"index":1,"title":"E1"}]}}`
	answers["GET /library/metadata/"+frierenS1+"/children@1"] = `200 {"MediaContainer":{"totalSize":300,"Metadata":[]}}`
	p, _, _ := newPlexTest(t, answers)
	eps, err := p.Episodes(context.Background(), PlexSeries(424536))
	if !errors.Is(err, ErrIncomplete) || len(eps) != 1+3 {
		t.Errorf("%d episodes, err %v; want 4 and incomplete", len(eps), err)
	}
}

// Keys are matched exactly: one that differs from a key Kizuna reads only in
// case, known or new, is left out, whatever it holds.
func TestPlexKeysMatchedExactly(t *testing.T) {
	answers := frieren()
	answers["GET /library/metadata/"+frierenShow] = `200 {"MediaContainer":{"TotalSize":"x","metadata":7,"Metadata":[{
		"title":"Frieren","Title":[{"tag":"x"}],"SUMMARY":{},"studio":"Madhouse","Studio":[{"tag":"y"}],"Index":"one",
		"Genre":[{"tag":"Fantasy","Tag":[1]}],"genre":"x",
		"Role":[{"tag":"A","Role":{},"role":"B","Order":"x"}],"Image":[{"type":"coverPoster","Type":1,"url":"https://x/p.jpg","URL":[]}]}]}}`
	p, _, _ := newPlexTest(t, answers)
	s, err := p.Show(context.Background(), PlexSeries(424536))
	if err != nil {
		t.Fatal(err)
	}
	d := s.Details
	if s.Title != "Frieren" || d.Studio != "Madhouse" || !slices.Equal(d.Genres, []string{"Fantasy"}) ||
		len(d.Cast) != 1 || d.Cast[0].Role != "B" || !slices.Equal(s.Art.Posters, []string{"https://x/p.jpg"}) {
		t.Errorf("title %q studio %q genres %v cast %v posters %v", s.Title, d.Studio, d.Genres, d.Cast, s.Art.Posters)
	}
}

// A key Kizuna reads that holds another kind of value still fails the
// answer: it isn't dropped silently.
func TestPlexWrongKindFails(t *testing.T) {
	answers := frieren()
	answers["GET /library/metadata/"+frierenShow] = `200 {"MediaContainer":{"Metadata":[{"title":"Frieren","index":"one"}]}}`
	p, _, _ := newPlexTest(t, answers)
	if _, err := p.Show(context.Background(), PlexSeries(424536)); err == nil {
		t.Error("no error")
	}
}

// A season list with more seasons than came in its one page is incomplete:
// the seasons that came, and their episodes, are still given.
func TestPlexSeasonListCutShort(t *testing.T) {
	answers := frieren()
	answers["GET /library/metadata/"+frierenShow+"/children"] = `200 {"MediaContainer":{"totalSize":201,"Metadata":[
		{"ratingKey":"` + frierenS1 + `","index":1,"summary":"S1"}]}}`
	p, _, _ := newPlexTest(t, answers)
	ctx := context.Background()
	seasons, err := p.Seasons(ctx, PlexSeries(424536))
	if !errors.Is(err, ErrIncomplete) || len(seasons) != 1 || seasons[0].Summary != "S1" {
		t.Errorf("seasons %v, err %v; want season 1 and incomplete", seasons, err)
	}
	eps, err := p.Episodes(ctx, PlexSeries(424536))
	if !errors.Is(err, ErrIncomplete) || len(eps) != 3 {
		t.Errorf("%d episodes, err %v; want season 1's 3 and incomplete", len(eps), err)
	}
}

// A season that fails leaves the list incomplete: the others still come.
func TestPlexEpisodesIncomplete(t *testing.T) {
	answers := frieren()
	answers["GET /library/metadata/"+frierenS0+"/children"] = "503 {}"
	p, _, _ := newPlexTest(t, answers)
	eps, err := p.Episodes(context.Background(), PlexSeries(424536))
	if !errors.Is(err, ErrIncomplete) || len(eps) != 3 {
		t.Errorf("%d episodes, err %v", len(eps), err)
	}
}

// The cast is cut to 50, in Plex's order; a rating with a country is kept
// under that country.
func TestPlexDetails(t *testing.T) {
	var roles []plexRole
	for i := 60; i > 0; i-- {
		roles = append(roles, plexRole{Tag: fmt.Sprintf("Actor %d", i), Order: i})
	}
	d := plexItem{Role: roles, Rating: "de/16"}.details()
	if len(d.Cast) != 50 || d.Cast[0].Name != "Actor 1" || d.Cast[49].Name != "Actor 50" {
		t.Errorf("%d cast, first %+v", len(d.Cast), d.Cast[0])
	}
	if d.Ratings["DE"] != "16" || len(d.Ratings) != 1 || d.Rating([]string{"DE"}) != "de/16" {
		t.Errorf("ratings %v", d.Ratings)
	}
}

// /images gives each picture with its site.
func TestPlexImages(t *testing.T) {
	p, _, _ := newPlexTest(t, frieren())
	im, err := p.Images(context.Background(), PlexSeries(424536))
	if err != nil {
		t.Fatal(err)
	}
	var src []string
	for _, x := range append(im.Posters, im.Backgrounds...) {
		src = append(src, x.Source)
	}
	if strings.Join(src, ",") != "tmdb,tmdb,imdb,tvdb,tmdb,tvdb,fanarttv" {
		t.Errorf("sources %v", src)
	}
	if im.Backgrounds[2].URL != "https://assets.fanart.tv/fanart/frieren-beyond-journeys-end-651b2c7b0eecb.jpg" {
		t.Errorf("backgrounds %+v", im.Backgrounds)
	}
}

// Without a token nothing is asked.
func TestPlexOff(t *testing.T) {
	f := fetch.New(t.TempDir())
	n := &plexNet{}
	f.SetTransport(n)
	p := NewPlex(f, "")
	if p.Enabled() || (*Plex)(nil).Enabled() {
		t.Error("enabled without a token")
	}
	if _, err := p.Show(context.Background(), PlexSeries(1)); err == nil || len(n.requests()) != 0 {
		t.Errorf("err %v, %d requests", err, len(n.requests()))
	}
}
