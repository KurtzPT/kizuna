package provider

import "context"

// episodeCrew sets each episode's directors and writers: TheTVDB's, else
// TheMovieDb's (per field). The episodes already carry what TheTVDB's list
// has (Ep.Directors, Ep.Writers) and the TheTVDB episode they are paired
// with (Ep.tvdbEp), set where they were paired, so TheTVDB's list isn't
// looked up again here. A TheTVDB v4 list has no crew: it's in one record
// per episode (EpisodeCrew). Returns false while some of those are still
// being fetched.
func (p *Provider) episodeCrew(ctx context.Context, s *Show, tm pairedList) bool {
	var ids []int
	s.eachEpisode(func(ep *Ep) {
		if ep.tvdbEp > 0 {
			ids = append(ids, ep.tvdbEp)
		}
	})
	crew, pending := p.tvdb.EpisodeCrew(ctx, ids)
	s.eachEpisode(func(ep *Ep) {
		if c, ok := crew[ep.tvdbEp]; ok && ep.tvdbEp > 0 {
			ep.Directors, ep.Writers = c.Directors, c.Writers
		}
		if len(ep.Directors) > 0 && len(ep.Writers) > 0 {
			return // TheMovieDb's isn't needed
		}
		if me, ok := tm.of(ep); ok {
			if len(ep.Directors) == 0 {
				ep.Directors = me.Directors
			}
			if len(ep.Writers) == 0 {
				ep.Writers = me.Writers
			}
		}
	})
	return !pending
}
