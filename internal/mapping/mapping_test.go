package mapping

import "testing"

func TestStartsSeries(t *testing.T) {
	l := &Lists{}
	l.SetForTest(map[int]Entry{
		1: {AID: 1, TVDB: Placement{ID: 100, Season: 1, Mapped: true}},
		2: {AID: 2, TVDB: Placement{ID: 100, Season: 2, Mapped: true}},
		3: {AID: 3, TMDB: Placement{ID: 500}},
		4: {AID: 4, TMDB: Placement{ID: 600}},
		5: {AID: 5, TMDB: Placement{ID: 600}},
	})
	for aid, want := range map[int]bool{1: true, 2: false, 3: true, 4: false, 9: false} {
		if got := l.StartsSeries(aid); got != want {
			t.Errorf("aid %d: got %v, want %v", aid, got, want)
		}
	}
}

// StartsShow picks the first entry of a series the same way on both sites,
// but a TheMovieDb placement without a TMDB season (not Mapped) never starts
// its show by itself: the lowest id does, as when no entry starts it.
func TestStartsShow(t *testing.T) {
	l := &Lists{}
	l.SetForTest(map[int]Entry{
		1: {AID: 1, TVDB: Placement{ID: 100, Season: 2, Mapped: true}, TMDB: Placement{ID: 500, Season: 1}},
		2: {AID: 2, TVDB: Placement{ID: 100, Season: 1, Mapped: true}, TMDB: Placement{ID: 500, Season: 1}},
		3: {AID: 3, TMDB: Placement{ID: 600, Season: 2, Mapped: true}},
		4: {AID: 4, TMDB: Placement{ID: 600, Season: -1, Mapped: true}},
	})
	for _, c := range []struct {
		site string
		aid  int
		want bool
	}{
		{"tvdb", 1, false}, {"tvdb", 2, true}, // season 1 wins over the lower id
		{"tmdb", 1, true}, {"tmdb", 2, false}, // unmapped: the lowest id
		{"tmdb", 3, false}, {"tmdb", 4, true}, // absolute without offset starts it
		{"tvdb", 3, false}, // no TVDB series
		{"anidb", 1, false},
	} {
		if got := l.StartsShow(c.site, c.aid); got != c.want {
			t.Errorf("StartsShow(%s, %d) = %v, want %v", c.site, c.aid, got, c.want)
		}
	}
	if l.FirstAID(100) != 2 {
		t.Errorf("FirstAID(100) = %d, want 2", l.FirstAID(100))
	}
}

// A TheTVDB placement places episodes by its season and offset; a
// TheMovieDb one only when Mapped, else only by its exceptions.
func TestPlacementMapped(t *testing.T) {
	ex := []EpisodeMap{{AniDBSeason: 1, AniDBEp: 2, Season: 3, Ep: 7}}
	mapped := Placement{ID: 1, Season: 2, Offset: 5, Mapped: true, Episodes: ex}
	unmapped := Placement{ID: 1, Season: 2, Offset: 5, Episodes: ex}
	if s, n, ok := mapped.Episode(1, 1); !ok || s != 2 || n != 6 {
		t.Errorf("mapped 1: got S%d E%d ok=%v, want S2 E6", s, n, ok)
	}
	if _, _, ok := unmapped.Episode(1, 1); ok {
		t.Error("unmapped: an unlisted episode has no place")
	}
	for _, pl := range []Placement{mapped, unmapped} {
		if s, n, ok := pl.Episode(1, 2); !ok || s != 3 || n != 7 {
			t.Errorf("exception: got S%d E%d ok=%v, want S3 E7", s, n, ok)
		}
	}
	if _, _, ok := (Placement{Season: 1, Mapped: true}).Episode(1, 1); ok {
		t.Error("no series id: no place")
	}
}

