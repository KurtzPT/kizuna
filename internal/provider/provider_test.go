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
		69: {AID: 69, TVDB: mapping.Placement{ID: 81797, Season: -1, Mapped: true},
			TMDB: mapping.Placement{ID: 37854}, IMDB: "tt0388629", MAL: 21, AniList: 21},
		100: {AID: 100, TVDB: mapping.Placement{ID: 81797, Season: 2, Mapped: true}, MAL: 99},
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
		got, _ := p.searchCandidates(c.mode, c.title, MatchRequest{GUID: c.guid}, nil)
		if len(got) != 1 || got[0].root != c.want {
			t.Errorf("%q %q: got %v, want %s", c.title, c.guid, got, c.want)
		}
	}
}

// TheMovieDb numbers TV shows and movies separately, so a [tmdb-N] tag or a
// tmdb://N GUID can be one anime's TV show and another anime's movie. Only
// a movie with the id: that movie. Both: the one the title search finds,
// else the TV show.
func TestMatchTMDBMovieOrTV(t *testing.T) {
	l := &mapping.Lists{}
	l.SetForTest(map[int]mapping.Entry{
		100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}, TMDB: mapping.Placement{ID: 77}},
		101: {AID: 101, TVDB: mapping.Placement{ID: 500, Season: 2, Mapped: true}, TMDB: mapping.Placement{ID: 77}},
		200: {AID: 200, TMDBMovie: 77},
		300: {AID: 300, TMDBMovie: 88},
	})
	titles := &anidb.Titles{}
	titles.SetForTest(map[int][]anidb.Title{
		100: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test"}},
		101: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test Second Season"}},
		200: {{Type: "main", Lang: "x-jat", Text: "Some Movie"}},
		300: {{Type: "main", Lang: "x-jat", Text: "Other Movie"}},
	})
	p := &Provider{lists: l, titles: titles}
	cases := []struct{ title, guid, want string }{
		{"Some Movie [tmdb-77]", "", "anidb-200"},
		{"Some Movie", "tmdb://77", "anidb-200"},
		{"Kizuna Test [tmdb-77]", "", "anidb-100"},
		{"Kizuna Test Second Season [tmdb-77]", "", "anidb-100"}, // the TV show's first entry, as before
		{"Unknown Name [tmdb-77]", "", "anidb-100"},
		{"Anything [tmdb-88]", "", "anidb-300"},
		{"Anything", "com.plexapp.agents.themoviedb://88?lang=en", "anidb-300"},
	}
	for _, c := range cases {
		got, how := p.searchCandidates(Modes["anidb"], c.title, MatchRequest{GUID: c.guid}, nil)
		if len(got) != 1 || got[0].root != c.want {
			t.Errorf("%q %q: got %v (%s), want %s", c.title, c.guid, got, how, c.want)
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
		69:  {AID: 69, TVDB: mapping.Placement{ID: 81797, Season: -1, Mapped: true}, MAL: 21},
		100: {AID: 100, TVDB: mapping.Placement{ID: 81797, Season: 2, Mapped: true}},
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
		got, _ := p.searchCandidates(auto, c.title, MatchRequest{GUID: c.guid}, nil)
		if len(got) != 1 || got[0].root != c.want {
			t.Errorf("default %s, %q %q: got %v, want %s", c.def, c.title, c.guid, got, c.want)
		}
	}
}

