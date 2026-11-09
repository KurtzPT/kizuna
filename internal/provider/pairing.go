package provider

import (
	"context"
	"fmt"
	"log/slog"

	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/sources"
)

// Episode titles and episode pairing, for both modes.
//
// Every episode collects one title per source it can be paired with (Ep.titles),
// and EPISODE_TITLES picks the first that is a real title. Pairing goes through
// the mapping: Anime-Lists' per-episode exceptions first, else the entry's TVDB
// season and episode offset. In /anidb, TheMovieDb is paired the same way by its
// own season, offset and exceptions (tmdbseason…), since its seasons often
// differ from TheTVDB's; an entry without them gets TheTVDB's season and
// number. In /tvdb, TheMovieDb is paired through AniDB: the place on TheTVDB's
// list gives the AniDB episode, and the mapping gives its place on TheMovieDb
// (none when the entry is on another TMDB show); without an AniDB
// counterpart, by absolute number (tvdbPairing.otherAt).

// spotName is where an episode is on its list, for the match report: "S01E05",
// or "E26" in absolute order.
func spotName(e sources.Episode) string {
	if e.Season < 0 {
		return fmt.Sprintf("E%02d", e.Number)
	}
	return fmt.Sprintf("S%02dE%02d", e.Season, e.Number)
}

// spot is a place on a TVDB or TMDB series: season (-1 = absolute order) and
// episode number.
type spot struct{ season, number int }

// episodeIndex finds a TVDB/TMDB episode by its spot.
type episodeIndex struct {
	bySpot map[spot]sources.Episode
	byAbs  map[int]sources.Episode
}

func indexEpisodes(eps []sources.Episode) episodeIndex {
	x := episodeIndex{map[spot]sources.Episode{}, map[int]sources.Episode{}}
	for _, e := range eps {
		x.bySpot[spot{e.Season, e.Number}] = e
		if e.Season > 0 && e.Absolute > 0 {
			x.byAbs[e.Absolute] = e
		}
	}
	return x
}

func (x episodeIndex) at(sp spot) (sources.Episode, bool) {
	if sp.season == -1 {
		e, ok := x.byAbs[sp.number]
		return e, ok
	}
	e, ok := x.bySpot[sp]
	return e, ok
}

// pairedList is one site's episode list, paired with a show: at gives each
// of the show's episodes its spot on the list.
type pairedList struct {
	x  episodeIndex
	at func(*Ep) (spot, bool)
}

// of looks up ep at the spot at() gives it on this list.
func (l pairedList) of(ep *Ep) (sources.Episode, bool) {
	if sp, ok := l.at(ep); ok {
		return l.x.at(sp)
	}
	return sources.Episode{}, false
}

// seriesEpisodes returns a TVDB or TMDB series' episodes, absolute numbers
// filled in (an empty index when the list can't be had). complete is false
// when the list failed or came back partial, so the show is rebuilt soon; a
// series the source doesn't have (404) counts as complete.
func (p *Provider) seriesEpisodes(ctx context.Context, src string, id int) (x episodeIndex, complete bool) {
	var eps []sources.Episode
	var err error
	switch src {
	case "tvdb":
		eps, err = p.tvdb.Episodes(ctx, id)
	case "tmdb":
		eps, err = p.tmdb.Episodes(ctx, id)
	}
	if err != nil {
		slog.Debug("episode list not available", "source", src, "id", id, "err", err)
	}
	sources.FillAbsolute(eps)
	return indexEpisodes(eps), err == nil || fetch.IsNotFound(err)
}

// spotOn returns where an AniDB episode (season 1 regular, 0 = S special)
// of an entry sits on the series pl places it on (openings, endings,
// trailers… only through a mapping exception: see mapping.Placement.Episode).
// starts: the entry is the first one of that series.
func spotOn(pl mapping.Placement, starts bool, season, index int) (spot, bool) {
	if !pl.HasException(season, index) {
		if season == 0 && (pl.Season == 0 || !starts) {
			// An unlisted special is the special with the same number only
			// on the entry that starts the series. A later entry's S1 would land
			// on the first entry's S00E01, and an entry whose own episodes live
			// in season 0 (OVAs, movies) shares its numbers with them.
			return spot{}, false
		}
		if pl.Season == -1 && pl.Offset == 0 && !starts {
			// "absolute from episode 1" on a later entry of the series: the
			// mapping has no real position for it (a Fribb entry without season
			// info), so it would get the first season's episodes
			return spot{}, false
		}
	}
	s, n, ok := pl.Episode(season, index)
	return spot{s, n}, ok
}

// entrySpots places one entry's AniDB episodes on TheTVDB and TheMovieDb.
// Whether the entry starts each series is looked up once, not per episode.
type entrySpots struct {
	tvdb, tmdb             mapping.Placement
	startsTVDB, startsTMDB bool
}

func (p *Provider) spotsOf(e mapping.Entry) entrySpots {
	return entrySpots{e.TVDB, e.TMDB, p.lists.StartsShow("tvdb", e.AID), p.lists.StartsShow("tmdb", e.AID)}
}

// at returns where an AniDB episode sits on a site ("tvdb" or "tmdb"). On
// TheMovieDb the mapping's TMDB season, offset and exceptions place it, with
// TheTVDB's rules for unlisted specials and later entries; an entry without
// a TMDB season gets TheTVDB's spot, except for the episodes a TMDB
// exception lists.
func (es entrySpots) at(site string, season, index int) (spot, bool) {
	if site == "tmdb" && (es.tmdb.Mapped || es.tmdb.HasException(season, index)) {
		return spotOn(es.tmdb, es.startsTMDB, season, index)
	}
	return spotOn(es.tvdb, es.startsTVDB, season, index)
}
