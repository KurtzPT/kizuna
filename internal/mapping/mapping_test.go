package mapping

import "testing"

func TestStartsSeries(t *testing.T) {
	l := &Lists{}
	l.set(map[int]Entry{
		1: {AID: 1, TVDB: 100, TVDBSeason: 1},
		2: {AID: 2, TVDB: 100, TVDBSeason: 2},
		3: {AID: 3, TMDBTV: 500},
		4: {AID: 4, TMDBTV: 600},
		5: {AID: 5, TMDBTV: 600},
	})
	for aid, want := range map[int]bool{1: true, 2: false, 3: true, 4: false, 9: false} {
		if got := l.StartsSeries(aid); got != want {
			t.Errorf("aid %d: got %v, want %v", aid, got, want)
		}
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
	if op.TVDB != 81797 || op.TVDBSeason != -1 || op.MAL != 21 || op.IMDB != "tt0388629" || op.TMDBTV != 37854 {
		t.Errorf("69: first list must win, fallback fills gaps: %+v", op)
	}
	if m := entries[7]; m.TVDB != 0 || m.TMDBMovie != 128 || m.MAL != 164 {
		t.Errorf("7: explicit tvdbid=movie must not get a TVDB id: %+v", m)
	}
	if u := entries[8]; u.TVDB != 77777 || u.TVDBSeason != 2 || u.TVDBOffset != 12 {
		t.Errorf("8: unknown TVDB id should be filled with its season/offset: %+v", u)
	}
	if n := entries[9]; n.TVDBSeason != -1 || n.AniList != 5 {
		t.Errorf("9: Fribb entry without season is absolute: %+v", n)
	}

	l := &Lists{}
	l.set(entries)
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
		s, n, ok := e.TVDBEpisode(c.season, c.ep)
		if ok != c.ok || (ok && (s != c.wantSeason || n != c.wantEp)) {
			t.Errorf("AniDB %d/%d: got S%d E%d ok=%v, want S%d E%d ok=%v", c.season, c.ep, s, n, ok, c.wantSeason, c.wantEp, c.ok)
		}
	}
	if s, n, ok := list[11].TVDBEpisode(1, 1); !ok || s != -1 || n != 27 {
		t.Errorf("absolute entry: got S%d E%d ok=%v, want absolute 27", s, n, ok)
	}
	// a later list fills a missing TVDB id together with its exceptions
	dst := map[int]Entry{10: {AID: 10}}
	merge(dst, list)
	if len(dst[10].Episodes) == 0 || dst[10].TVDB != 500 {
		t.Errorf("merge: %+v", dst[10])
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
		s, n, ok := list[x.aid].TVDBEpisode(x.as, x.ep)
		if ok != x.ok || (ok && (s != x.s || n != x.n)) {
			t.Errorf("aid %d AniDB %d/%d: got S%d E%d ok=%v, want S%d E%d ok=%v", x.aid, x.as, x.ep, s, n, ok, x.s, x.n, x.ok)
		}
	}
	if !list[9304].HasException(0, 3) || list[9304].HasException(1, 3) {
		t.Error("an offset-only mapping lists every episode of its AniDB season, and only those")
	}
	dst := map[int]Entry{9304: {AID: 9304}}
	merge(dst, list)
	if len(dst[9304].Ranges) != 1 {
		t.Errorf("merge must carry the open ranges with the TVDB id: %+v", dst[9304])
	}
}
