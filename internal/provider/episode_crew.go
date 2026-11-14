package provider

import (
	"context"
	"slices"

	"kizuna/internal/sources"
)

// Episode directors and writers follow EPISODE_CREW_SOURCES alone, in both
// modes: per episode and field, the first listed source that has it. The
// episodes arrive with what TheTVDB's list has (Ep.Directors, Ep.Writers:
// only a legacy v3 list has crew) and the TheTVDB episode they are paired
// with (Ep.tvdbEp), set where they were paired. The sources are walked in
// order, once: each is asked only for what the ones before it left empty.
// TheMovieDb's list is at hand; a TheTVDB v4 list has no crew: it's in one
// record per episode (TVDB.EpisodeCrew), one request each, asked only for
// the episodes with a gap (tvdbCrew; with tvdb first, every one); Plex's
// list is read only when it can fill a gap (plexCrewNeeded).

// episodeCrew sets an /anidb show's episode directors and writers from
// TheTVDB, TheMovieDb's list (tm) and Plex's (px, read here when crew
// needs it and its read for titles or fill values didn't happen). Returns
// false while TheTVDB's records are still being fetched.
func (p *Provider) episodeCrew(ctx context.Context, s *Show, tm pairedList, px *plexFill) bool {
	order := p.cfg.EpisodeCrewSources
	var crew map[int]sources.Crew
	pending := false
	for i, src := range order {
		switch src {
		case "tvdb":
			crew, pending = p.tvdbCrew(ctx, s, order[:i], tm, px.list)
		case "plex":
			px.readCrew(p, s, crew, tm, pending)
		}
	}
	applyCrew(s, order, crew, tm, px.list)
	return !pending
}

// tvdbCrew asks TheTVDB for the crew records of the episodes paired with
// one (Ep.tvdbEp) that still lack a director or a writer after the sources
// in prior (those listed before tvdb; tm and px their lists). A record is
// a request per episode: none is asked for an episode those filled. With
// tvdb first, every paired episode is asked for. pending: some are still on
// their way.
func (p *Provider) tvdbCrew(ctx context.Context, s *Show, prior []string, tm, px pairedList) (crew map[int]sources.Crew, pending bool) {
	var ids []int
	s.eachEpisode(func(ep *Ep) {
		if ep.tvdbEp > 0 && lacksCrew(ep, prior, nil, tm, px) {
			ids = append(ids, ep.tvdbEp)
		}
	})
	return p.tvdb.EpisodeCrew(ctx, ids) // no ids: no request
}

// lacksCrew: ep still lacks a director or a writer after the sources in order
// (crew, tm and px as for applyCrew).
func lacksCrew(ep *Ep, order []string, crew map[int]sources.Crew, tm, px pairedList) bool {
	d, w, _, _ := pickCrew(ep, order, crew, tm, px)
	return len(d) == 0 || len(w) == 0
}

// plexCrewNeeded: Plex's episode list is worth reading for episode crew:
// plex is in EPISODE_CREW_SOURCES, and an episode Plex has a place for
// (onList) still lacks a director or a writer after the sources listed
// before plex (so always, with plex first). Not while TheTVDB's records are
// on their way (pending) and tvdb comes before plex: a gap can't be told
// from a record still to come, and the rebuild soon decides. crew and tm as
// for applyCrew.
func (p *Provider) plexCrewNeeded(s *Show, crew map[int]sources.Crew, tm pairedList, pending bool, onList func(*Ep) bool) bool {
	order := p.cfg.EpisodeCrewSources
	i := slices.Index(order, "plex")
	if i < 0 || pending && before(order, "tvdb", "plex") {
		return false
	}
	gap := false
	s.eachEpisode(func(ep *Ep) {
		if gap || !onList(ep) {
			return
		}
		gap = lacksCrew(ep, order[:i], crew, tm, pairedList{})
	})
	return gap
}

// applyCrew sets every episode's directors and writers in order
// (EPISODE_CREW_SOURCES): crew holds TheTVDB's v4 records by TheTVDB
// episode, tm and px are TheMovieDb's and Plex's lists (empty when not
// read). TheTVDB's list's crew is taken off the episodes as TheTVDB's
// value, so it is dropped when tvdb isn't listed. Called once per build,
// last: a second call would take what the first one set for TheTVDB's.
func applyCrew(s *Show, order []string, crew map[int]sources.Crew, tm, px pairedList) {
	s.eachEpisode(func(ep *Ep) {
		ep.Directors, ep.Writers, ep.src.directors, ep.src.writers = pickCrew(ep, order, crew, tm, px)
	})
}

// pickCrew gives ep's directors and writers, each from the first source in
// order that has it, and their sources. TheTVDB's is its record in crew,
// else what its list gave (still on ep: pickCrew reads it, applyCrew
// replaces it); TheMovieDb's and Plex's are their lists' episodes at ep's
// spots.
func pickCrew(ep *Ep, order []string, crew map[int]sources.Crew, tm, px pairedList) (d, w []string, dSrc, wSrc srcCode) {
	for _, o := range order {
		if len(d) > 0 && len(w) > 0 {
			break // the next source's isn't needed
		}
		var cd, cw []string
		var code srcCode
		switch o {
		case "tvdb":
			cd, cw, code = ep.Directors, ep.Writers, srcTVDB
			if c, ok := crew[ep.tvdbEp]; ok && ep.tvdbEp > 0 {
				cd, cw = c.Directors, c.Writers
			}
		case "tmdb", "plex":
			l, c := tm, srcTMDB
			if o == "plex" {
				l, c = px, srcPlex
			}
			e, ok := l.of(ep)
			if !ok {
				continue
			}
			cd, cw, code = e.Directors, e.Writers, c
		}
		if len(d) == 0 && len(cd) > 0 {
			d, dSrc = cd, code
		}
		if len(w) == 0 && len(cw) > 0 {
			w, wSrc = cw, code
		}
	}
	return d, w, dSrc, wSrc
}
