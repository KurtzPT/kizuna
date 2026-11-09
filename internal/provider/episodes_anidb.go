package provider

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"kizuna/internal/mapping"
	"kizuna/internal/report"
)

// episodeLists: an /anidb show's TheTVDB and TheMovieDb episode lists, each
// fetched once, on first use (empty, without a fetch, for a site without
// the show or not set up).
type episodeLists struct {
	tvdb, tmdb func() (episodeIndex, bool)
}

func (p *Provider) episodeLists(ctx context.Context, entry mapping.Entry) episodeLists {
	list := func(src string, id int, on bool) func() (episodeIndex, bool) {
		return sync.OnceValues(func() (episodeIndex, bool) {
			if !on || id <= 0 {
				return episodeIndex{}, true
			}
			return p.seriesEpisodes(ctx, src, id)
		})
	}
	return episodeLists{list("tvdb", entry.TVDB.ID, p.tvdb.Enabled()), list("tmdb", entry.TMDB.ID, p.tmdb.Enabled())}
}

// start fetches both lists, in wg.
func (l episodeLists) start(wg *sync.WaitGroup) {
	wg.Go(func() { l.tvdb() })
	wg.Go(func() { l.tmdb() })
}

// listsEarly: an /anidb show's episode lists are worth fetching before
// AniDB answers, which can take seconds (its fair-use gap): AniDB may be
// asked, and the lists will likely be paired with its episodes (any entry
// but a movie known by its TMDB movie id alone; a special-only one is
// paired too, as TheTVDB's special).
func (p *Provider) listsEarly(aid int, entry mapping.Entry) bool {
	if !p.anidb.Enabled() {
		return false // the answer is at hand: no details
	}
	if _, blocked := p.anidb.BlockedUntil(aid); blocked {
		return false
	}
	return entry.SpecialOnly() || entry.TMDB.ID > 0 || entry.TMDBMovie == 0
}

// fillEpisodes pairs an /anidb show's episodes with its TVDB (and TMDB)
// series (lists). TheTVDB gives the thumbnail, which AniDB never has, plus the
// summary, air date and runtime where AniDB has none (EPISODE_FILL_SOURCES).
// Both give a title for EPISODE_TITLES, and directors and writers (AniDB has
// none per episode). Returns false when a list it needed failed or came back
// partial, or episode crew is still being fetched.
func (p *Provider) fillEpisodes(ctx context.Context, s *Show, entry mapping.Entry, lists episodeLists) bool {
	fill := slices.Contains(p.cfg.EpisodeFillSources, "tvdb")
	wantTVDB := p.tvdb.Enabled() && entry.TVDB.ID > 0
	wantTMDB := p.tmdb.Enabled() && entry.TMDB.ID > 0
	if !wantTVDB && !wantTMDB {
		return true
	}
	var tv, tm episodeIndex
	tvOK, tmOK := true, true
	var wg sync.WaitGroup
	if wantTVDB {
		wg.Go(func() { tv, tvOK = lists.tvdb() })
	}
	if wantTMDB {
		wg.Go(func() { tm, tmOK = lists.tmdb() })
	}
	wg.Wait()

	es := p.spotsOf(entry)
	on := func(site string, x episodeIndex) pairedList {
		return pairedList{x, func(ep *Ep) (spot, bool) {
			if ep.Season != 0 && ep.Season != 1 {
				return spot{}, false
			}
			return es.at(site, ep.Season, ep.mapIndex())
		}}
	}
	tvList, tmList := on("tvdb", tv), on("tmdb", tm)
	record := report.RecorderFrom(ctx) != nil // pairs are for the match report only
	paired, pairedTMDB := 0, 0
	for _, season := range []int{1, 0} {
		for i := range s.Episodes[season] {
			ep := &s.Episodes[season][i]
			if _, ok := tmList.title(ep, "tmdb", record); ok {
				pairedTMDB++
			}
			if e, ok := tvList.title(ep, "tvdb", record); ok {
				paired++
				ep.Directors, ep.Writers, ep.tvdbEp = e.Directors, e.Writers, e.ID // episodeCrew completes them
				if fill {
					ep.Thumb = firstOf(ep.Thumb, e.Thumb)
					ep.Summary = firstOf(ep.Summary, e.Summary)
					ep.Date = firstOf(ep.Date, e.Aired)
					if ep.Minutes == 0 {
						ep.Minutes = e.Minutes
					}
				}
			}
		}
	}
	if debugOn(ctx) {
		slog.Debug("episode pairing", "root", s.Root, "tvdb", entry.TVDB.ID, "season", entry.TVDB.Season, "offset", entry.TVDB.Offset,
			"exceptions", len(entry.TVDB.Episodes), "episodes", len(s.Episodes[1])+len(s.Episodes[0]), "paired_tvdb", paired,
			"tmdb", entry.TMDB.ID, "tmdb_mapped", entry.TMDB.Mapped, "paired_tmdb", pairedTMDB)
	}
	crewOK := p.episodeCrew(ctx, s, tmList)
	return tvOK && tmOK && crewOK
}
