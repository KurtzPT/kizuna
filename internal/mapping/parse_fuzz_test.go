package mapping

import (
	"strings"
	"testing"
)

// FuzzParseAnimeLists: any Anime-Lists XML parses without a panic, and the
// entries keep the shape the provider relies on.
func FuzzParseAnimeLists(f *testing.F) {
	f.Add([]byte(`<anime-list>
<anime anidbid="23" tvdbid="76885" defaulttvdbseason="1" episodeoffset="" tmdbtv="30991" tmdbseason="1" imdbid="tt0213338,tt1">
<mapping-list><mapping anidbseason="0" tvdbseason="0">;1-5;2-6+7;3-0;</mapping>
<mapping anidbseason="1" tvdbseason="2" start="13" end="26" offset="-12"/>
<mapping anidbseason="0" tmdbseason="0" start="101" offset="10"/></mapping-list>
</anime></anime-list>`))
	f.Add([]byte(`<anime-list><anime anidbid="1" tvdbid="unknown" defaulttvdbseason="a"><mapping-list><mapping anidbseason="1" tvdbseason="a" offset="3"/></mapping-list></anime></anime-list>`))
	f.Add([]byte(`<anime-list><anime anidbid="2" tvdbid="movie" tmdbid="12,13"/></anime-list>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		list, err := parseAnimeLists(data)
		if err != nil {
			return
		}
		for aid, e := range list {
			checkEntry(t, aid, e)
			for _, pl := range []Placement{e.TVDB, e.TMDB} {
				for _, r := range pl.Ranges {
					if r.From <= 0 {
						t.Errorf("aid %d: bad range %+v", aid, r)
					}
				}
				for _, m := range pl.Episodes {
					if m.AniDBEp <= 0 {
						t.Errorf("aid %d: episode map for AniDB episode %d", aid, m.AniDBEp)
					}
				}
			}
		}
	})
}

// FuzzParseFribb: any Fribb JSON parses without a panic.
func FuzzParseFribb(f *testing.F) {
	f.Add([]byte(`[{"anidb_id":23,"tvdb_id":76885,"imdb_id":["tt0213338"],"mal_id":1,"themoviedb_id":{"tv":30991,"movie":[1,2]},"season":{"tvdb":1,"tmdb":1},"episode_offset":{"tvdb":0,"tmdb":0}},{"mal_id":5}]`))
	f.Add([]byte(`[{"anidb_id":7,"imdb_id":["nm1","tt2"],"season":{}}]`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		list, err := parseFribb(data)
		if err != nil {
			return
		}
		for aid, e := range list {
			checkEntry(t, aid, e)
		}
	})
}

func checkEntry(t *testing.T, aid int, e Entry) {
	t.Helper()
	if aid == 0 || e.AID != aid {
		t.Errorf("entry %+v under key %d", e, aid)
	}
	if e.IMDB != "" && !strings.HasPrefix(e.IMDB, "tt") {
		t.Errorf("aid %d: IMDb id %q", aid, e.IMDB)
	}
}