func TestMergeFillsOnlyMissing(t *testing.T) {
	al, _ := parseAnimeLists([]byte(`<anime-list>
  <anime anidbid="69" tvdbid="81797" defaulttvdbseason="a" imdbid=""/>
  <anime anidbid="7" tvdbid="movie" tmdbid="128"/>
  <anime anidbid="8" tvdbid="unknown"/>
</anime-list>`))
	fr, err := parseFribb([]byte(`[
  {"anidb_id": 69, "tvdb_id": 99999, "mal_id": 21, "imdb_id": ["tt0388629"], "themoviedb_id": {"tv": 37854}},
  {"anidb_id": 7, "tvdb_id": 55555, "mal_id": 164, "themoviedb_id": {"movie": [999]}},
  {"anidb_id": 8, "tvdb_id": 77777, "season": {"tvdb": 2}, "episode_offset": {"tvdb": 12}},
  {"anidb_id": 9, "tvdb_id": 88888, "anilist_id": 5},
  {"mal_id": 12345}
]`))
	if err != nil {
		t.Fatal(err)
	}
	entries := map[int]Entry{}
	merge(entries, al)
	added, _ := merge(entries, fr)
	if added != 1 {
		t.Errorf("added %d, want 1 (aid 9; entries without an AniDB id are skipped)", added)
	}
	op := entries[69]
	if op.TVDB.ID != 81797 || op.TVDB.Season != -1 || op.MAL != 21 || op.IMDB != "tt0388629" || op.TMDB.ID != 37854 {
		t.Errorf("69: first list must win, fallback fills gaps: %+v", op)
	}
	if m := entries[7]; m.TVDB.ID != 0 || m.TMDBMovie != 128 || m.MAL != 164 {
		t.Errorf("7: explicit tvdbid=movie must not get a TVDB id: %+v", m)
	}
	if u := entries[8]; u.TVDB.ID != 77777 || u.TVDB.Season != 2 || u.TVDB.Offset != 12 {
		t.Errorf("8: unknown TVDB id should be filled with its season/offset: %+v", u)
	}
	if n := entries[9]; n.TVDB.Season != -1 || n.AniList != 5 {
		t.Errorf("9: Fribb entry without season is absolute: %+v", n)
	}

	l := &Lists{}
	l.SetForTest(entries)
	for src, id := range map[string]string{"mal": "21", "imdb": "tt0388629", "tmdb": "37854", "tvdb": "81797"} {
		if got := l.AIDFor(src, id); got != 69 {
			t.Errorf("AIDFor(%s, %s) = %d, want 69", src, id, got)
		}
	}
	if got := l.AIDFor("anilist", "5"); got != 9 {
		t.Errorf("AIDFor(anilist) = %d, want 9", got)
	}
}

