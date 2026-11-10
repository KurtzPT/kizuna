package mapping

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

// parseAnimeLists parses the downloaded list; parseAnimeListsFile is the
// same for any file in its format (the custom file), named in the warning.
func parseAnimeLists(data []byte) (map[int]Entry, error) {
	return parseAnimeListsFile(data, "anime-list-master.xml")
}

// pairConflict: an AniDB episode that two <mapping>s of an entry pair with
// two different episodes of a site.
type pairConflict struct {
	aid           int
	site          string
	first, second EpisodeMap
}

func (c pairConflict) String() string {
	return fmt.Sprintf("anidb-%d S%dE%d: %s S%dE%d and S%dE%d", c.aid, c.first.AniDBSeason, c.first.AniDBEp,
		c.site, c.first.Season, c.first.Ep, c.second.Season, c.second.Ep)
}

func parseAnimeListsFile(data []byte, file string) (map[int]Entry, error) {
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
	var conflicts []pairConflict
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
			TMDBMovie: atoi(first(a.TMDB)), IMDB: imdbID(strings.Split(a.IMDB, ",")...)}
		e.noTVDB = e.TVDB.ID == 0 && tvdb != "" && !strings.EqualFold(tvdb, "unknown")
		// A <mapping> is for TheTVDB (tvdbseason), TheMovieDb (tmdbseason) or both.
		var tvdbPairs, tmdbPairs int // explicit pairs so far, at the start of Episodes
		for _, m := range a.Maps {
			var clash []EpisodeMap
			if strings.TrimSpace(m.TVDBSeason) != "" {
				tvdbPairs, clash = e.TVDB.addMapping(tvdbPairs, atoi(m.AniDBSeason), listSeason(m.TVDBSeason), m.Pairs, m.Start, m.End, m.Offset)
				for i := 0; i < len(clash); i += 2 {
					conflicts = append(conflicts, pairConflict{aid, "TVDB", clash[i], clash[i+1]})
				}
			}
			if strings.TrimSpace(m.TMDBSeason) != "" {
				tmdbPairs, clash = e.TMDB.addMapping(tmdbPairs, atoi(m.AniDBSeason), listSeason(m.TMDBSeason), m.Pairs, m.Start, m.End, m.Offset)
				for i := 0; i < len(clash); i += 2 {
					conflicts = append(conflicts, pairConflict{aid, "TMDB", clash[i], clash[i+1]})
				}
			}
		}
		out[aid] = e
	}
	if len(conflicts) > 0 {
		// One line per parse, not per episode: the list is parsed again
		// whenever it changes.
		shown := make([]string, 0, 5)
		for _, c := range conflicts[:min(len(conflicts), cap(shown))] {
			shown = append(shown, c.String())
		}
		slog.Warn("mapping: an AniDB episode is paired with two different episodes in two <mapping>s of its entry; "+
			"the first is used. Fix the entry (your own in anime-list-custom.xml; the list's by reporting it to Anime-Lists, "+
			"or with a corrected entry in anime-list-custom.xml)", "file", file, "episodes", len(conflicts), "first", shown)
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
// they win over a range that covers the same episode, also one of an earlier
// <mapping>. pairs is how many explicit pairs earlier <mapping>s put at the
// start of Episodes; it returns the count with this one's added.
// An AniDB episode an earlier <mapping> paired too keeps one pair: an
// episode beats "none" in either order (";1-0;" for the specials and
// ";1-26;" for season 1: it is S01E26, the list's way to say "not a special
// there"); two different episodes are a mistake in the list: the first
// stays, and both are returned in clash (first, second, ...) for a warning.
// A range without end runs on (every later episode); an offset without start
// and end covers the whole AniDB season. Such open ranges go to Ranges.
// An open range of specials stays inside its hundred: specials 1–99, credits
// 101–199, trailers 201–299… are separate kinds of episodes.
func (pl *Placement) addMapping(pairs, anidbSeason, season int, list, start, end, offset string) (count int, clash []EpisodeMap) {
	var explicit []EpisodeMap
	var own []int // every AniDB episode this mapping pairs, kept in explicit or not
	for _, p := range strings.Split(list, ";") {
		a, t, ok := strings.Cut(strings.TrimSpace(p), "-")
		if !ok {
			continue
		}
		t, _, _ = strings.Cut(t, "+")
		n := atoi(a)
		if n <= 0 {
			continue
		}
		m := EpisodeMap{anidbSeason, n, season, atoi(t)}
		own = append(own, n)
		earlier := slices.IndexFunc(pl.Episodes[:pairs], func(o EpisodeMap) bool { return o.AniDBSeason == anidbSeason && o.AniDBEp == n })
		if earlier < 0 {
			explicit = append(explicit, m)
			continue
		}
		switch o := pl.Episodes[earlier]; {
		case m.Ep <= 0 || o == m: // "none" here, or the same pair again: the earlier one stands
		case o.Ep <= 0:
			pl.Episodes[earlier] = m // "none" there, an episode here
		default:
			clash = append(clash, o, m)
		}
	}
	// Kept in order, after the earlier <mapping>s' pairs: an AniDB episode
	// listed twice in one <mapping> (";1-1;1-2;", two episodes on the other
	// site) gets the first, as with "+".
	pl.Episodes = slices.Insert(pl.Episodes, pairs, explicit...)
	from, to, shift := atoi(start), atoi(end), atoi(offset)
	switch {
	case strings.TrimSpace(start) == "" && strings.TrimSpace(end) == "":
		if strings.TrimSpace(offset) == "" {
			return pairs + len(explicit), clash // only pairs
		}
		from = 1
		fallthrough
	case strings.TrimSpace(end) == "":
		if from <= 0 {
			break
		}
		to = 1 << 30
		if anidbSeason == 0 {
			to = from/100*100 + 99
		}
		// Explicit pairs win: they come first in Episodes, which is checked first.
		pl.Ranges = append(pl.Ranges, EpisodeRange{anidbSeason, from, to, season, shift})
	case from > 0 && to >= from && to-from < 5000 && to < 1<<30: // the bound keeps n++ from wrapping
		listed := make(map[int]bool, len(own)) // this mapping's pairs: no range entry behind them
		for _, n := range own {
			listed[n] = true
		}
		for n := from; n <= to; n++ {
			if !listed[n] {
				pl.Episodes = append(pl.Episodes, EpisodeMap{anidbSeason, n, season, n + shift})
			}
		}
	}
	return pairs + len(explicit), clash
}
