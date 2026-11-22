package provider

import (
	"context"
	"fmt"
	"log/slog"

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
// (none when the entry is on another TMDB show, or a TMDB exception says the
// episode has no counterpart); without an AniDB counterpart, or when the
// mapping has no TMDB season or exception for it (TheTVDB's spot says nothing
// about TheMovieDb's split), by absolute number (tvdbPairing.otherAt).

// spotName is where an episode is on its list, for the match report: "S01E05",
// or "E26" in absolute order.
func spotName(e sources.Episode) string {
	if e.Season < 0 {
		return fmt.Sprintf("E%02d", e.Number)
	}
	return fmt.Sprintf("S%02dE%02d", e.Season, e.Number)
}

// tvdbSite and tmdbSite mark whose list a spot is on. The two sites split
// seasons differently, so a spot on one is no key on the other.
type tvdbSite struct{}
type tmdbSite struct{}

type site interface{ tvdbSite | tmdbSite }

// spot is a place on a series of site S: season (-1 = absolute order) and
// episode number. The zero-size field makes tvdbSpot and tmdbSpot
// inconvertible: a TheTVDB spot can't be passed or converted where
// TheMovieDb's is wanted.
type spot[S site] struct {
	_      [0]S
	season int
	number int
}

type tvdbSpot = spot[tvdbSite]
type tmdbSpot = spot[tmdbSite]

// tmdbSpotAtSameNumbers is a TheTVDB spot's numbers read on TheMovieDb:
// only for where the mapping says nothing about TheMovieDb's split (an entry
// without a TMDB season, a special that keeps its number in season 0).
func tmdbSpotAtSameNumbers(sp tvdbSpot) tmdbSpot {
	return tmdbSpot{season: sp.season, number: sp.number}
}

// episodeIndex finds an episode of site S by its spot. The maps hold
// positions in eps: whole episodes would cost an allocation each.
type episodeIndex[S site] struct {
	eps    []sources.Episode
	bySpot map[spot[S]]int
	byAbs  map[int]int
	// posters: TheMovieDb's season posters, read with its episode list
	// (nil for TheTVDB's).
	posters map[int][]string
}

func indexEpisodes[S site](eps []sources.Episode) episodeIndex[S] {
	x := episodeIndex[S]{eps: eps, bySpot: make(map[spot[S]]int, len(eps)), byAbs: make(map[int]int, len(eps))}
	for i, e := range eps {
		x.bySpot[spot[S]{season: e.Season, number: e.Number}] = i
		if e.Season > 0 && e.Absolute > 0 {
			x.byAbs[e.Absolute] = i
		}
	}
	return x
}

func (x episodeIndex[S]) at(sp spot[S]) (sources.Episode, bool) {
	var i int
	var ok bool
	if sp.season == -1 {
		i, ok = x.byAbs[sp.number]
	} else {
		i, ok = x.bySpot[sp]
	}
	if !ok {
		return sources.Episode{}, false
	}
	return x.eps[i], true
}

// pairedList is one site's episode list, paired with a show: at gives each
// of the show's episodes its spot on the list.
type pairedList[S site] struct {
	x  episodeIndex[S]
	at func(*Ep) (spot[S], bool)
}

// of looks up ep at the spot at() gives it on this list (the zero
// pairedList: a list not read, which has nothing).
func (l pairedList[S]) of(ep *Ep) (sources.Episode, bool) {
	if l.at == nil {
		return sources.Episode{}, false
	}
	if sp, ok := l.at(ep); ok {
		return l.x.at(sp)
	}
	return sources.Episode{}, false
}

// title looks ep up on this list (of) and, when it's there, gives ep that
// episode's title from src, and (record) where it is, for the match report.
func (l pairedList[S]) title(ep *Ep, src string, record bool) (sources.Episode, bool) {
	e, ok := l.of(ep)
	if ok {
		ep.setTitle(src, e.Title)
		if record {
			ep.setPair(src, spotName(e))
		}
	}
	return e, ok
}

// tvdbEpisodeIndex, tmdbEpisodeIndex and plexEpisodeIndex return a series'
// episodes, tagged with the site whose spots place them (Plex's by the
// TheTVDB id). err is why the list failed or came back partial: the show is
// rebuilt soon (lazy.failed). A series the source doesn't have (404) is
// complete.
func (p *Provider) tvdbEpisodeIndex(ctx context.Context, id int) (episodeIndex[tvdbSite], error) {
	eps, err := p.tvdb.Episodes(ctx, id)
	return indexList[tvdbSite]("tvdb", id, eps, nil, err)
}

func (p *Provider) tmdbEpisodeIndex(ctx context.Context, id int) (episodeIndex[tmdbSite], error) {
	eps, posters, err := p.tmdb.EpisodesAndPosters(ctx, id)
	return indexList[tmdbSite]("tmdb", id, eps, posters, err)
}

func (p *Provider) plexEpisodeIndex(ctx context.Context, tvdb int) (episodeIndex[tvdbSite], error) {
	eps, err := p.plex.Episodes(ctx, sources.PlexSeries(tvdb))
	return indexList[tvdbSite]("plex", tvdb, eps, nil, err)
}

// indexList fills in absolute numbers and indexes eps; empty when err.
func indexList[S site](src string, id int, eps []sources.Episode, posters map[int][]string, err error) (episodeIndex[S], error) {
	if err != nil {
		slog.Debug("episode list not available", "source", src, "id", id, "err", err)
	}
	sources.FillAbsolute(eps)
	x := indexEpisodes[S](eps)
	x.posters = posters
	return x, err
}

// sitePlacement is the placement of an entry on site S's series: where the
// mapping puts it, and whether the entry is the first one of that series.
// The zero-size field is spot's: sitePlacement[tmdbSite](es.tvdb) won't compile.
type sitePlacement[S site] struct {
	_ [0]S
	mapping.Placement
	starts bool
}

// spotOn returns where an AniDB episode (season 1 regular, 0 = S special)
// of the entry sits on the series pl places it on (openings, endings,
// trailers… only through a mapping exception: see mapping.Placement.Episode).
func (pl sitePlacement[S]) spotOn(season, index int) (spot[S], bool) {
	if !pl.HasException(season, index) {
		if season == 0 && (pl.Season == 0 || !pl.starts) {
			// An unlisted special is the special with the same number only
			// on the entry that starts the series. A later entry's S1 would land
			// on the first entry's S00E01, and an entry whose own episodes live
			// in season 0 (OVAs, movies) shares its numbers with them.
			return spot[S]{}, false
		}
		if pl.Season == -1 && pl.Offset == 0 && !pl.starts {
			// "absolute from episode 1" on a later entry of the series: the
			// mapping has no real position for it (a Fribb entry without season
			// info), so it would get the first season's episodes
			return spot[S]{}, false
		}
	}
	s, n, ok := pl.Episode(season, index)
	return spot[S]{season: s, number: n}, ok
}

// entrySpots places one entry's AniDB episodes on TheTVDB and TheMovieDb.
// Whether the entry starts each series is looked up once, not per episode.
type entrySpots struct {
	tvdb sitePlacement[tvdbSite]
	tmdb sitePlacement[tmdbSite]
}

func (p *Provider) spotsOf(e mapping.Entry) entrySpots {
	return entrySpots{
		tvdb: sitePlacement[tvdbSite]{Placement: e.TVDB, starts: p.lists.StartsShow("tvdb", e.AID)},
		tmdb: sitePlacement[tmdbSite]{Placement: e.TMDB, starts: p.lists.StartsShow("tmdb", e.AID)},
	}
}

// tvdbAt returns where an AniDB episode sits on TheTVDB.
func (es entrySpots) tvdbAt(season, index int) (tvdbSpot, bool) {
	return es.tvdb.spotOn(season, index)
}

// tmdbAt returns where the mapping places an AniDB episode on TheMovieDb:
// its TMDB season, offset and exceptions, with TheTVDB's rules for unlisted
// specials and later entries. With neither a TMDB season nor an exception
// (tmdbPlaces) it places nothing.
func (es entrySpots) tmdbAt(season, index int) (tmdbSpot, bool) {
	if !es.tmdbPlaces(season, index) {
		return tmdbSpot{}, false
	}
	return es.tmdb.spotOn(season, index)
}

// tmdbPlaces reports whether the mapping itself places an AniDB episode on
// TheMovieDb (a TMDB season, or a TMDB exception for it), rather than at
// TheTVDB's spot.
func (es entrySpots) tmdbPlaces(season, index int) bool {
	return es.tmdb.Mapped || es.tmdb.HasException(season, index)
}
