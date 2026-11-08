package mapping

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"kizuna/internal/fetch"
)

// fribbURL: Fribb/anime-lists, the JSON list (see the package doc).
const fribbURL = "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-mini.json"

func getFribb(ctx context.Context, f *fetch.Client) ([]byte, error) {
	return f.Get(ctx, fetch.Request{URL: fribbURL, Key: "mapping/fribb-anime-list-mini.json", TTL: 24 * time.Hour, Revalidate: true,
		Validate: func(b []byte) error {
			if len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '[' {
				return errors.New("not a JSON list")
			}
			return nil
		}})
}

func parseFribb(data []byte) (map[int]Entry, error) {
	var list []struct {
		AID     int      `json:"anidb_id"`
		TVDB    int      `json:"tvdb_id"`
		IMDB    []string `json:"imdb_id"`
		MAL     int      `json:"mal_id"`
		AniList int      `json:"anilist_id"`
		Kitsu   int      `json:"kitsu_id"`
		ANN     int      `json:"animenewsnetwork_id"`
		TMDB    struct {
			TV    int   `json:"tv"`
			Movie []int `json:"movie"`
		} `json:"themoviedb_id"`
		Season *struct {
			TVDB *int `json:"tvdb"`
			TMDB *int `json:"tmdb"`
		} `json:"season"`
		Offset struct {
			TVDB int `json:"tvdb"`
			TMDB int `json:"tmdb"`
		} `json:"episode_offset"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	out := make(map[int]Entry, len(list))
	for _, a := range list {
		if a.AID == 0 { // anime without an AniDB id are of no use here
			continue
		}
		e := Entry{AID: a.AID,
			TVDB: Placement{ID: a.TVDB, Offset: a.Offset.TVDB, Mapped: true},
			TMDB: Placement{ID: a.TMDB.TV, Offset: a.Offset.TMDB},
			MAL:  a.MAL, AniList: a.AniList, Kitsu: a.Kitsu, ANN: a.ANN}
		// Fribb drops Anime-Lists' absolute marker ("a"): a TVDB id without a
		// season is an absolute-order entry.
		e.TVDB.Season = -1
		if a.Season != nil && a.Season.TVDB != nil {
			e.TVDB.Season = *a.Season.TVDB
		}
		// A TMDB id without a season stays unmapped rather than absolute: it
		// is Anime-Lists' "a", but also every TMDB id Fribb looked up itself.
		if a.Season != nil && a.Season.TMDB != nil {
			e.TMDB.Mapped, e.TMDB.Season = true, *a.Season.TMDB
		}
		if len(a.TMDB.Movie) > 0 {
			e.TMDBMovie = a.TMDB.Movie[0]
		}
		if len(a.IMDB) > 0 && strings.HasPrefix(a.IMDB[0], "tt") {
			e.IMDB = a.IMDB[0]
		}
		out[a.AID] = e
	}
	return out, nil
}