// An AniDB special the mapping doesn't list is the TVDB special with the same
// number, but only on the entry that starts the series (Air Gear S1 = TVDB
// S00E01); later entries and entries living in TVDB season 0 need an exception.
func TestTVDBSpotSpecials(t *testing.T) {
	l := &mapping.Lists{}
	l.SetForTest(map[int]mapping.Entry{
		4196: {AID: 4196, TVDB: mapping.Placement{ID: 79796, Season: 1, Mapped: true}},
		4197: {AID: 4197, TVDB: mapping.Placement{ID: 79796, Season: 2, Mapped: true}},
		7669: {AID: 7669, TVDB: mapping.Placement{ID: 79796, Season: 0,
			Episodes: []mapping.EpisodeMap{{AniDBSeason: 1, AniDBEp: 1, Season: 0, Ep: 2}}, Mapped: true}},
		7670: {AID: 7670, TVDB: mapping.Placement{ID: 79796, Season: 0,
			Episodes: []mapping.EpisodeMap{{AniDBSeason: 0, AniDBEp: 1, Season: 0, Ep: 9}}, Mapped: true}},
	})
	p := &Provider{lists: l}
	cases := []struct {
		aid, season, index int
		want               spot
		ok                 bool
	}{
		{4196, 0, 1, spot{0, 1}, true}, // first entry: S1 -> S00E01
		{4196, 0, 101, spot{}, false},  // an opening has no TVDB number
		{4196, 1, 3, spot{1, 3}, true}, // regular episodes unchanged
		{4197, 0, 1, spot{}, false},    // later entry: would be the first entry's special
		{7669, 0, 1, spot{}, false},    // lives in TVDB season 0: its numbers are taken
		{7669, 1, 1, spot{0, 2}, true}, // its own episodes still follow the mapping
		{7670, 0, 1, spot{0, 9}, true}, // an exception always wins
	}
	for _, c := range cases {
		got, ok := p.spotsOf(l.ByAID(c.aid)).at("tvdb", c.season, c.index)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("aid %d %d/%d: got %v ok=%v, want %v ok=%v", c.aid, c.season, c.index, got, ok, c.want, c.ok)
		}
	}
}