func TestEpisodeExceptions(t *testing.T) {
	// recaps 9 and 18 are TVDB specials, so the episodes after them shift back
	list, err := parseAnimeLists([]byte(`<anime-list>
  <anime anidbid="10" tvdbid="500" defaulttvdbseason="1" episodeoffset="">
    <mapping-list>
      <mapping anidbseason="0" tvdbseason="0">;1-4;2-0;</mapping>
      <mapping anidbseason="1" tvdbseason="0">;9-1;18-2;</mapping>
      <mapping anidbseason="1" tvdbseason="1" start="10" end="17" offset="-1"/>
      <mapping anidbseason="1" tvdbseason="1" start="19" end="26" offset="-2"/>
    </mapping-list>
  </anime>
  <anime anidbid="11" tvdbid="500" defaulttvdbseason="a" episodeoffset="26"/>
</anime-list>`))
	if err != nil {
		t.Fatal(err)
	}
	e := list[10]
	cases := []struct {
		season, ep, wantSeason, wantEp int
		ok                             bool
	}{
		{1, 5, 1, 5, true},    // default: season 1, no offset
		{1, 9, 0, 1, true},    // recap -> TVDB special 1
		{1, 12, 1, 11, true},  // range, offset -1
		{1, 20, 1, 18, true},  // range, offset -2
		{0, 1, 0, 4, true},    // AniDB special 1 -> TVDB S00E04
		{0, 2, 0, 0, false},   // "2-0": no TVDB episode
		{0, 3, 0, 3, true},    // unlisted special: same number on TVDB
		{0, 101, 0, 0, false}, // unlisted opening: no TVDB number
	}
	for _, c := range cases {
		s, n, ok := e.TVDB.Episode(c.season, c.ep)
		if ok != c.ok || (ok && (s != c.wantSeason || n != c.wantEp)) {
			t.Errorf("AniDB %d/%d: got S%d E%d ok=%v, want S%d E%d ok=%v", c.season, c.ep, s, n, ok, c.wantSeason, c.wantEp, c.ok)
		}
	}
	if s, n, ok := list[11].TVDB.Episode(1, 1); !ok || s != -1 || n != 27 {
		t.Errorf("absolute entry: got S%d E%d ok=%v, want absolute 27", s, n, ok)
	}
	// a later list fills a missing TVDB id together with its exceptions
	dst := map[int]Entry{10: {AID: 10}}
	merge(dst, list)
	if len(dst[10].TVDB.Episodes) == 0 || dst[10].TVDB.ID != 500 {
		t.Errorf("merge: %+v", dst[10])
	}
}

