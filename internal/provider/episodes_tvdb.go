package provider

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"kizuna/internal/mapping"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// tvdbPairing is what a /tvdb show's AniDB entries tell about each place on
// its episode list (TheTVDB's): AniDB's titles, and the place of the same
// AniDB episode on the other site, TheMovieDb, which often splits seasons
// differently (One Piece: TVDB S2E1 is AniDB episode 9, TMDB S2E1 is
// episode 62).
type tvdbPairing struct {
	titles  map[spot]anidbTitles
	other   map[spot]otherPlace
	loading []int // AniDB entries not cached yet, fetched in the background
}

// otherPlace is an episode's place on TheMovieDb. on is false when the
// mapping puts its AniDB entry on another show there, or a TMDB exception
// says the episode has no counterpart: it pairs with nothing.
type otherPlace struct {
	at spot
	on bool
}

type anidbTitles struct {
	title, any string
	aid        int    // the AniDB entry and episode it is, for the match report
	epNo       string // (formatted only when recording: see tvdbEpisodeTitles)
}

// atListed looks up an episode by its place on the list, else (regular
// episodes) by its absolute number, for entries mapped in absolute order.
func atListed[V any](m map[spot]V, ep *Ep) (V, bool) {
	v, ok := m[ep.listed]
	if !ok && ep.Season > 0 {
		v, ok = m[spot{-1, ep.Index}]
	}
	return v, ok
}

// otherAt: an episode's place on TheMovieDb, through AniDB. An episode
// whose AniDB entry is on another show there has none, nor has one a TMDB
// exception says has no counterpart. Without an AniDB counterpart (not
// cached yet, or a TheTVDB-only extra), or one the mapping doesn't place on
// TheMovieDb (no TMDB season or exception), a regular episode
// goes by its absolute number, which doesn't depend on how either site
// splits seasons (an absolute number the other site doesn't have pairs with
// nothing). A special has no absolute number: it keeps its number in season
// 0, as unlisted specials do everywhere else.
func (pr tvdbPairing) otherAt(ep *Ep) (spot, bool) {
	if o, ok := atListed(pr.other, ep); ok {
		return o.at, o.on
	}
	if ep.Season > 0 {
		return spot{-1, ep.Index}, true
	}
	return ep.listed, true
}

// tvdbBuild is what the steps of a /tvdb build share.
type tvdbBuild struct {
	tvdbID, tmdbID int                 // tmdbID 0 = no TheMovieDb show
	aid            int                 // the series' first AniDB entry (0 = none), looked up once per build
	first          *sources.AniDBAnime // its details (nil = not available)
	eps            []sources.Episode   // TheTVDB's episode list, absolute numbers filled in
	// tmdbEps: TheMovieDb's episode list, for the titles, the crew and the
	// season posters; fetched from the build's start (startTMDB; empty when
	// the show isn't there, or no setting asks the list for anything:
	// tmdbOn, with tmdbListed).
	tmdbEps    lazy[episodeIndex]
	tmdbListed bool
	// crew: TheTVDB's crew records of every episode, fetched from start when
	// tvdb is first in EPISODE_CREW_SOURCES (no f otherwise: they are asked
	// at crew time, for the episodes with a gap: tvdbCrew).
	crew lazy[crewRecords]
	// partial: TheTVDB's list came back with parts missing.
	partial bool
	// plex: Plex's episode list, at each episode's own TheTVDB spot, read
	// for the titles (tvdbPlexTitles) or the crew (episodeCrew); fetched from
	// start when read for crew.
	plex plexEpisodes
	// started: what startTMDB and start fetch ahead. The build waits for it
	// before it returns.
	started group
	// photos: the crew photos of the build's sources (episode_crew.go).
	photos crewPhotos
}

func newTVDBBuild(tvdbID, tmdbID, aid int, first *sources.AniDBAnime, eps []sources.Episode) *tvdbBuild {
	return &tvdbBuild{tvdbID: tvdbID, tmdbID: tmdbID, aid: aid, first: first, eps: eps}
}

// tmdbOn reports whether TheMovieDb's episode list is fetched: only for a
// show found there, and when a setting asks it for something
// (tmdbEpisodesListed: tmdbListed).
func (b *tvdbBuild) tmdbOn() bool {
	return b.tmdbID > 0 && b.tmdbListed
}

