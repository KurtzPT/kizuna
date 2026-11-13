package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"kizuna/internal/anidb"
	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/sources"
)

// The plex source: Plex's metadata service, answered by a fake at its real
// address. Plex has TheTVDB 500 (testSeries' series) as p500, and TheMovieDb
// movie 900 (IMDb tt900) as p900.

const plexHost = "metadata.provider.plex.tv"

// plexMatchPath: a match request's type and guid, from its body ("/2/tvdb://500").
func plexMatchPath(r *http.Request) string {
	body, _ := io.ReadAll(r.Body)
	var m struct {
		Type int    `json:"type"`
		GUID string `json:"guid"`
	}
	_ = json.Unmarshal(body, &m)
	return fmt.Sprintf("/%d/%s", m.Type, m.GUID)
}

func plexEpisode(season, n int, title string) string {
	return fmt.Sprintf(`{"index":%d,"parentIndex":%d,"title":%q,"summary":"Plex summary %d-%d","thumb":"https://plex/e%d-%d.jpg",
		"originallyAvailableAt":"2001-01-0%d","duration":1500000,"Director":[{"tag":"Plex Director"}],"Writer":[{"tag":"Plex Writer"}]}`,
		n, season, title, season, n, season, n, n)
}

func plexList(items ...string) string {
	return fmt.Sprintf(`{"MediaContainer":{"offset":0,"size":%d,"totalSize":%d,"Metadata":[%s]}}`, len(items), len(items), strings.Join(items, ","))
}

// plexService answers Plex's service; anything else goes to next.
func plexService(next func(string, map[string][]string) (int, string)) func(string, map[string][]string) (int, string) {
	return func(path string, q map[string][]string) (int, string) {
		switch strings.TrimPrefix(path, plexHost) {
		case "/library/metadata/matches/2/tvdb://500":
			return 200, plexList(`{"ratingKey":"p500"}`)
		case "/library/metadata/matches/1/tmdb://900", "/library/metadata/matches/1/imdb://tt900":
			return 200, plexList(`{"ratingKey":"p900"}`)
		case "/library/metadata/p500":
			return 200, plexList(`{"title":"Plex Name","summary":"From Plex.","studio":"Plex Studio","contentRating":"TV-14",
				"originallyAvailableAt":"2009-04-01","Genre":[{"tag":"Plex Genre"}],"Director":[{"tag":"Plex Show Director"}],
				"Role":[{"tag":"Plex Actor","role":"Hero","order":1}],
				"Image":[{"type":"coverPoster","url":"https://plex/poster.jpg"},{"type":"background","url":"https://plex/bg.jpg"},
				{"type":"clearLogo","url":"https://plex/logo.png"},{"type":"banner","url":"https://plex/banner.jpg"}]}`)
		case "/library/metadata/p900":
			return 200, plexList(`{"title":"Plex Movie","summary":"A Plex movie.","studio":"Movie Studio",
				"Image":[{"type":"coverPoster","url":"https://plex/movie-poster.jpg"}]}`)
		case "/library/metadata/p500/children":
			return 200, plexList(`{"ratingKey":"p500s0","index":0,"title":"Specials"}`,
				`{"ratingKey":"p500s1","index":1,"summary":"Plex season one."}`,
				`{"ratingKey":"p500s2","index":2,"summary":"Plex season two.","Image":[{"type":"coverPoster","url":"https://plex/s2.jpg"}]}`)
		case "/library/metadata/p500s0/children":
			return 200, plexList(plexEpisode(0, 1, "Plex Special"))
		case "/library/metadata/p500s1/children":
			return 200, plexList(plexEpisode(1, 1, "Plex One"), plexEpisode(1, 2, "Plex Two"))
		case "/library/metadata/p500s2/children":
			return 200, plexList(plexEpisode(2, 1, "Plex Three"))
		case "/library/metadata/p500/images":
			return 200, `{"MediaContainer":{"Thumb":[{"key":"https://image.tmdb.org/t/p/original/plex-tmdb.jpg","source":"tmdb"},
				{"key":"https://assets.fanart.tv/poster.jpg","source":"fanarttv"}],
				"Art":[{"key":"https://artworks.thetvdb.com/plex-tvdb-bg.jpg","source":"tvdb"},{"key":"https://plex/gracenote-bg.jpg","source":"gracenote"}]}}`
		}
		if next == nil {
			return 0, ""
		}
		return next(path, q)
	}
}

