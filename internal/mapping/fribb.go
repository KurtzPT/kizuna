package mapping

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"kizuna/internal/fetch"
)

// fribbURL: Fribb/anime-lists, the JSON list (see the package doc).
const fribbURL = "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-mini.json"

// getFribb downloads the list; parse is the full check a download must also
// pass before it's cached (see fetch.Checked).
func getFribb(ctx context.Context, f *fetch.Client, parse func([]byte) error) ([]byte, error) {
	return f.Get(ctx, fetch.Request{URL: fribbURL, Key: "mapping/fribb-anime-list-mini.json", TTL: 24 * time.Hour, Revalidate: true,
		Validate: func(b []byte) error {
			if len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '[' {
				return errors.New("not a JSON list")
			}
			return parse(b)
		}})
}

// fribbRow is one anime of the Fribb list.
type fribbRow struct {
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

// fribbBadRows: a list with more than one row in this many that doesn't
// read is refused, so the cached copy stays. A few bad rows are a typo
// upstream; many mean the format changed, and using what is left would
// quietly lose most of the ids.
const fribbBadRows = 100

// parseFribb reads the list row by row. A row with a field of the wrong type
// is skipped, not the whole list. Broken JSON (a cut-off download) still
// fails it whole.
func parseFribb(data []byte) (map[int]Entry, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('[') {
		return nil, errors.New("not a JSON list")
	}
	out := map[int]Entry{}
	bad, total := 0, 0
	var firstBad error
	for dec.More() {
		total++
		var a fribbRow
		if err := dec.Decode(&a); err != nil {
			// Decode reads the whole row before it fills r, so after a type
			// error the next row starts cleanly. Any other error is broken
			// JSON.
			var te *json.UnmarshalTypeError
			if !errors.As(err, &te) {
				return nil, err
			}
			bad++
			if firstBad == nil {
				firstBad = err
			}
			continue
		}
		if a.AID != 0 { // anime without an AniDB id are of no use here
			out[a.AID] = fribbEntry(a)
		}
	}
	if _, err := dec.Token(); err != nil { // the closing ]
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("text after the list")
	}
	if bad*fribbBadRows > total {
		return nil, fmt.Errorf("%d of %d rows don't read (more than 1 in %d: has the list's format changed?): %w", bad, total, fribbBadRows, firstBad)
	}
	if bad > 0 {
		slog.Warn("mapping: skipped Fribb rows that don't read; the rest of the list is used", "list", "fribb", "rows", bad, "of", total, "first", firstBad.Error())
	}
	return out, nil
}

// fribbEntry is a row as a mapping entry.
func fribbEntry(a fribbRow) Entry {
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
	e.IMDB = imdbID(a.IMDB...)
	return e
}