// TheMovieDb is placed by its own season, offset and <mapping>s, which can
// differ from TheTVDB's (real Anime-Lists entries, trimmed).
func TestTMDBEpisode(t *testing.T) {
	list, err := parseAnimeLists([]byte(`<anime-list>
<anime anidbid="69" tvdbid="81797" defaulttvdbseason="a" episodeoffset="" tmdbtv="37854" tmdbseason="a" tmdboffset="" tmdbid="" imdbid="">
  <name>One Piece</name>
  <mapping-list>
    <mapping anidbseason="0" tvdbseason="0">;1-27;2-3;6-0;</mapping>
    <mapping anidbseason="1" tmdbseason="1" start="1" end="61" offset="0"/>
    <mapping anidbseason="1" tmdbseason="2" start="62" end="77" offset="-61"/>
    <mapping anidbseason="1" tmdbseason="22" start="1089" end="1155" offset="-1088"/>
    <mapping anidbseason="1" tvdbseason="1" start="1" end="8" offset="0"/>
    <mapping anidbseason="1" tvdbseason="5" start="61" end="69" offset="-60"/>
  </mapping-list>
</anime>
<anime anidbid="4880" tvdbid="79824" defaulttvdbseason="a" episodeoffset="" tmdbtv="31910" tmdbseason="a" tmdboffset="" tmdbid="" imdbid="">
  <name>Naruto Shippuuden</name>
  <mapping-list>
    <mapping anidbseason="0" tvdbseason="0" tmdbseason="0">;1-0;</mapping>
    <mapping anidbseason="1" tmdbseason="10" start="197" end="221" offset="-196"/>
    <mapping anidbseason="1" tmdbseason="11" start="222" end="242" offset="-221"/>
    <mapping anidbseason="1" tvdbseason="1" tmdbseason="1" start="1" end="32" offset="0"/>
    <mapping anidbseason="1" tvdbseason="10" start="197" end="222" offset="-196"/>
    <mapping anidbseason="1" tvdbseason="11" start="223" end="242" offset="-222"/>
  </mapping-list>
</anime>
<anime anidbid="377" tvdbid="75113" defaulttvdbseason="a" episodeoffset="" tmdbtv="34163" tmdbseason="1" tmdboffset="" tmdbid="" imdbid="">
  <name>Urusei Yatsura</name>
  <mapping-list>
    <mapping anidbseason="1" tmdbseason="1" offset="23">;1-1;1-2;2-3;2-4;22-43;22-44;23-45;46-70;</mapping>
    <mapping anidbseason="1" tvdbseason="1" start="1" end="54"/>
  </mapping-list>
</anime>
<anime anidbid="11370" tvdbid="305089" defaulttvdbseason="1" episodeoffset="" tmdbtv="65942" tmdbseason="1" tmdboffset="" tmdbid="" imdbid=""/>
<anime anidbid="14792" tvdbid="305089" defaulttvdbseason="2" episodeoffset="" tmdbtv="65942" tmdbseason="1" tmdboffset="26" tmdbid="" imdbid=""/>
<anime anidbid="13430" tvdbid="305089" defaulttvdbseason="0" episodeoffset="" tmdbtv="65942" tmdbseason="0" tmdboffset="" tmdbid="" imdbid=""/>
<anime anidbid="23" tvdbid="76885" defaulttvdbseason="1" episodeoffset="" tmdbtv="" tmdbseason="" tmdbid="" imdbid=""/>
</anime-list>`))
	if err != nil {
		t.Fatal(err)
	}
	type c struct {
		aid, as, ep, s, n int
		ok                bool
	}
	for _, x := range []c{
		// One Piece: TMDB seasons from its own ranges, not TheTVDB's
		{69, 1, 1, 1, 1, true}, {69, 1, 61, 1, 61, true}, {69, 1, 62, 2, 1, true}, {69, 1, 1100, 22, 12, true},
		{69, 1, 1200, -1, 1200, true}, // no TMDB range: tmdbseason="a"
		{69, 0, 1, 0, 1, true},        // TVDB's special pairs don't apply
		// Naruto Shippuuden: rows with both seasons count for TheMovieDb too
		{4880, 1, 5, 1, 5, true}, {4880, 1, 222, 11, 1, true}, {4880, 1, 221, 10, 25, true}, {4880, 0, 1, 0, 0, false},
		// Urusei Yatsura: an episode listed twice gets its first; the offset covers the rest
		{377, 1, 1, 1, 1, true}, {377, 1, 2, 1, 3, true}, {377, 1, 23, 1, 45, true}, {377, 1, 46, 1, 70, true}, {377, 1, 30, 1, 53, true},
		// Re:Zero 2nd season: TVDB S2, TMDB S1 from episode 27
		{14792, 1, 1, 1, 27, true},
		{13430, 1, 1, 0, 1, true},
		{23, 1, 1, 0, 0, false}, // no TMDB show
	} {
		s, n, ok := list[x.aid].TMDB.Episode(x.as, x.ep)
		if ok != x.ok || (ok && (s != x.s || n != x.n)) {
			t.Errorf("aid %d AniDB %d/%d: got TMDB S%d E%d ok=%v, want S%d E%d ok=%v", x.aid, x.as, x.ep, s, n, ok, x.s, x.n, x.ok)
		}
	}
	// TheTVDB is unchanged by the TMDB fields and rows.
	if s, n, ok := list[4880].TVDB.Episode(1, 222); !ok || s != 10 || n != 26 {
		t.Errorf("4880 TVDB: got S%d E%d ok=%v, want S10 E26", s, n, ok)
	}
	if s, n, ok := list[14792].TVDB.Episode(1, 1); !ok || s != 2 || n != 1 {
		t.Errorf("14792 TVDB: got S%d E%d ok=%v, want S2 E1", s, n, ok)
	}
	if !list[69].TMDB.Mapped || list[23].TMDB.Mapped || !list[4880].TMDB.HasException(0, 1) || list[69].TMDB.HasException(0, 1) {
		t.Error("TMDB.Mapped / TMDB.HasException")
	}

	// The first entry of the TMDB show: season 1 or absolute, no offset.
	l := &Lists{}
	l.SetForTest(list)
	for aid, want := range map[int]bool{69: true, 11370: true, 14792: false, 13430: false, 23: false} {
		if got := l.StartsShow("tmdb", aid); got != want {
			t.Errorf("StartsShow(tmdb, %d) = %v, want %v", aid, got, want)
		}
	}

	// Fribb's TMDB season and offset; without a season, no TMDB placement.
	fr, err := parseFribb([]byte(`[
  {"anidb_id": 14792, "tvdb_id": 305089, "themoviedb_id": {"tv": 65942}, "season": {"tvdb": 2, "tmdb": 1}, "episode_offset": {"tmdb": 26}},
  {"anidb_id": 69, "tvdb_id": 81797, "themoviedb_id": {"tv": 37854}},
  {"anidb_id": 70, "themoviedb_id": {"tv": 99}}
]`))
	if err != nil {
		t.Fatal(err)
	}
	if e := fr[14792]; !e.TMDB.Mapped || e.TMDB.Season != 1 || e.TMDB.Offset != 26 {
		t.Errorf("Fribb 14792: %+v", e)
	}
	if fr[69].TMDB.Mapped {
		t.Error("Fribb 69 (no TMDB season) must stay unmapped")
	}
	// merge carries the TMDB placement with a missing TMDB id, and never
	// mixes it into an entry that has its own.
	dst := map[int]Entry{14792: {AID: 14792}, 69: list[69]}
	merge(dst, fr)
	if e := dst[14792]; e.TMDB.ID != 65942 || !e.TMDB.Mapped || e.TMDB.Season != 1 || e.TMDB.Offset != 26 {
		t.Errorf("merge 14792: %+v", e)
	}
	if e := dst[69]; !e.TMDB.Mapped || e.TMDB.Season != -1 || len(e.TMDB.Ranges)+len(e.TMDB.Episodes) == 0 {
		t.Errorf("merge 69: %+v", e)
	}
}

