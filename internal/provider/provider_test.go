package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kizuna/internal/anidb"
	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/sources"
)

func TestFillAbsolute(t *testing.T) {
	eps := []sources.Episode{{Season: 1, Number: 1}, {Season: 1, Number: 2}, {Season: 2, Number: 1}, {Season: 2, Number: 2}, {Season: 0, Number: 1}}
	sources.FillAbsolute(eps)
	want := []int{1, 2, 3, 4, 0}
	for i, w := range want {
		if eps[i].Absolute != w {
			t.Errorf("episode %d: absolute %d, want %d", i, eps[i].Absolute, w)
		}
	}
}

func TestKeyRe(t *testing.T) {
	for key, ok := range map[string]bool{"anidb-1": true, "tvdb-2-s0": true, "tvdb-2-s2-e33": true, "foo-1": false, "anidb-1-e2": false} {
		if (keyRe.FindStringSubmatch(key) != nil) != ok {
			t.Errorf("%s: want match=%v", key, ok)
		}
	}
}

func TestMatchByOtherIDs(t *testing.T) {
	l := &mapping.Lists{}
	l.SetForTest(map[int]mapping.Entry{
		69:  {AID: 69, TVDB: 81797, TVDBSeason: -1, TMDBTV: 37854, IMDB: "tt0388629", MAL: 21, AniList: 21},
		100: {AID: 100, TVDB: 81797, TVDBSeason: 2, MAL: 99},
	})
	p := &Provider{lists: l}
	anidbMode, tvdbMode := Modes["anidb"], Modes["tvdb"]
	cases := []struct {
		title, guid string
		mode        Mode
		want        string
	}{
		{"One Piece [mal-21]", "", anidbMode, "anidb-69"},
		{"One Piece [mal-99]", "", anidbMode, "anidb-100"},
		{"One Piece [imdb-tt0388629]", "", anidbMode, "anidb-69"},
		{"One Piece {anilist-21}", "", tvdbMode, "tvdb-81797"},
		{"One Piece", "com.plexapp.agents.themoviedb://37854?lang=en", anidbMode, "anidb-69"},
		{"One Piece", "com.plexapp.agents.thetvdb://81797?lang=en", anidbMode, "anidb-69"},
		{"One Piece", "imdb://tt0388629", tvdbMode, "tvdb-81797"},
	}
	for _, c := range cases {
		got, _ := p.candidates(c.mode, c.title, 0, "", c.guid, false)
		if len(got) != 1 || got[0].root != c.want {
			t.Errorf("%q %q: got %v, want %s", c.title, c.guid, got, c.want)
		}
	}
}

func TestPlaceholderEpisodes(t *testing.T) {
	s := &Show{Root: "anidb-1", Episodes: map[int][]Ep{1: nil}, Placeholders: true}
	if ep := s.Episode(1, 12); ep == nil || ep.Index != 12 {
		t.Errorf("S01E12: got %+v, want a placeholder", ep)
	}
	if s.Episode(0, 101) == nil || s.Episode(2, 1) != nil || s.Episode(1, 0) != nil {
		t.Error("placeholders only for S00/S01 with an episode number")
	}
	p := &Provider{}
	if it := p.seasonItem(Modes["anidb"], s, 0, false); it == nil {
		t.Error("specials season missing")
	}
	s.Placeholders = false
	if s.Episode(1, 12) != nil {
		t.Error("no placeholders on a complete show")
	}
}

func TestPruneShowsKeepsFresh(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	p := &Provider{shows: map[string]cachedShow{
		"anidb-1": {&Show{Complete: true}, time.Now()},
		"anidb-2": {&Show{Complete: true}, old},
		"anidb-3": {&Show{}, time.Now().Add(-5 * time.Minute)}, // incomplete: 2 min TTL
	}}
	p.pruneShows()
	if _, ok := p.shows["anidb-1"]; !ok || len(p.shows) != 1 {
		t.Fatalf("left %v", p.shows)
	}
}

func TestAutoModePerFolder(t *testing.T) {
	l := &mapping.Lists{}
	l.SetForTest(map[int]mapping.Entry{
		69:  {AID: 69, TVDB: 81797, TVDBSeason: -1, MAL: 21},
		100: {AID: 100, TVDB: 81797, TVDBSeason: 2},
		500: {AID: 500, MAL: 500}, // a movie: no TVDB series
	})
	auto := Modes["auto"]
	cases := []struct {
		def, title, guid, want string
	}{
		{"anidb", "One Piece [anidb-100]", "", "anidb-100"},                                   // AniDB tag: /anidb rules
		{"anidb", "One Piece [tvdb-81797]", "", "tvdb-81797"},                                 // TVDB tag: /tvdb rules
		{"anidb", "One Piece [tvdb3-81797]", "", "tvdb-81797"},                                // HAMA's tvdb3 tag too
		{"tvdb", "One Piece [anidb2-69]", "", "anidb-69"},                                     // a tag beats AUTO_DEFAULT
		{"anidb", "One Piece [mal-21]", "", "anidb-69"},                                       // other ids: AUTO_DEFAULT
		{"tvdb", "One Piece [mal-21]", "", "tvdb-81797"},                                      //
		{"tvdb", "Movie [mal-500]", "", "anidb-500"},                                          // no TVDB series: stays AniDB
		{"anidb", "One Piece", "com.plexapp.agents.hama://tvdb3-81797?lang=en", "tvdb-81797"}, // HAMA GUID
		{"anidb", "One Piece", "tv.plex.agents.custom.kizuna.tvdb://show/tvdb-81797", "tvdb-81797"},
		{"tvdb", "One Piece", "tv.plex.agents.custom.kizuna.anidb://show/anidb-100", "anidb-100"},
	}
	for _, c := range cases {
		p := &Provider{lists: l}
		p.cfg.AutoDefault = c.def
		got, _ := p.candidates(auto, c.title, 0, "", c.guid, false)
		if len(got) != 1 || got[0].root != c.want {
			t.Errorf("default %s, %q %q: got %v, want %s", c.def, c.title, c.guid, got, c.want)
		}
	}
}