// withPlex sets up the plex source on net, with a token.
func withPlex(t *testing.T, p *Provider, net *fakeNet) {
	t.Helper()
	f := fetch.New(t.TempDir())
	f.SetTransport(net)
	p.plex = sources.NewPlex(f, "token")
	f.ThrottleAll(0)
}

// plexAsked: requests to Plex's service whose path ends in suffix.
func plexAsked(net *fakeNet, suffix string) int {
	net.mu.Lock()
	defer net.mu.Unlock()
	n := 0
	for path, c := range net.hits {
		if strings.HasPrefix(path, plexHost) && strings.HasSuffix(path, suffix) {
			n += c
		}
	}
	return n
}

// Plex fills the show fields the sources before it in the default lists
// leave empty; what they gave stays.
func TestPlexShowFields(t *testing.T) {
	net := &fakeNet{answer: plexService(testSeries)}
	p := newTestProvider(t, net)
	withPlex(t, p, net)
	for _, root := range []string{"anidb-100", "tvdb-500"} {
		s, err := p.buildShow(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if s.Title != "Kizuna Test" || s.Summary != "From AniDB." {
			t.Errorf("%s: title %q summary %q, want AniDB's", root, s.Title, s.Summary)
		}
		if s.Studio != "Plex Studio" || s.ContentRating != "TV-14" || strings.Join(s.Genres, ",") != "Plex Genre" ||
			len(s.Roles) != 1 || s.Roles[0].Name != "Plex Actor" {
			t.Errorf("%s: studio %q rating %q genres %q cast %+v, want Plex's", root, s.Studio, s.ContentRating, s.Genres, s.Roles)
		}
		if strings.Join(s.Art.Logos, " ") != "https://plex/logo.png" || s.Art.Posters[0] != "https://image.tmdb.org/t/p/original/p.jpg" {
			t.Errorf("%s: logos %q posters %q, want Plex's logo, TheMovieDb's poster first", root, s.Art.Logos, s.Art.Posters)
		}
		if !s.Complete {
			t.Errorf("%s: want complete", root)
		}
	}
	if c := plexAsked(net, "/matches/2/tvdb://500"); c != 1 {
		t.Errorf("matched %d times, want 1 (cached)", c)
	}
	if c := plexAsked(net, "/images"); c != 0 {
		t.Errorf("/images asked %d times without ART_ALL_SOURCES", c)
	}
}

// A later entry gets the summary of the Plex season it sits in, none of
// Plex's series-wide fields, and the season's pictures.
func TestPlexLaterEntry(t *testing.T) {
	net := &fakeNet{answer: plexService(nil)}
	p := newTestProvider(t, net)
	withPlex(t, p, net)
	p.cfg.SummarySources, p.cfg.TitleSources, p.cfg.StudioSources = []string{"plex"}, []string{"plex"}, []string{"plex"}
	p.cfg.PosterSources = []string{"plex"}
	entry := mapping.Entry{AID: 200, TVDB: mapping.Placement{ID: 500, Season: 2, Mapped: true}}
	ctx := context.Background()
	ss := p.newSources(ctx, 200, entry, 500, 0, false, firstEntry{})
	ss.setAniDB(ctx, nil, entry)
	s := &Show{Episodes: map[int][]Ep{1: nil}}
	ss.anidbTargets(entry, plexSeasons(s))
	ss.fill(s)
	if s.Summary != "Plex season two." || s.Title != "" || s.Studio != "" {
		t.Errorf("summary %q title %q studio %q, want season 2's summary only", s.Summary, s.Title, s.Studio)
	}
	if strings.Join(s.Art.Posters, " ") != "https://plex/s2.jpg" {
		t.Errorf("posters %q, want season 2's", s.Art.Posters)
	}
	if !ss.finish(s) {
		t.Error("a source failed")
	}
}

// A series is asked for only by its TheTVDB id; a movie as a movie, by its
// TheMovieDb movie id, else (AniDB says movie) by its IMDb id, which needs
// AniDB's details.
func TestPlexWhatIsAsked(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		entry mapping.Entry
		tvdb  int
		movie bool
		a     *sources.AniDBAnime
		match string // "" = nothing asked
		title string
	}{
		{"series", mapping.Entry{TVDB: mapping.Placement{ID: 500}}, 500, false, nil, "/2/tvdb://500", "Plex Name"},
		{"series without a TheTVDB id", mapping.Entry{TMDB: mapping.Placement{ID: 77}, IMDB: "tt900"}, 0, false, nil, "", ""},
		{"TheMovieDb movie", mapping.Entry{TMDBMovie: 900}, 0, true, nil, "/1/tmdb://900", "Plex Movie"},
		{"IMDb movie", mapping.Entry{IMDB: "tt900"}, 0, false, &sources.AniDBAnime{Type: "Movie"}, "/1/imdb://tt900", "Plex Movie"},
		{"IMDb movie, no details", mapping.Entry{IMDB: "tt900"}, 0, false, nil, "", ""},
		{"IMDb, not a movie", mapping.Entry{IMDB: "tt900"}, 0, false, &sources.AniDBAnime{Type: "TV Series"}, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			net := &fakeNet{answer: plexService(nil)}
			p := newTestProvider(t, net)
			withPlex(t, p, net)
			p.cfg.TitleSources = []string{"plex"}
			ss := p.newSources(ctx, 100, c.entry, c.tvdb, c.entry.TMDBMovie, c.movie, firstEntry{tvdb: true, tmdb: true})
			ss.setAniDB(ctx, c.a, c.entry)
			s := &Show{Episodes: map[int][]Ep{1: nil}}
			ss.anidbTargets(c.entry, plexSeasons(s))
			ss.fill(s)
			asked := plexAsked(net, "/matches"+c.match)
			if all := plexAsked(net, ""); c.match == "" && all != 0 || c.match != "" && (asked != 1 || s.Title != c.title) {
				t.Errorf("asked %d (%d in all), title %q", asked, all, s.Title)
			}
		})
	}
}