// Real Anime-Lists entries (trimmed) that use the less common <mapping>
// forms: TheMovieDb mappings, ranges without an end, offsets without a
// range, and credits numbered C1 = 101.
func TestAnimeListsMappingForms(t *testing.T) {
	list, err := parseAnimeLists([]byte(`<anime-list>
<anime anidbid="69" tvdbid="81797" defaulttvdbseason="a" episodeoffset="" tmdbtv="37854" tmdbseason="a" tmdboffset="" tmdbid="" imdbid="">
  <name>One Piece</name>
  <mapping-list>
    <mapping anidbseason="0" tvdbseason="0">;1-27;2-3;6-0;</mapping>
    <mapping anidbseason="1" tmdbseason="1" start="1" end="61" offset="0"/>
    <mapping anidbseason="1" tmdbseason="2" start="62" end="77" offset="-61"/>
    <mapping anidbseason="1" tvdbseason="1" start="1" end="8" offset="0"/>
    <mapping anidbseason="1" tvdbseason="2" start="9" end="30" offset="-8"/>
    <mapping anidbseason="1" tvdbseason="5" start="61" end="69" offset="-60"/>
    <mapping anidbseason="1" tvdbseason="22" start="1086" end="1155" offset="-1085"/>
    <mapping anidbseason="1" tvdbseason="23" start="1156" offset="-1155"/>
  </mapping-list>
</anime>
<anime anidbid="4880" tvdbid="79824" defaulttvdbseason="a" episodeoffset="" tmdbtv="31910" tmdbseason="a">
  <mapping-list>
    <mapping anidbseason="0" tvdbseason="0" tmdbseason="0">;1-0;</mapping>
    <mapping anidbseason="1" tmdbseason="10" start="197" end="221" offset="-196"/>
    <mapping anidbseason="1" tvdbseason="1" tmdbseason="1" start="1" end="32" offset="0"/>
    <mapping anidbseason="1" tvdbseason="10" start="197" end="222" offset="-196"/>
  </mapping-list>
</anime>
<anime anidbid="377" tvdbid="75113" defaulttvdbseason="a" episodeoffset="" tmdbtv="34163" tmdbseason="1">
  <mapping-list>
    <mapping anidbseason="1" tmdbseason="1" offset="23">;1-1;1-2;2-3;2-4;</mapping>
    <mapping anidbseason="1" tvdbseason="1" start="1" end="54"/>
    <mapping anidbseason="1" tvdbseason="2" start="55" end="106" offset="-54"/>
  </mapping-list>
</anime>
<anime anidbid="9304" tvdbid="262954" defaulttvdbseason="1" episodeoffset="">
  <mapping-list><mapping anidbseason="0" tvdbseason="0" offset="7"/></mapping-list>
</anime>
<anime anidbid="3427" tvdbid="79880" defaulttvdbseason="1" episodeoffset="">
  <mapping-list><mapping anidbseason="0" tvdbseason="0" offset="5">;1-30;6-34;7-35;8-31;</mapping></mapping-list>
</anime>
<anime anidbid="14086" tvdbid="347734" defaulttvdbseason="a" episodeoffset="">
  <mapping-list>
    <mapping anidbseason="1" tvdbseason="2" start="7" end="19" offset="-6"/>
    <mapping anidbseason="1" tvdbseason="6" start="49" offset="-48"/>
  </mapping-list>
</anime>
<anime anidbid="1431" tvdbid="79032" defaulttvdbseason="1" episodeoffset="">
  <mapping-list><mapping anidbseason="0" tvdbseason="0">;1-0;101-3;102-4;103-5;201-1;401-2;</mapping></mapping-list>
</anime>
</anime-list>`))
	if err != nil {
		t.Fatal(err)
	}
	type c struct {
		aid, as, ep, s, n int
		ok                bool
	}
	for _, x := range []c{
		// TheMovieDb mappings don't move TVDB episodes; TheTVDB's apply.
		{69, 1, 1, 1, 1, true}, {69, 1, 9, 2, 1, true}, {69, 1, 62, 5, 2, true}, {69, 1, 1155, 22, 70, true},
		{69, 0, 1, 0, 27, true}, {69, 0, 6, 0, 0, false},
		{69, 1, 1156, 23, 1, true}, {69, 1, 1200, 23, 45, true}, // range without end
		{69, 1, 40, -1, 40, true}, // not in any TVDB range: the absolute default
		{4880, 1, 200, 10, 4, true}, {4880, 1, 5, 1, 5, true}, {4880, 0, 1, 0, 0, false},
		{377, 1, 1, 1, 1, true}, {377, 1, 2, 1, 2, true}, {377, 1, 60, 2, 6, true},
		// offset without a range: the whole AniDB season, specials only
		{9304, 0, 1, 0, 8, true}, {9304, 0, 99, 0, 106, true}, {9304, 0, 101, 0, 0, false},
		{3427, 0, 1, 0, 30, true}, {3427, 0, 6, 0, 34, true}, {3427, 0, 2, 0, 7, true}, // pairs win, offset for the rest
		{14086, 1, 10, 2, 4, true}, {14086, 1, 49, 6, 1, true}, {14086, 1, 60, 6, 12, true}, {14086, 1, 3, -1, 3, true},
		// credits C1..C3 = 101..103, trailer T1 = 201, other O1 = 401
		{1431, 0, 102, 0, 4, true}, {1431, 0, 201, 0, 1, true}, {1431, 0, 401, 0, 2, true},
	} {
		s, n, ok := list[x.aid].TVDB.Episode(x.as, x.ep)
		if ok != x.ok || (ok && (s != x.s || n != x.n)) {
			t.Errorf("aid %d AniDB %d/%d: got S%d E%d ok=%v, want S%d E%d ok=%v", x.aid, x.as, x.ep, s, n, ok, x.s, x.n, x.ok)
		}
	}
	if !list[9304].TVDB.HasException(0, 3) || list[9304].TVDB.HasException(1, 3) {
		t.Error("an offset-only mapping lists every episode of its AniDB season, and only those")
	}
	dst := map[int]Entry{9304: {AID: 9304}}
	merge(dst, list)
	if len(dst[9304].TVDB.Ranges) != 1 {
		t.Errorf("merge must carry the open ranges with the TVDB id: %+v", dst[9304])
	}
}

