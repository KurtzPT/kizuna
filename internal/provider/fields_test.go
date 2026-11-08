package provider

import (
	"slices"
	"testing"

	"kizuna/internal/config"
	"kizuna/internal/sources"
)

func TestPickFallsBack(t *testing.T) {
	vals := map[string]string{"anidb": "", "tvdb": "from tvdb", "tmdb": "from tmdb"}
	var asked []string
	got, _ := pick([]string{"anidb", "tvdb", "tmdb"}, func(src string) string { asked = append(asked, src); return vals[src] })
	if got != "from tvdb" {
		t.Fatalf("got %q, want tvdb value", got)
	}
	if !slices.Equal(asked, []string{"anidb", "tvdb"}) { // tmdb never fetched
		t.Fatalf("asked %v", asked)
	}
}

func TestImagesAllAndFirst(t *testing.T) {
	art := map[string][]string{"tmdb": nil, "tvdb": {"a", "b"}, "anidb": {"c", "a"}}
	get := func(src string) []string { return art[src] }
	order := []string{"tmdb", "tvdb", "anidb"}
	if got, _ := images(order, true, get); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("all: %v", got)
	}
	if got, _ := images(order, false, get); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("first: %v", got)
	}
}

func TestSeriesGuard(t *testing.T) {
	series := func() sources.Details { return sources.Details{Genres: []string{"series"}, Date: "2004-01-01"} }
	ss := &showSources{tvdb: 1, tmdb: 2}
	ss.tvdbDetails.f, ss.tmdbDetails.f = series, series
	ss.mal.f = func() sources.MALAnime { return sources.MALAnime{Details: sources.Details{Genres: []string{"mal"}}} }
	order := []string{"tvdb", "tmdb", "mal"}
	has := func(d sources.Details) bool { return len(d.Genres) > 0 }

	if got := pd(ss.pickDetails(order, has)).Genres[0]; got != "mal" { // sequel entry: skip series data
		t.Errorf("not first entry: got %q, want mal", got)
	}
	ss.movie = true // a TMDB movie is its own entry
	if got := pd(ss.pickDetails([]string{"tvdb", "tmdb"}, has)).Genres; len(got) != 1 {
		t.Errorf("movie: got %v, want tmdb genres", got)
	}
	ss.movie, ss.series = false, true
	if got := pd(ss.pickDetails(order, has)).Genres[0]; got != "series" {
		t.Errorf("first entry: got %q, want series", got)
	}
}

func pd(d sources.Details, _ string) sources.Details { return d }

func TestANNFallback(t *testing.T) {
	cfg := config.Config{TitleSources: []string{"ann"}, SummarySources: []string{"anidb", "ann"},
		GenreSources: []string{"anidb", "ann"}, StudioSources: []string{"anidb"}}
	ann := sources.ANNAnime{ID: 13, Title: "Cowboy Bebop", Summary: "Bounty hunters.",
		Details: sources.Details{Genres: []string{"Drama"}}}
	ss := &showSources{p: &Provider{cfg: cfg}}
	ss.ann.f = func() sources.ANNAnime { return ann }
	s := &Show{}
	ss.fill(s)
	// ANN's text is used unchanged (its credit is in the README and code only).
	if s.Summary != "Bounty hunters." || s.Title != "Cowboy Bebop" || len(s.Genres) != 1 {
		t.Fatalf("summary %q title %q genres %v", s.Summary, s.Title, s.Genres)
	}
}

func TestUniqueCast(t *testing.T) {
	got := uniqueCast([]sources.Person{
		{Name: "Atsumi Tanezaki", Role: "Frieren", Photo: "a.jpg"},
		{Name: "Yoji Ueda", Role: "Eisen"},
		{Name: "Yoji Ueda ", Role: "eisen", Photo: "u.jpg"}, // same person and character
		{Name: "Yoji Ueda", Role: "Narrator"},               // another character: kept
	})
	want := []sources.Person{
		{Name: "Atsumi Tanezaki", Role: "Frieren", Photo: "a.jpg"},
		{Name: "Yoji Ueda", Role: "Eisen", Photo: "u.jpg"},
		{Name: "Yoji Ueda", Role: "Narrator"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