// crewRecords is TheTVDB.EpisodeCrew's answer: the records by TheTVDB
// episode, and pending: some are still on their way, or their request
// failed.
type crewRecords struct {
	crew    map[int]sources.Crew
	pending bool
}

// startTMDB looks up TheMovieDb's show (tmdbShow: tmdbShowOf's) and
// fetches its episode list (loadTMDB), as soon as the build starts: it
// needs only the show's id, not TheTVDB's list, and both can take a while
// on a long show.
func (b *tvdbBuild) startTMDB(ctx context.Context, p *Provider, tmdbShow func() (int, error)) {
	b.tmdbListed = p.tmdbEpisodesListed(true)
	b.tmdbEps.from(1, true, func() (episodeIndex, error) {
		if id, _ := tmdbShow(); id > 0 && b.tmdbListed { // tmdbOn, once the build has set tmdbID from the same lookup
			return p.seriesEpisodes(ctx, "tmdb", id)
		}
		return episodeIndex{}, nil
	})
	b.tmdbEps.start(&b.started)
}

// start starts what the build needs whatever AniDB answers, once TheTVDB's
// list is in: Plex's list, when it is read for episode crew (plexCrewRead),
// and TheTVDB's crew records for every episode, when tvdb is first in
// EPISODE_CREW_SOURCES. Otherwise the records are asked at crew time, only
// for the episodes the sources before tvdb left a gap in (tvdbCrew).
func (b *tvdbBuild) start(ctx context.Context, p *Provider) {
	if p.plexEpisodesListed(b.tvdbID, false) { // the closure only when used: it allocates
		b.plex.at = func(ep *Ep) (spot, bool) { return ep.listed, true } // each at its own TheTVDB spot
		b.plex.x.from(b.tvdbID, true, func() (episodeIndex, error) { return p.seriesEpisodes(ctx, "plex", b.tvdbID) })
		if b.plex.crew = p.plexCrewRead(b.tvdbID); b.plex.crew {
			b.plex.x.start(&b.started)
		}
	}
	if order := p.cfg.EpisodeCrewSources; len(order) == 0 || order[0] != "tvdb" {
		return
	}
	ids := make([]int, 0, len(b.eps))
	for _, e := range b.eps {
		if e.ID > 0 {
			ids = append(ids, e.ID)
		}
	}
	b.crew.from(1, true, func() (crewRecords, error) {
		crew, pending := p.tvdb.EpisodeCrew(ctx, ids)
		return crewRecords{crew, pending}, nil
	})
	b.crew.start(&b.started)
}

// loadTMDB waits for TheMovieDb's episode list (startTMDB fetches it).
// Returns false when it failed or came back partial.
func (b *tvdbBuild) loadTMDB() bool {
	b.tmdbEps.get()
	return !b.tmdbEps.failed()
}

// onTMDBShow reports whether an entry has a place on the show on TheMovieDb.
// Every entry walked is on the TVDB series; an entry of another TMDB show
// has no place on this one.
func (b *tvdbBuild) onTMDBShow(e mapping.Entry) bool {
	return e.TMDB.ID == 0 || e.TMDB.ID == b.tmdbID
}

// tvdbPairing walks every AniDB entry of a /tvdb series. b.first (b.aid's)
// is already fetched: the first entry isn't looked up again, so a mapping
// reload during the build can't pair with another entry's details. Other entries come from the cache; missing ones are fetched in the
// background for the next build, and ready is false meanwhile (not for an
// entry in its 24 h block after an AniDB error: no build before then gets
// it). record notes those missing entries in pr.loading, for the match report.
func (p *Provider) tvdbPairing(b *tvdbBuild, record bool) (pr tvdbPairing, ready bool) {
	pr = tvdbPairing{titles: map[spot]anidbTitles{}, other: map[spot]otherPlace{}}
	ready = true
	for _, aid := range p.lists.AIDsForTVDB(b.tvdbID) {
		a := b.first
		if aid != b.aid {
			var fresh bool
			if a, fresh = p.anidb.Peek(aid); !fresh && p.anidb.Enabled() {
				p.anidb.Prefetch(aid)
				if _, blocked := p.anidb.BlockedUntil(aid); a == nil && !blocked {
					ready = false
					if record {
						pr.loading = append(pr.loading, aid)
					}
				}
			}
		}
		if a == nil {
			continue
		}
		e := p.lists.ByAID(aid)
		es, otherOK := p.spotsOf(e), b.onTMDBShow(e)
		for _, ae := range a.Episodes {
			sp, ok := es.at("tvdb", ae.Season, ae.ListIndex())
			if !ok {
				continue
			}
			if _, taken := pr.titles[sp]; !taken { // first entry wins
				title, anyTitle := p.anidbEpisodeTitle(ae.Titles)
				pr.titles[sp] = anidbTitles{title, anyTitle, aid, ae.EpNo}
			}
			if _, taken := pr.other[sp]; taken {
				continue
			}
			if !otherOK { // on another show there: no pairing, not the absolute-number fallback
				pr.other[sp] = otherPlace{}
				continue
			}
			if !es.tmdbPlaces(ae.Season, ae.ListIndex()) {
				// No TMDB season or exception (a TMDB id only Fribb has): TheTVDB's
				// spot says nothing about TheMovieDb's split, so otherAt pairs it
				// by absolute number, as before the entry was cached.
				continue
			}
			if o, ok := es.at("tmdb", ae.Season, ae.ListIndex()); ok {
				pr.other[sp] = otherPlace{o, true}
			} else if es.tmdb.HasException(ae.Season, ae.ListIndex()) { // the mapping says it has none there
				pr.other[sp] = otherPlace{}
			}
		}
	}
	return pr, ready
}