// Lookups read one snapshot: before anything loaded they find nothing, ids
// are matched as numbers (IMDb as text), and they never allocate.
func TestLookups(t *testing.T) {
	l := &Lists{}
	if l.Ready() || l.ByAID(1).AID != 0 || l.AIDFor("mal", "7") != 0 || l.FirstAID(100) != 0 {
		t.Error("lookups before a load must find nothing")
	}
	l.SetForTest(map[int]Entry{
		1: {AID: 1, TVDB: Placement{ID: 100, Season: 1, Mapped: true}, MAL: 7, IMDB: "tt01"},
		2: {AID: 2, TVDB: Placement{ID: 100, Season: 2, Mapped: true}, IMDB: "tt01"},
	})
	cases := []struct {
		source, id string
		want       int
	}{
		{"mal", "7", 1}, {"mal", " 7 ", 1}, {"mal", "x", 0}, {"mal", "0", 0}, {"kitsu", "7", 0},
		{"imdb", "tt01", 1}, {"imdb", " tt01", 1}, {"tvdb", "100", 1}, {"unknown", "7", 0},
	}
	for _, c := range cases {
		if got := l.AIDFor(c.source, c.id); got != c.want {
			t.Errorf("AIDFor(%s, %q) = %d, want %d", c.source, c.id, got, c.want)
		}
	}
	allocs := testing.AllocsPerRun(10, func() {
		l.AIDFor("tvdb", "100")
		l.AIDFor("imdb", "tt01")
		l.StartsSeries(2)
		l.AIDsForTVDB(100)
	})
	if allocs != 0 {
		t.Errorf("lookups allocate %.0f times, want 0", allocs)
	}
	three := &Lists{} // three appends leave room for a fourth
	three.SetForTest(map[int]Entry{1: {TVDB: Placement{ID: 100}}, 2: {TVDB: Placement{ID: 100}}, 3: {TVDB: Placement{ID: 100}}})
	if aids := three.AIDsForTVDB(100); len(aids) != 3 || cap(aids) != len(aids) {
		t.Errorf("AIDsForTVDB(100) = %v (cap %d): want all three, clipped so an append can't write into the mapping", aids, cap(aids))
	}
}

