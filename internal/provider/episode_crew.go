package provider

import (
	"context"
	"slices"

	"kizuna/internal/sources"
)

// Episode directors, writers and producers follow EPISODE_CREW_SOURCES
// alone, in both modes: per episode and role, the first listed source that
// has it. The episodes arrive with what TheTVDB's list has (Ep.Directors,
// Ep.Writers: only a legacy v3 list has crew) and the TheTVDB episode they
// are paired with (Ep.tvdbEp), set where they were paired. The sources are
// walked in order, once. TheMovieDb's list is at hand; Plex's list is read
// for every series Plex can be asked for (plexCrewRead), with the other
// lists; a TheTVDB v4 list has no crew: it's in one record per episode
// (TVDB.EpisodeCrew), one request each, asked only for the episodes the
// sources before tvdb left a gap in (tvdbCrew; with tvdb first, every one).
//
// Each person sent gets a photo (crewPhotos, finishCrew): that of the first
// source in EPISODE_CREW_SOURCES with one, from any of its episodes (Plex's
// first, with CREW_PHOTOS_FROM_PLEX). The photo sent always comes from the
// memory (crewMemory), which finishCrew tells what the build found: Plex
// keeps one photo per person for the whole server, and a name sent without
// one takes it away.

// plexCrewRead: Plex's episode list is read for episode crew, without a gap
// test: Plex can be asked for the series (by its TheTVDB id), and plex is in
// EPISODE_CREW_SOURCES or CREW_PHOTOS_FROM_PLEX is on (with a crew list:
// none means no crew at all). Read for the setting alone, it gives photos
// only.
func (p *Provider) plexCrewRead(tvdb int) bool {
	order := p.cfg.EpisodeCrewSources
	return tvdb > 0 && p.plex.Enabled() && len(order) > 0 && (p.cfg.CrewPhotosFromPlex || slices.Contains(order, "plex"))
}

// episodeCrew sets an /anidb show's episode crew from TheTVDB, TheMovieDb's
// list (tm) and Plex's (px, read here for crew when its read for titles or
// fill values didn't happen), and adds their photos to cp. Returns false
// while TheTVDB's records are still being fetched.
func (p *Provider) episodeCrew(ctx context.Context, s *Show, tm pairedList, px *plexFill, cp *crewPhotos) bool {
	if len(p.cfg.EpisodeCrewSources) == 0 {
		applyCrew(s, nil, nil, tm, px.list) // takes TheTVDB's list's crew off: no crew at all
		return true
	}
	if px.get != nil && px.crew {
		px.fetch()
	}
	crew, pending := p.walkCrew(cp, tm, px.list, func(prior []string) (map[int]sources.Crew, bool) {
		return p.tvdbCrew(ctx, s, prior, cp, tm, px.list)
	})
	applyCrew(s, p.cfg.EpisodeCrewSources, crew, tm, px.list)
	return !pending
}

// walkCrew walks EPISODE_CREW_SOURCES once, adding each source's photos to
// cp as it reaches it (Plex's first, with CREW_PHOTOS_FROM_PLEX: they count
// in TheTVDB's gap test then). records gives TheTVDB's crew records, once
// the photos of the sources before tvdb (prior) are in cp; pending: some are
// still on their way.
func (p *Provider) walkCrew(cp *crewPhotos, tm, px pairedList,
	records func(prior []string) (map[int]sources.Crew, bool)) (crew map[int]sources.Crew, pending bool) {
	order := p.cfg.EpisodeCrewSources
	if p.cfg.CrewPhotosFromPlex {
		cp.addList(srcPlex, px)
	}
	for i, src := range order {
		switch src {
		case "tvdb":
			crew, pending = records(order[:i])
			for _, c := range crew {
				cp.add(srcTVDB, c.Photos)
			}
		case "tmdb":
			cp.addList(srcTMDB, tm)
		case "plex":
			if !p.cfg.CrewPhotosFromPlex {
				cp.addList(srcPlex, px)
			}
		}
	}
	return crew, pending
}

