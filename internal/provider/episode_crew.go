package provider

import "context"

// episodeCrew sets each episode's directors and writers: TheTVDB's, else
// TheMovieDb's (per field). A TheTVDB v4 list has no crew: it's in one
// record per episode (EpisodeCrew). Returns false while some of those are
// still being fetched.
func (p *Provider) episodeCrew(ctx context.Context, s *Show, tv, tm pairedList) bool {
	var ids []int
	s.eachEpisode(func(ep *Ep) {
		if e, ok := tv.of(ep); ok && e.ID > 0 {
			ids = append(ids, e.ID)
		}
	})
	crew, pending := p.tvdb.EpisodeCrew(ctx, ids)
	s.eachEpisode(func(ep *Ep) {
		te, tvOK := tv.of(ep)
		me, tmOK := tm.of(ep)
		if !tvOK && !tmOK {
			return
		}
		var d, w []string
		if tvOK {
			d, w = te.Directors, te.Writers
			if c, ok := crew[te.ID]; ok {
				d, w = c.Directors, c.Writers
			}
		}
		if tmOK {
			if len(d) == 0 {
				d = me.Directors
			}
			if len(w) == 0 {
				w = me.Writers
			}
		}
		ep.Directors, ep.Writers = d, w
	})
	return !pending
}