func TestEpisodeTitlePick(t *testing.T) {
	ep := &Ep{anyTitle: "Daiichiwa"}
	ep.setTitle("anidb", "Episode 1") // AniDB's placeholder: doesn't count
	ep.setTitle("tvdb", "The Beginning")
	ep.setTitle("tmdb", "Beginning")
	cases := []struct {
		order []string
		want  string
	}{
		{[]string{"anidb", "tvdb", "tmdb"}, "The Beginning"}, // placeholder skipped
		{[]string{"tmdb", "anidb"}, "Beginning"},
		{[]string{"anidb"}, "Daiichiwa"}, // AniDB in another language beats a placeholder
		{nil, ""},                        // EPISODE_TITLES=none -> "Episode N"
	}
	for _, c := range cases {
		if got, _ := episodeTitle(ep, c.order); got != c.want {
			t.Errorf("%v: got %q, want %q", c.order, got, c.want)
		}
	}
	credit := &Ep{anyTitle: "Opening 1a"} // only AniDB lists openings
	if got, _ := episodeTitle(credit, []string{"tvdb", "tmdb"}); got != "Opening 1a" {
		t.Errorf("AniDB-only episode, anidb not listed: got %q, want AniDB's title", got)
	}
	only := &Ep{}
	only.setTitle("tvdb", "TBA")
	if got, _ := episodeTitle(only, []string{"anidb", "tvdb"}); got != "TBA" {
		t.Errorf("only a placeholder: got %q, want it kept", got)
	}
}

func TestTVDBEpisodeTitlesFromEveryEntry(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(dir)
	// the series' second AniDB entry is only in the cache; its episode 3 is a
	// recap that TheTVDB lists as special 1
	second := `<anime id="200"><type>TV Series</type><enddate>2010-01-01</enddate><episodes>
  <episode><epno type="1">1</epno><title xml:lang="en">Second Start</title></episode>
  <episode><epno type="1">3</epno><title xml:lang="en">Recap</title></episode>
</episodes></anime>`
	if err := os.MkdirAll(filepath.Join(dir, "anidb", "anime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "anidb", "anime", "200.xml"), []byte(second), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &mapping.Lists{}
	l.SetForTest(map[int]mapping.Entry{
		100: {AID: 100, TVDB: 500, TVDBSeason: 1},
		200: {AID: 200, TVDB: 500, TVDBSeason: 2, Episodes: []mapping.EpisodeMap{{AniDBSeason: 1, AniDBEp: 3, TVDBSeason: 0, TVDBEp: 1}}},
		300: {AID: 300, TVDB: 500, TVDBSeason: 3}, // never fetched
	})
	p := &Provider{lists: l, anidb: sources.NewAniDB(f, dir, "", "1"), tvdb: sources.NewTVDB(f, "", ""), tmdb: sources.NewTMDB(f, "")}
	p.cfg.EpisodeTitles, p.cfg.EpisodeLangs = []string{"anidb", "tvdb"}, []string{"en"}

	first := &sources.AniDBAnime{AID: 100, Episodes: []sources.AniDBEpisode{
		{Season: 1, Index: 1, Titles: []anidb.Title{{Lang: "x-jat", Text: "Hajimari"}}}, // no English title
	}}
	s := &Show{Root: "tvdb-500", Episodes: map[int][]Ep{
		1: {{Season: 1, Index: 1, listed: spot{1, 1}}},
		2: {{Season: 2, Index: 13, listed: spot{2, 1}}},
		0: {{Season: 0, Index: 1, listed: spot{0, 1}}},
	}}
	s.eachEpisode(func(ep *Ep) { ep.setTitle("tvdb", fmt.Sprintf("TVDB S%dE%d", ep.listed.season, ep.listed.number)) })

	if ready := p.tvdbEpisodeTitles(context.Background(), s, 500, 0, "tvdb", first); !ready {
		t.Error("no AniDB client: nothing can be fetched, so nothing to wait for")
	}
	p.pickEpisodeTitles(s)
	want := map[int]string{1: "TVDB S1E1", 2: "Second Start", 0: "Recap"} // S01: no English AniDB title -> TVDB
	for season, w := range want {
		if got := s.Episodes[season][0].Title; got != w {
			t.Errorf("season %d: got %q, want %q", season, got, w)
		}
	}
	p.cfg.EpisodeTitles = []string{"anidb"} // only AniDB: its title in any language
	p.pickEpisodeTitles(s)
	if got := s.Episodes[1][0].Title; got != "Hajimari" {
		t.Errorf("anidb only: got %q, want the romaji title", got)
	}
}