// tvdbCrew asks TheTVDB for the crew records of the episodes paired with
// one (Ep.tvdbEp) that still have a gap after the sources in prior (those
// listed before tvdb; tm and px their lists, cp their photos): lacksCrew.
// A record is a request per episode: none is asked for an episode those
// filled. With tvdb first, every paired episode is asked for. pending: some
// are still on their way.
func (p *Provider) tvdbCrew(ctx context.Context, s *Show, prior []string, cp *crewPhotos, tm, px pairedList) (crew map[int]sources.Crew, pending bool) {
	var ids []int
	plexPhotos := p.cfg.CrewPhotosFromPlex
	s.eachEpisode(func(ep *Ep) {
		if ep.tvdbEp > 0 && lacksCrew(ep, prior, cp, plexPhotos, tm, px) {
			ids = append(ids, ep.tvdbEp)
		}
	})
	return p.tvdb.EpisodeCrew(ctx, ids) // no ids: no request
}

// lacksCrew: after the sources in order (tm and px as for applyCrew), ep
// still lacks directors, writers or producers, or names a person none of
// those sources has a photo of (cp; Plex's too with plexPhotos). The memory
// doesn't count: it never stops a source being asked.
func lacksCrew(ep *Ep, order []string, cp *crewPhotos, plexPhotos bool, tm, px pairedList) bool {
	c := pickCrew(ep, order, nil, tm, px)
	if len(c.directors) == 0 || len(c.writers) == 0 || len(c.producers) == 0 {
		return true
	}
	for _, names := range [...][]string{c.directors, c.writers, c.producers} {
		for _, n := range names {
			if ph, _ := cp.photo(cp.key(n), order, plexPhotos); ph == "" {
				return true
			}
		}
	}
	return false
}

// applyCrew sets every episode's directors, writers and producers in order
// (EPISODE_CREW_SOURCES): crew holds TheTVDB's v4 records by TheTVDB
// episode, tm and px are TheMovieDb's and Plex's lists (empty when not
// read). TheTVDB's list's crew is taken off the episodes as TheTVDB's
// value, so it is dropped when tvdb isn't listed. Called once per build,
// last: a second call would take what the first one set for TheTVDB's.
func applyCrew(s *Show, order []string, crew map[int]sources.Crew, tm, px pairedList) {
	s.eachEpisode(func(ep *Ep) {
		c := pickCrew(ep, order, crew, tm, px)
		ep.Directors, ep.Writers, ep.Producers = c.directors, c.writers, c.producers
		ep.src.directors, ep.src.writers, ep.src.producers = c.dSrc, c.wSrc, c.pSrc
	})
}

// crewPick is an episode's directors, writers and producers, each from the
// first source that has it, and the source of each.
type crewPick struct {
	directors, writers, producers []string
	dSrc, wSrc, pSrc              srcCode
}

// take fills the roles still empty from one source's (code).
func (c *crewPick) take(d, w, pr []string, code srcCode) {
	if len(c.directors) == 0 && len(d) > 0 {
		c.directors, c.dSrc = d, code
	}
	if len(c.writers) == 0 && len(w) > 0 {
		c.writers, c.wSrc = w, code
	}
	if len(c.producers) == 0 && len(pr) > 0 {
		c.producers, c.pSrc = pr, code
	}
}

// pickCrew picks ep's directors, writers and producers in order. TheTVDB's
// are its record in crew, else what its list gave (still on ep: pickCrew
// reads it, applyCrew replaces it); TheMovieDb's and Plex's are their lists'
// episodes at ep's spots.
func pickCrew(ep *Ep, order []string, crew map[int]sources.Crew, tm, px pairedList) crewPick {
	var c crewPick
	for _, o := range order {
		if len(c.directors) > 0 && len(c.writers) > 0 && len(c.producers) > 0 {
			break // the next source's isn't needed
		}
		switch o {
		case "tvdb":
			if r, ok := crew[ep.tvdbEp]; ok && ep.tvdbEp > 0 {
				c.take(r.Directors, r.Writers, r.Producers, srcTVDB)
			} else {
				c.take(ep.Directors, ep.Writers, ep.Producers, srcTVDB)
			}
		case "tmdb", "plex":
			l, code := tm, srcTMDB
			if o == "plex" {
				l, code = px, srcPlex
			}
			if e, ok := l.of(ep); ok {
				c.take(e.Directors, e.Writers, e.Producers, code)
			}
		}
	}
	return c
}

// crewPhotos is what one build's sources say about crew photos: per source,
// each person's photo by nameKey, from its episode answers (and, in
// finishCrew, the show answers the build read anyway). A plain value: with
// no crew it stays empty and costs nothing.
type crewPhotos struct {
	by   [srcPlex + 1]map[string]string // by source code: key -> photo; "" = two different photos
	keys map[string]string              // name -> nameKey, worked out once per build
}

