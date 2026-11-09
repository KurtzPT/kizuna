package mapping

import "testing"

// TestSetupDocExamples checks every anime-list-custom.xml example in MAPPING.md
// against the parser. Keep both in sync.
func TestSetupDocExamples(t *testing.T) {
	list, err := parseAnimeLists([]byte(`<anime-list>
<anime anidbid="1" tvdbid="56789" defaulttvdbseason="1" episodeoffset="0" />
<anime anidbid="2" tvdbid="56789" defaulttvdbseason="3" episodeoffset="0" />
<anime anidbid="3" tvdbid="56789" defaulttvdbseason="1" episodeoffset="12" />
<anime anidbid="4" tvdbid="56789" defaulttvdbseason="a" episodeoffset="61" />
<anime anidbid="5" tvdbid="56789" defaulttvdbseason="1" episodeoffset="0"><mapping-list>
  <mapping anidbseason="1" tvdbseason="0">;9-1;18-2;</mapping>
  <mapping anidbseason="1" tvdbseason="1" start="10" end="17" offset="-1"/>
  <mapping anidbseason="1" tvdbseason="1" start="19" end="26" offset="-2"/>
</mapping-list></anime>
<anime anidbid="6" tvdbid="56789" defaulttvdbseason="1" episodeoffset="0"><mapping-list>
  <mapping anidbseason="0" tvdbseason="0">;1-4;2-0;3-5;</mapping>
</mapping-list></anime>
<anime anidbid="7" tvdbid="56789" defaulttvdbseason="0" episodeoffset="6" />
<anime anidbid="8" tvdbid="56789" defaulttvdbseason="1" episodeoffset="0"><mapping-list>
  <mapping anidbseason="1" tvdbseason="2" start="13" end="24" offset="-12"/>
</mapping-list></anime>
<anime anidbid="9" tvdbid="movie" tmdbid="98765" imdbid="tt1234567" />
<anime anidbid="10" tvdbid="56789"><mapping-list><mapping anidbseason="1" tvdbseason="1">;5-5+6;</mapping></mapping-list></anime>
<anime anidbid="11" tvdbid="56789" />
</anime-list>`))
	if err != nil {
		t.Fatal(err)
	}
	type c struct {
		aid, as, ep, s, n int
		ok                bool
	}
	for _, x := range []c{
		{1, 1, 1, 1, 1, true},
		{2, 1, 1, 3, 1, true}, {2, 1, 12, 3, 12, true},
		{3, 1, 1, 1, 13, true}, {3, 1, 12, 1, 24, true},
		{4, 1, 1, -1, 62, true},
		{5, 1, 8, 1, 8, true}, {5, 1, 9, 0, 1, true}, {5, 1, 10, 1, 9, true}, {5, 1, 17, 1, 16, true},
		{5, 1, 18, 0, 2, true}, {5, 1, 19, 1, 17, true}, {5, 1, 26, 1, 24, true},
		{6, 0, 1, 0, 4, true}, {6, 0, 2, 0, 0, false}, {6, 0, 3, 0, 5, true},
		{7, 1, 1, 0, 7, true}, {7, 1, 2, 0, 8, true},
		{8, 1, 12, 1, 12, true}, {8, 1, 13, 2, 1, true}, {8, 1, 24, 2, 12, true},
		{9, 1, 1, 0, 0, false},
		{10, 1, 5, 1, 5, true},
		{11, 1, 3, 0, 3, true}, // defaulttvdbseason left out: counts as 0 (specials), as the doc warns
	} {
		s, n, ok := list[x.aid].TVDB.Episode(x.as, x.ep)
		if ok != x.ok || (ok && (s != x.s || n != x.n)) {
			t.Errorf("aid %d AniDB %d/%d: got S%d E%d ok=%v, want S%d E%d ok=%v", x.aid, x.as, x.ep, s, n, ok, x.s, x.n, x.ok)
		}
	}
	if e := list[9]; e.TVDB.ID != 0 || !e.noTVDB || e.TMDBMovie != 98765 || e.IMDB != "tt1234567" {
		t.Errorf("movie: %+v", e)
	}
	t.Logf("example 5 stores %d exception episodes", len(list[5].TVDB.Episodes))
}
