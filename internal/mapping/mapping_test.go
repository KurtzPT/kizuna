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
		{1, 5, 1, 5, true},   // default: season 1, no offset
		{1, 9, 0, 1, true},   // recap -> TVDB special 1
		{1, 12, 1, 11, true}, // range, offset -1
		{1, 20, 1, 18, true}, // range, offset -2
		{0, 1, 0, 4, true},   // AniDB special 1 -> TVDB S00E04
		{0, 2, 0, 0, false},  // "2-0": no TVDB episode
		{0, 3, 0, 0, false},  // unlisted special
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