// key is name's nameKey.
func (cp *crewPhotos) key(name string) string {
	if k, ok := cp.keys[name]; ok {
		return k
	}
	if cp.keys == nil {
		cp.keys = map[string]string{}
	}
	k := nameKey(name)
	cp.keys[name] = k
	return k
}

// lastKey is name's nameKey for its last use in the build (finishCrew,
// which meets each name once): from the cache when the name was keyed
// before, else worked out without keeping it.
func (cp *crewPhotos) lastKey(name string) string {
	if k, ok := cp.keys[name]; ok {
		return k
	}
	return nameKey(name)
}

// add adds people's photos as src's.
func (cp *crewPhotos) add(src srcCode, people []sources.Person) {
	if len(people) == 0 {
		return
	}
	if cp.by[src] == nil {
		cp.by[src] = map[string]string{}
	}
	addPhotos(cp.by[src], people, cp.key)
}

// addList adds the photos of every episode on a list as src's: a person
// with a photo on one episode has it on all.
func (cp *crewPhotos) addList(src srcCode, l pairedList) {
	for _, e := range l.x.eps {
		cp.add(src, e.Photos)
	}
}

// photo is key's photo and its source: Plex's first with plexFirst
// (CREW_PHOTOS_FROM_PLEX), then each source's of order in turn.
func (cp *crewPhotos) photo(key string, order []string, plexFirst bool) (string, srcCode) {
	if key == "" {
		return "", srcNone
	}
	if plexFirst {
		if ph := cp.by[srcPlex][key]; ph != "" {
			return ph, srcPlex
		}
	}
	for _, o := range order {
		if c := crewSource(o); c != srcNone {
			if ph := cp.by[c][key]; ph != "" {
				return ph, c
			}
		}
	}
	return "", srcNone
}

// crewSource is an EPISODE_CREW_SOURCES name's code.
func crewSource(src string) srcCode {
	switch src {
	case "tvdb":
		return srcTVDB
	case "tmdb":
		return srcTMDB
	case "plex":
		return srcPlex
	}
	return srcNone
}

// crewRef is a crew name of a built show: its nameKey, by which the memory
// gives its photo at answer time, and the source of the photo this build
// found (srcNone: none; the match report counts it).
type crewRef struct {
	key string
	src srcCode
}

// finishCrew runs once the show's fields and its episodes are both in. It
// adds the crew photos of the show answers the build read anyway (ss:
// TheTVDB's and TheMovieDb's details, Plex's show), each in its source's
// place when that source is in the walk, then gives each distinct name on
// the episodes its key and photo source (s.crew), and tells the memory every
// photo found. Nothing is fetched here.
func (p *Provider) finishCrew(s *Show, ss *showSources, cp *crewPhotos) {
	order := p.cfg.EpisodeCrewSources
	named := false
	s.eachEpisode(func(ep *Ep) { named = named || len(ep.Directors)+len(ep.Writers)+len(ep.Producers) > 0 })
	if !named || len(order) == 0 {
		return
	}
	plexFirst := p.cfg.CrewPhotosFromPlex
	for _, src := range order {
		switch src {
		case "tvdb":
			if d, ok := ss.tvdbDetails.done(); ok {
				cp.add(srcTVDB, d.CrewPhotos)
			}
		case "tmdb":
			if d, ok := ss.tmdbDetails.done(); ok {
				cp.add(srcTMDB, d.CrewPhotos)
			}
		case "plex":
			if !plexFirst {
				if sh, ok := ss.plexShow.done(); ok {
					cp.add(srcPlex, sh.Details.CrewPhotos)
				}
			}
		}
	}
	if plexFirst {
		if sh, ok := ss.plexShow.done(); ok {
			cp.add(srcPlex, sh.Details.CrewPhotos)
		}
	}
	s.crew = map[string]crewRef{}
	changed := false
	s.eachEpisode(func(ep *Ep) {
		for _, names := range [...][]string{ep.Directors, ep.Writers, ep.Producers} {
			for _, n := range names {
				if _, ok := s.crew[n]; ok {
					continue
				}
				k := cp.lastKey(n)
				ph, src := cp.photo(k, order, plexFirst)
				s.crew[n] = crewRef{k, src}
				if ph != "" && p.crewMemory.learn(k, ph) {
					changed = true
				}
			}
		}
	})
	if changed {
		p.crewMemory.save()
	}
}
