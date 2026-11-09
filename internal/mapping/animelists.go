package mapping

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"strings"
	"time"

	"kizuna/internal/fetch"
)

// animeListsURL: Anime-Lists, the XML list (see the package doc).
const animeListsURL = "https://raw.githubusercontent.com/Anime-Lists/anime-lists/master/anime-list-master.xml"

// getAnimeLists downloads the list; parse is the full check a download must
// also pass before it's cached (see fetch.Checked).
func getAnimeLists(ctx context.Context, f *fetch.Client, parse func([]byte) error) ([]byte, error) {
	return f.Get(ctx, fetch.Request{URL: animeListsURL, Key: "mapping/anime-list-master.xml", TTL: 24 * time.Hour, Revalidate: true,
		Validate: func(b []byte) error {
			if !bytes.Contains(b[:min(len(b), 500)], []byte("<anime-list")) {
				return errors.New("not an anime-list")
			}
			return parse(b)
		}})
}

func parseAnimeLists(data []byte) (map[int]Entry, error) {
	var doc struct {
		Anime []struct {
			AID    string `xml:"anidbid,attr"`
			TVDB   string `xml:"tvdbid,attr"`
			Season string `xml:"defaulttvdbseason,attr"`
			Offset string `xml:"episodeoffset,attr"`
			TMDBTV string `xml:"tmdbtv,attr"`
			TMDBSn string `xml:"tmdbseason,attr"`
			TMDBOf string `xml:"tmdboffset,attr"`
			TMDB   string `xml:"tmdbid,attr"`
			IMDB   string `xml:"imdbid,attr"`
			Maps   []struct {
				AniDBSeason string `xml:"anidbseason,attr"`
				TVDBSeason  string `xml:"tvdbseason,attr"`
				TMDBSeason  string `xml:"tmdbseason,attr"`
				Start       string `xml:"start,attr"`
				End         string `xml:"end,attr"`
				Offset      string `xml:"offset,attr"`
				Pairs       string `xml:",chardata"`
			} `xml:"mapping-list>mapping"`
		} `xml:"anime"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	out := make(map[int]Entry, len(doc.Anime))
	for _, a := range doc.Anime {
		aid := atoi(a.AID)
		if aid == 0 {
			continue
		}
		tvdb := strings.TrimSpace(a.TVDB)
		e := Entry{AID: aid,
			TVDB: Placement{ID: atoi(tvdb), Season: listSeason(a.Season), Offset: atoi(a.Offset), Mapped: true},
			TMDB: Placement{ID: atoi(a.TMDBTV), Season: listSeason(a.TMDBSn), Offset: atoi(a.TMDBOf),
				Mapped: strings.TrimSpace(a.TMDBSn) != ""},
			TMDBMovie: atoi(first(a.TMDB)), IMDB: first(a.IMDB)}
		e.noTVDB = e.TVDB.ID == 0 && tvdb != "" && !strings.EqualFold(tvdb, "unknown")
		if !strings.HasPrefix(e.IMDB, "tt") {
			e.IMDB = ""
		}
		// A <mapping> is for TheTVDB (tvdbseason), TheMovieDb (tmdbseason) or both.
		for _, m := range a.Maps {
			if strings.TrimSpace(m.TVDBSeason) != "" {
				e.TVDB.addMapping(atoi(m.AniDBSeason), listSeason(m.TVDBSeason), m.Pairs, m.Start, m.End, m.Offset)
			}
			if strings.TrimSpace(m.TMDBSeason) != "" {
				e.TMDB.addMapping(atoi(m.AniDBSeason), listSeason(m.TMDBSeason), m.Pairs, m.Start, m.End, m.Offset)
			}
		}
		out[aid] = e
	}
	return out, nil
}

// listSeason reads a TVDB or TMDB season attribute ("a" = absolute: -1).
func listSeason(s string) int {
	if strings.TrimSpace(s) == "a" {
		return -1
	}
	return atoi(s)
}

// addMapping reads one <mapping> into a TheTVDB or TheMovieDb placement
// (season is that site's): explicit ";1-3;2-4+5;3-0;" pairs (AniDB episode -
// episode there; "+" = several episodes there, the first is used; 0 = none),
// and/or a start..end range shifted by offset. Explicit pairs come first, so
// they win over a range that covers the same episode.
// A range without end runs on (every later episode); an offset without start
// and end covers the whole AniDB season. Such open ranges go to Ranges.
// An open range of specials stays inside its hundred: specials 1–99, credits
// 101–199, trailers 201–299… are separate kinds of episodes.
func (pl *Placement) addMapping(anidbSeason, season int, pairs, start, end, offset string) {
	listed := map[int]bool{}
	var explicit []EpisodeMap
	for _, p := range strings.Split(pairs, ";") {
		a, t, ok := strings.Cut(strings.TrimSpace(p), "-")
		if !ok {
			continue
		}
		t, _, _ = strings.Cut(t, "+")
		if n := atoi(a); n > 0 {
			explicit = append(explicit, EpisodeMap{anidbSeason, n, season, atoi(t)})
			listed[n] = true
		}
	}
	// Kept in order: an AniDB episode listed twice (";1-1;1-2;", two
	// episodes on the other site) gets the first, as with "+".
	pl.Episodes = append(explicit, pl.Episodes...)
	from, to, shift := atoi(start), atoi(end), atoi(offset)
	switch {
	case strings.TrimSpace(start) == "" && strings.TrimSpace(end) == "":
		if strings.TrimSpace(offset) == "" {
			return // only pairs
		}
		from = 1
		fallthrough
	case strings.TrimSpace(end) == "":
		if from <= 0 {
			return
		}
		to = 1 << 30
		if anidbSeason == 0 {
			to = from/100*100 + 99
		}
		// Explicit pairs win: they come first in Episodes, which is checked first.
		pl.Ranges = append(pl.Ranges, EpisodeRange{anidbSeason, from, to, season, shift})
	case from > 0 && to >= from && to-from < 5000:
		for n := from; n <= to; n++ {
			if !listed[n] {
				pl.Episodes = append(pl.Episodes, EpisodeMap{anidbSeason, n, season, n + shift})
			}
		}
	}
}