// Both lists give an entry the first IMDb title id ("tt…") they list, also
// when another kind of id comes first.
func TestFirstIMDbTitleID(t *testing.T) {
	al, err := parseAnimeLists([]byte(`<anime-list>
<anime anidbid="1" tvdbid="1" imdbid="nm0000001,tt0000002,tt0000003"/>
<anime anidbid="2" tvdbid="1" imdbid=" tt0000004 "/>
<anime anidbid="3" tvdbid="1" imdbid="nm0000001"/>
<anime anidbid="4" tvdbid="1" imdbid=""/></anime-list>`))
	if err != nil {
		t.Fatal(err)
	}
	fr, err := parseFribb([]byte(`[{"anidb_id": 1, "imdb_id": ["", "nm0000001", "tt0000002", "tt0000003"]},
{"anidb_id": 2, "imdb_id": ["tt0000004"]}, {"anidb_id": 3, "imdb_id": ["nm0000001"]}, {"anidb_id": 4}]`))
	if err != nil {
		t.Fatal(err)
	}
	for aid, want := range map[int]string{1: "tt0000002", 2: "tt0000004", 3: "", 4: ""} {
		if got := al[aid].IMDB; got != want {
			t.Errorf("animelists %d: IMDb %q, want %q", aid, got, want)
		}
		if got := fr[aid].IMDB; got != want {
			t.Errorf("fribb %d: IMDb %q, want %q", aid, got, want)
		}
	}
}
