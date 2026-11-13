package provider

import (
	"context"

	"kizuna/internal/sources"
)

// episodeCrew sets each episode's directors and writers: TheTVDB's, else
// TheMovieDb's, else Plex's (per field; Plex's only when its list was read
// for the build: px is empty otherwise). The episodes already carry what TheTVDB's list
// has (Ep.Directors, Ep.Writers) and the TheTVDB episode they are paired
// with (Ep.tvdbEp), set where they were paired, so TheTVDB's list isn't
// looked up again here. A TheTVDB v4 list has no crew: it's in one record
// per episode (EpisodeCrew). Returns false while some of those are still
// being fetched.
func (p *Provider) episodeCrew(ctx context.Context, s *Show, tm, px pairedList) bool {
	var ids []int
	s.eachEpisode(func(ep *Ep) {
		if ep.tvdbEp > 0 {
			ids = append(ids, ep.tvdbEp)
		}
	})
	crew, pending := p.tvdb.EpisodeCrew(ctx, ids)
	applyCrew(s, crew, tm, px)
	return !pending
}

// applyCrew sets the episodes' TheTVDB crew records (crew, by TheTVDB
// episode), then TheMovieDb's, then Plex's for what is still missing.
func applyCrew(s *Show, crew map[int]sources.Crew, tm, px pairedList) {
	s.eachEpisode(func(ep *Ep) {
		if c, ok := crew[ep.tvdbEp]; ok && ep.tvdbEp > 0 {
			ep.Directors, ep.Writers = c.Directors, c.Writers
			ep.src.directors, ep.src.writers = srcTVDB, srcTVDB
		}
		for _, l := range [...]struct {
			list pairedList
			code srcCode
		}{{tm, srcTMDB}, {px, srcPlex}} {
			if len(ep.Directors) > 0 && len(ep.Writers) > 0 {
				return // the next source's isn't needed
			}
			if e, ok := l.list.of(ep); ok {
				if len(ep.Directors) == 0 {
					ep.Directors, ep.src.directors = e.Directors, l.code
				}
				if len(ep.Writers) == 0 {
					ep.Writers, ep.src.writers = e.Writers, l.code
				}
			}
		}
	})
}
