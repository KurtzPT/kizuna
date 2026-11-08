package provider

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"

	"kizuna/internal/anidb"
	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/sources"
)

// Episode titles and episode pairing, for both modes.
//
// Every episode collects one title per source it can be paired with (Ep.titles),
// and EPISODE_TITLES picks the first that is a real title. Pairing goes through
// the mapping: Anime-Lists' per-episode exceptions first, else the entry's TVDB
// season and episode offset. TheMovieDb is paired at the same season and number
// as TheTVDB (most anime are numbered alike on both).

// spot is a place on a TVDB-numbered series: season (-1 = absolute order) and
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

// tvdbSpot returns where an AniDB episode (season 1 regular, 0 = S special)
// of entry e sits on its TVDB series.
func (p *Provider) tvdbSpot(e mapping.Entry, season, index int) (spot, bool) {
	if season == 0 && index >= 100 {
		return spot{}, false // openings, endings, trailers…: never on TheTVDB
	}
	if !e.HasException(season, index) && e.TVDBSeason == -1 && e.TVDBOffset == 0 && !p.lists.StartsSeries(e.AID) {
		// "absolute from episode 1" on a later entry of the series: the
		// mapping has no real position for it (a Fribb entry without season
		// info), so it would get the first season's episodes
		return spot{}, false
	}
	s, n, ok := e.TVDBEpisode(season, index)
	return spot{s, n}, ok
}

// --- /anidb ----------------------------------------------------------------