// tvdbEpisodeTitles adds every title EPISODE_TITLES may pick to a /tvdb show:
// TheMovieDb's (TheTVDB's, from the episode list, is already set), paired
// through AniDB (tvdbPairing), and AniDB's, through every AniDB entry of the
// series. TheMovieDb's list is b.tmdbEps (loadTMDB). Returns the pairing for
// the crew (episodeCrew), and false while some AniDB entries are still
// missing.
func (p *Provider) tvdbEpisodeTitles(ctx context.Context, s *Show, b *tvdbBuild) (tvdbPairing, bool) {
	order := p.cfg.EpisodeTitles
	tmdbOn := b.tmdbOn()
	rec := report.RecorderFrom(ctx)
	var pr tvdbPairing
	ready := true
	if tmdbOn || slices.Contains(order, "anidb") { // TheMovieDb's list needs the pairing: its crew, its season posters
		pr, ready = p.tvdbPairing(b, rec != nil)
		if len(pr.loading) > 0 { // only when recording
			ids := make([]string, len(pr.loading))
			for i, aid := range pr.loading {
				ids[i] = strconv.Itoa(aid)
			}
			rec.Check(report.Check{Level: report.LevelWarn, Cause: true,
				Title: plural(len(pr.loading), "AniDB entry was", "AniDB entries were") + " still loading",
				Text: "AniDB " + joinShort(ids, 8) + " were not cached yet and are being fetched in the background (AniDB allows few requests), " +
					"so their episodes got titles from other sources for now. The next build uses them.", Section: report.SectionEpisodes})
		}
	}
	if tmdbOn && slices.Contains(order, "tmdb") {
		tm := pairedList{b.tmdbEps.get(), pr.otherAt}
		s.eachEpisode(func(ep *Ep) { tm.title(ep, "tmdb", rec != nil) })
	}
	if slices.Contains(order, "anidb") {
		s.eachEpisode(func(ep *Ep) {
			if f, ok := atListed(pr.titles, ep); ok {
				ep.setTitle("anidb", f.title)
				ep.anyTitle = f.any
				if rec != nil {
					ep.setPair("anidb", fmt.Sprintf("%d ep %s", f.aid, f.epNo))
				}
			}
		})
	}
	return pr, ready
}

// tvdbPlexTitles adds Plex's episode titles to a /tvdb show, when its list
// can add something (plexListNeeded: in /tvdb only a title counts; the
// episodes' other values come only from TheTVDB). It reuses the list start
// read for crew. Plex's episodes are in TheTVDB's aired order: each lands at
// its own TheTVDB spot (Ep.listed), not TheMovieDb's. They set no TheTVDB
// episode (Ep.tvdbEp). Returns false when the list failed or came back
// partial.
func (p *Provider) tvdbPlexTitles(ctx context.Context, s *Show, b *tvdbBuild) bool {
	if !b.plex.x.fetches() || !p.plexListNeeded(s, !b.partial, false, b.plex.on) {
		return true
	}
	ok := b.plex.read()
	record := report.RecorderFrom(ctx) != nil
	s.eachEpisode(func(ep *Ep) { b.plex.list.title(ep, "plex", record) })
	return ok
}
