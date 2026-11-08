package provider

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"kizuna/internal/mapping"
)

// fillEpisodes pairs an /anidb show's episodes with its TVDB (and TMDB)
// series. TheTVDB gives the thumbnail, which AniDB never has, plus the
// summary, air date and runtime where AniDB has none (EPISODE_FILL_SOURCES).
// Both give a title for EPISODE_TITLES, and directors and writers (AniDB has
// none per episode). Returns false when a list it needed failed or came back
// partial, or episode crew is still being fetched.
func (p *Provider) fillEpisodes(ctx context.Context, s *Show, entry mapping.Entry) bool {
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
		wg.Go(func() { tv, tvOK = p.seriesEpisodes(ctx, "tvdb", entry.TVDB.ID) })
	}
	if wantTMDB {
		wg.Go(func() { tm, tmOK = p.seriesEpisodes(ctx, "tmdb", entry.TMDB.ID) })
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
	paired, pairedTMDB := 0, 0
	for _, season := range []int{1, 0} {
		for i := range s.Episodes[season] {
			ep := &s.Episodes[season][i]
			if e, ok := tmList.of(ep); ok {
				pairedTMDB++
				ep.setTitle("tmdb", e.Title)
			}
			if e, ok := tvList.of(ep); ok {
				paired++
				ep.setTitle("tvdb", e.Title)
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
	slog.Debug("episode pairing", "root", s.Root, "tvdb", entry.TVDB.ID, "season", entry.TVDB.Season, "offset", entry.TVDB.Offset,
		"exceptions", len(entry.TVDB.Episodes), "episodes", len(s.Episodes[1])+len(s.Episodes[0]), "paired_tvdb", paired,
		"tmdb", entry.TMDB.ID, "tmdb_mapped", entry.TMDB.Mapped, "paired_tmdb", pairedTMDB)
	crewOK := p.episodeCrew(ctx, s, tvList, tmList)
	return tvOK && tmOK && crewOK
}