// TheMovieDb gets its own spot when the mapping has a TMDB season, with
// TheTVDB's rules for unlisted specials and later entries; otherwise
// TheTVDB's spot (Re:Zero: TVDB S2 is TMDB S1 from episode 27).
func TestTMDBSpot(t *testing.T) {
	l := &mapping.Lists{}
	l.SetForTest(map[int]mapping.Entry{
		11370: {AID: 11370, TVDB: mapping.Placement{ID: 305089, Season: 1, Mapped: true},
			TMDB: mapping.Placement{ID: 65942, Mapped: true, Season: 1}},
		14792: {AID: 14792, TVDB: mapping.Placement{ID: 305089, Season: 2, Mapped: true},
			TMDB: mapping.Placement{ID: 65942, Mapped: true, Season: 1, Offset: 26}},
		13430: {AID: 13430, TVDB: mapping.Placement{ID: 305089, Season: 0, Mapped: true},
			TMDB: mapping.Placement{ID: 65942, Mapped: true, Season: 0,
				Episodes: []mapping.EpisodeMap{{AniDBSeason: 0, AniDBEp: 2, Season: 0, Ep: 5}}}},
		16000: {AID: 16000, TVDB: mapping.Placement{ID: 305089, Season: 3, Mapped: true},
			TMDB: mapping.Placement{ID: 65942, Mapped: true, Season: -1}},
		500: {AID: 500, TVDB: mapping.Placement{ID: 900, Season: 2, Offset: 3, Mapped: true}, TMDB: mapping.Placement{ID: 901}},
		600: {AID: 600, TMDB: mapping.Placement{ID: 902, Mapped: true, Season: 2}},
	})
	p := &Provider{lists: l}
	cases := []struct {
		aid, season, index int
		want               spot
		ok                 bool
	}{
		{11370, 0, 1, spot{0, 1}, true},  // first entry: S1 -> S00E01
		{14792, 1, 1, spot{1, 27}, true}, // TMDB's own season and offset
		{14792, 0, 1, spot{}, false},     // later entry: would be the first entry's special
		{13430, 1, 1, spot{0, 1}, true},  // lives in TMDB season 0
		{13430, 0, 1, spot{}, false},     // so its unlisted specials have no number
		{13430, 0, 2, spot{0, 5}, true},  // an exception always wins
		{16000, 1, 1, spot{}, false},     // absolute from 1 on a later entry
		{500, 1, 1, spot{2, 4}, true},    // no TMDB season: TheTVDB's spot
		{600, 1, 3, spot{2, 3}, true},    // TMDB only
		{600, 0, 1, spot{0, 1}, true},    // the show's only entry starts it, as on TheTVDB
	}
	for _, c := range cases {
		got, ok := p.spotsOf(l.ByAID(c.aid)).at("tmdb", c.season, c.index)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("aid %d %d/%d: got %v ok=%v, want %v ok=%v", c.aid, c.season, c.index, got, ok, c.want, c.ok)
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
		100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}},
		200: {AID: 200, TVDB: mapping.Placement{ID: 500, Season: 2,
			Episodes: []mapping.EpisodeMap{{AniDBSeason: 1, AniDBEp: 3, Season: 0, Ep: 1}}, Mapped: true}},
		300: {AID: 300, TVDB: mapping.Placement{ID: 500, Season: 3, Mapped: true}}, // never fetched
	})
	p := &Provider{lists: l, anidb: sources.NewAniDB(f, dir, "", "1"), tvdb: sources.NewTVDB(f, "", "", ""), tmdb: sources.NewTMDB(f, "")}
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

	if _, ready := p.tvdbEpisodeTitles(context.Background(), s, newTVDBBuild(500, 0, "tvdb", first, nil)); !ready {
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

// The /tvdb pairing walk places each entry on both sites, either of which
// can hold the episode list: an entry of another TMDB show has no place on
// this show's TMDB list, and an entry without a TMDB season is placed on
// TheMovieDb like on TheTVDB.
func TestTVDBPairingPerSite(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(dir)
	if err := os.MkdirAll(filepath.Join(dir, "anidb", "anime"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, aid := range []int{200, 300} {
		xml := fmt.Sprintf(`<anime id="%d"><type>TV Series</type><enddate>2010-01-01</enddate><episodes>
  <episode><epno type="1">1</epno><title xml:lang="en">Entry %d</title></episode></episodes></anime>`, aid, aid)
		if err := os.WriteFile(filepath.Join(dir, "anidb", "anime", fmt.Sprintf("%d.xml", aid)), []byte(xml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := &mapping.Lists{}
	l.SetForTest(map[int]mapping.Entry{
		100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true},
			TMDB: mapping.Placement{ID: 77, Season: 1, Offset: 1, Mapped: true}},
		200: {AID: 200, TVDB: mapping.Placement{ID: 500, Season: 2, Mapped: true},
			TMDB: mapping.Placement{ID: 88, Season: 1, Mapped: true}}, // another TMDB show
		300: {AID: 300, TVDB: mapping.Placement{ID: 500, Season: 3, Mapped: true},
			TMDB: mapping.Placement{ID: 77, Season: 9}}, // no TMDB season: placed like on TheTVDB
	})
	p := &Provider{lists: l, anidb: sources.NewAniDB(f, dir, "", "1")}
	p.cfg.EpisodeLangs = []string{"en"}
	first := &sources.AniDBAnime{AID: 100, Episodes: []sources.AniDBEpisode{
		{Season: 1, Index: 1, Titles: []anidb.Title{{Lang: "en", Text: "Entry 100"}}}}}

	cases := []struct {
		list   string
		titles map[spot]string
		other  map[spot]otherPlace
	}{
		{"tvdb",
			map[spot]string{{1, 1}: "Entry 100", {2, 1}: "Entry 200", {3, 1}: "Entry 300"},
			// 200 is on another TMDB show: no pairing, not the absolute-number fallback
			map[spot]otherPlace{{1, 1}: {spot{1, 2}, true}, {2, 1}: {}, {3, 1}: {spot{3, 1}, true}}},
		{"tmdb",
			map[spot]string{{1, 2}: "Entry 100", {3, 1}: "Entry 300"}, // 200 isn't on this list
			map[spot]otherPlace{{1, 2}: {spot{1, 1}, true}, {3, 1}: {spot{3, 1}, true}}},
	}
	for _, c := range cases {
		pr, ready := p.tvdbPairing(newTVDBBuild(500, 77, c.list, first, nil), false)
		if !ready {
			t.Errorf("%s list: every entry is cached, want ready", c.list)
		}
		if len(pr.titles) != len(c.titles) || len(pr.other) != len(c.other) {
			t.Errorf("%s list: titles %v other %v, want %v %v", c.list, pr.titles, pr.other, c.titles, c.other)
		}
		for sp, w := range c.titles {
			if got := pr.titles[sp].title; got != w {
				t.Errorf("%s list: title at %v = %q, want %q", c.list, sp, got, w)
			}
		}
		for sp, w := range c.other {
			if got, ok := pr.other[sp]; !ok || got != w {
				t.Errorf("%s list: other site's spot for %v = %v (%v), want %v", c.list, sp, got, ok, w)
			}
		}
	}
}