// fillEpisodes pairs an /anidb show's episodes with its TVDB (and TMDB)
// series. TheTVDB gives the thumbnail, which AniDB never has, plus the
// summary, air date and runtime where AniDB has none (EPISODE_FILL_SOURCES).
// Both give a title for EPISODE_TITLES. Returns false when a list it needed
// failed or came back partial.
func (p *Provider) fillEpisodes(ctx context.Context, s *Show, entry mapping.Entry) bool {
	fill := slices.Contains(p.cfg.EpisodeFillSources, "tvdb")
	wantTVDB := (fill || slices.Contains(p.cfg.EpisodeTitles, "tvdb")) && p.tvdb.Enabled() && entry.TVDB > 0
	wantTMDB := slices.Contains(p.cfg.EpisodeTitles, "tmdb") && p.tmdb.Enabled() && entry.TMDBTV > 0
	if !wantTVDB && !wantTMDB {
		return true
	}
	var tv, tm episodeIndex
	tvOK, tmOK := true, true
	var wg sync.WaitGroup
	if wantTVDB {
		wg.Go(func() { tv, tvOK = p.seriesEpisodes(ctx, "tvdb", entry.TVDB) })
	}
	if wantTMDB {
		wg.Go(func() { tm, tmOK = p.seriesEpisodes(ctx, "tmdb", entry.TMDBTV) })
	}
	wg.Wait()

	paired := 0
	for _, season := range []int{1, 0} {
		for i := range s.Episodes[season] {
			ep := &s.Episodes[season][i]
			sp, ok := p.tvdbSpot(entry, season, ep.Index)
			if !ok {
				continue
			}
			if e, ok := tv.at(sp); ok {
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
			if e, ok := tm.at(sp); ok {
				ep.setTitle("tmdb", e.Title)
			}
		}
	}
	slog.Debug("episode pairing", "root", s.Root, "tvdb", entry.TVDB, "season", entry.TVDBSeason, "offset", entry.TVDBOffset,
		"exceptions", len(entry.Episodes), "episodes", len(s.Episodes[1])+len(s.Episodes[0]), "paired_tvdb", paired)
	return tvOK && tmOK
}

// --- /tvdb -----------------------------------------------------------------

// tvdbEpisodeTitles adds every title EPISODE_TITLES may pick to a /tvdb show:
// the other of TVDB/TMDB (the episode list's own source is already set),
// paired by season and number, and AniDB's, through every AniDB entry of the
// series. first is the series' first AniDB entry (already fetched). Other
// entries come from the cache; missing ones are fetched in the background
// for the next build. Returns false while some are still missing, or when
// the other episode list failed.
func (p *Provider) tvdbEpisodeTitles(ctx context.Context, s *Show, tvdbID, tmdbID int, listSrc string, first *sources.AniDBAnime) bool {
	order := p.cfg.EpisodeTitles
	ready := true
	for _, src := range []string{"tvdb", "tmdb"} {
		id, enabled := tvdbID, p.tvdb.Enabled()
		if src == "tmdb" {
			id, enabled = tmdbID, p.tmdb.Enabled() && tmdbID > 0
		}
		if src == listSrc || !enabled || !slices.Contains(order, src) {
			continue
		}
		x, ok := p.seriesEpisodes(ctx, src, id)
		ready = ready && ok
		s.eachEpisode(func(ep *Ep) {
			if e, ok := x.at(ep.listed); ok {
				ep.setTitle(src, e.Title)
			}
		})
	}
	if !slices.Contains(order, "anidb") {
		return ready
	}

	type found struct{ title, any string }
	at := map[spot]found{}
	firstAID := p.lists.FirstAID(tvdbID)
	for _, aid := range p.lists.AIDsForTVDB(tvdbID) {
		a := first
		if aid != firstAID {
			var fresh bool
			if a, fresh = p.anidb.Peek(aid); !fresh && p.anidb.Enabled() {
				p.anidb.Prefetch(aid)
				ready = ready && a != nil
			}
		}
		if a == nil {
			continue
		}
		e := p.lists.ByAID(aid)
		for _, ae := range a.Episodes {
			sp, ok := p.tvdbSpot(e, ae.Season, ae.Index)
			if _, taken := at[sp]; !ok || taken {
				continue
			}
			at[sp] = found{anidb.FindTitle(ae.Titles, p.cfg.EpisodeLangs, false), anidb.PickTitle(ae.Titles, p.cfg.EpisodeLangs, false)}
		}
	}
	s.eachEpisode(func(ep *Ep) {
		f, ok := at[ep.listed]
		if !ok && ep.Season > 0 { // entries mapped in TheTVDB's absolute order
			f, ok = at[spot{-1, ep.Index}]
		}
		if ok {
			ep.setTitle("anidb", f.title)
			ep.anyTitle = f.any
		}
	})
	return ready
}

// --- picking ---------------------------------------------------------------

// genericTitle: placeholder titles that don't count as a title, so the next
// source in EPISODE_TITLES gets a chance ("Episode 12", "TBA").
var genericTitle = regexp.MustCompile(`(?i)^\s*(?:(?:episode|ep\.?)\s*\d+|tba|tbd|tbc)\s*$`)

// pickEpisodeTitles sets every episode's title per EPISODE_TITLES and logs
// (debug) how many titles each source gave.
func (p *Provider) pickEpisodeTitles(s *Show) {
	used := map[string]int{}
	s.eachEpisode(func(ep *Ep) {
		var src string
		ep.Title, src = episodeTitle(ep, p.cfg.EpisodeTitles)
		used[cmpOr(src, "-")]++
	})
	attrs := []any{"root", s.Root, "order", p.cfg.EpisodeTitles}
	for _, src := range append(slices.Clone(p.cfg.EpisodeTitles), "-") {
		attrs = append(attrs, src, used[src])
	}
	slog.Debug("episode titles: source used", attrs...)
}

// episodeTitle: the first real title in source order; else AniDB's title in
// any language (when anidb is listed); else a placeholder title from a
// listed source; else AniDB's title even when anidb isn't listed, since no
// listed source has any (openings and endings are only on AniDB); else ""
// (shown as "Episode N"). EPISODE_TITLES=none always gives "".
func episodeTitle(ep *Ep, order []string) (title, src string) {
	for _, src := range order {
		if t := ep.titles[src]; t != "" && !genericTitle.MatchString(t) {
			return t, src
		}
	}
	if slices.Contains(order, "anidb") && ep.anyTitle != "" && !genericTitle.MatchString(ep.anyTitle) {
		return ep.anyTitle, "anidb"
	}
	for _, src := range order {
		if t := ep.titles[src]; t != "" {
			return t, src
		}
	}
	if len(order) > 0 && ep.anyTitle != "" { // no listed source has any title (openings, endings…: only AniDB lists them)
		return ep.anyTitle, "anidb"
	}
	return "", ""
}

func (e *Ep) setTitle(src, title string) {
	if title = strings.TrimSpace(title); title == "" {
		return
	}
	if e.titles == nil {
		e.titles = map[string]string{}
	}
	e.titles[src] = title
}

func (s *Show) eachEpisode(f func(*Ep)) {
	for n := range s.Episodes {
		for i := range s.Episodes[n] {
			f(&s.Episodes[n][i])
		}
	}
}