// A movie among a series' specials (One Piece Film Red): the show's fields
// come from Plex's movie, its episode from the specials of Plex's series.
func TestPlexMovieAmongSpecials(t *testing.T) {
	net := &fakeNet{answer: plexService(func(path string, q map[string][]string) (int, string) {
		if path == "api.anidb.net/httpapi" {
			return 200, movieXML(300, "Film", "", 0, "2022-08-06")
		}
		return testSeries(path, q)
	})}
	p := newTestProvider(t, net)
	withPlex(t, p, net)
	p.tvdb = sources.NewTVDB(fetch.New(t.TempDir()), "", "", "") // TheTVDB off: Plex's list is the main one
	p.titles.SetForTest(map[int][]anidb.Title{300: {{Type: "main", Lang: "x-jat", Text: "Film"}}})
	p.lists.SetForTest(map[int]mapping.Entry{300: {AID: 300, TVDB: mapping.Placement{ID: 500, Season: 0, Offset: 0, Mapped: true},
		TMDBMovie: 900}})
	p.cfg.SummarySources = []string{"plex"}
	s, err := p.buildShow(context.Background(), "anidb-300")
	if err != nil {
		t.Fatal(err)
	}
	if s.Summary != "A Plex movie." || s.Studio != "Movie Studio" {
		t.Errorf("summary %q studio %q, want the movie's", s.Summary, s.Studio)
	}
	if ep := s.Episode(1, 1); ep == nil || ep.titles["plex"] != "Plex Special" || ep.Minutes != 25 || ep.tvdbEp != 0 {
		t.Errorf("episode %+v, want Plex's special, no TheTVDB episode", ep)
	}
}
