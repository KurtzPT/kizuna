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
// season and episode offset. In /anidb, TheMovieDb is paired the same way by its
// own season, offset and exceptions (tmdbseason…), since its seasons often
// differ from TheTVDB's; an entry without them gets TheTVDB's season and
// number. In /tvdb, the other site (TheMovieDb, or TheTVDB when the list came
// from TheMovieDb) is paired through AniDB: the place on the list gives the
// AniDB episode, and the mapping gives its place on the other site.

// spot is a place on a TVDB or TMDB series: season (-1 = absolute order) and
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

// of finds an episode at the place at gives it on this list.
func (x episodeIndex) of(ep *Ep, at func(*Ep) (spot, bool)) (sources.Episode, bool) {
	if sp, ok := at(ep); ok {
		return x.at(sp)
	}
	return sources.Episode{}, false
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
// of entry e sits on its TVDB series (openings, endings, trailers… only
// through a mapping exception: see mapping.Entry.TVDBEpisode).
func (p *Provider) tvdbSpot(e mapping.Entry, season, index int) (spot, bool) {
	if season == 0 && !e.HasException(season, index) && (e.TVDBSeason == 0 || !p.lists.StartsSeries(e.AID)) {
		// An unlisted special is the TVDB special with the same number only
		// on the entry that starts the series. A later entry's S1 would land
		// on the first entry's S00E01, and an entry whose own episodes live
		// in TVDB season 0 (OVAs, movies) shares its numbers with them.
		return spot{}, false
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

// tmdbSpot is tvdbSpot for the entry's TheMovieDb show, placed by the
// mapping's TMDB season, offset and exceptions (see
// mapping.Entry.TMDBEpisode), with the same rules for unlisted specials and
// later entries. An entry without a TMDB season gets TheTVDB's spot.
func (p *Provider) tmdbSpot(e mapping.Entry, season, index int) (spot, bool) {
	listed := e.HasTMDBException(season, index)
	switch {
	case !listed && !e.TMDBMapped:
		return p.tvdbSpot(e, season, index)
	case !listed && season == 0 && (e.TMDBSeason == 0 || !p.lists.StartsTMDBShow(e.AID)):
		return spot{}, false
	case !listed && e.TMDBSeason == -1 && e.TMDBOffset == 0 && !p.lists.StartsTMDBShow(e.AID):
		return spot{}, false
	}
	s, n, ok := e.TMDBEpisode(season, index)
	return spot{s, n}, ok
}

// --- /anidb ----------------------------------------------------------------

// fillEpisodes pairs an /anidb show's episodes with its TVDB (and TMDB)
// series. TheTVDB gives the thumbnail, which AniDB never has, plus the
// summary, air date and runtime where AniDB has none (EPISODE_FILL_SOURCES).
// Both give a title for EPISODE_TITLES, and directors and writers (AniDB has
// none per episode). Returns false when a list it needed failed or came back
// partial, or episode crew is still being fetched.
func (p *Provider) fillEpisodes(ctx context.Context, s *Show, entry mapping.Entry) bool {
	fill := slices.Contains(p.cfg.EpisodeFillSources, "tvdb")
	wantTVDB := p.tvdb.Enabled() && entry.TVDB > 0
	wantTMDB := p.tmdb.Enabled() && entry.TMDBTV > 0
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

	tvAt := func(ep *Ep) (spot, bool) {
		if ep.Season != 0 && ep.Season != 1 {
			return spot{}, false
		}
		return p.tvdbSpot(entry, ep.Season, ep.mapIndex())
	}
	tmAt := func(ep *Ep) (spot, bool) {
		if ep.Season != 0 && ep.Season != 1 {
			return spot{}, false
		}
		return p.tmdbSpot(entry, ep.Season, ep.mapIndex())
	}
	paired, pairedTMDB := 0, 0
	for _, season := range []int{1, 0} {
		for i := range s.Episodes[season] {
			ep := &s.Episodes[season][i]
			if sp, ok := tmAt(ep); ok {
				if e, ok := tm.at(sp); ok {
					pairedTMDB++
					ep.setTitle("tmdb", e.Title)
				}
			}
			sp, ok := tvAt(ep)
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
		}
	}
	slog.Debug("episode pairing", "root", s.Root, "tvdb", entry.TVDB, "season", entry.TVDBSeason, "offset", entry.TVDBOffset,
		"exceptions", len(entry.Episodes), "episodes", len(s.Episodes[1])+len(s.Episodes[0]), "paired_tvdb", paired,
		"tmdb", entry.TMDBTV, "tmdb_mapped", entry.TMDBMapped, "paired_tmdb", pairedTMDB)
	crewOK := p.episodeCrew(ctx, s, tv, tm, tvAt, tmAt)
	return tvOK && tmOK && crewOK
}

// --- /tvdb -----------------------------------------------------------------

// tvdbPairing is what a /tvdb show's AniDB entries tell about each place on
// its episode list (TheTVDB's, or TheMovieDb's when the list came from
// there): AniDB's titles, and the place of the same AniDB episode on the
// other site, which often splits seasons differently (One Piece: TVDB S2E1
// is AniDB episode 9, TMDB S2E1 is episode 62).
type tvdbPairing struct {
	titles map[spot]anidbTitles
	other  map[spot]spot
}

type anidbTitles struct{ title, any string }

// atListed looks up an episode by its place on the list, else (regular
// episodes) by its absolute number, for entries mapped in absolute order.
func atListed[V any](m map[spot]V, ep *Ep) (V, bool) {
	v, ok := m[ep.listed]
	if !ok && ep.Season > 0 {
		v, ok = m[spot{-1, ep.Index}]
	}
	return v, ok
}

// otherAt: an episode's place on the other site, through AniDB; without an
// AniDB counterpart (not cached yet, or a TheTVDB-only extra), the same
// season and number as on the list.
func (pr tvdbPairing) otherAt(ep *Ep) (spot, bool) {
	if sp, ok := atListed(pr.other, ep); ok {
		return sp, true
	}
	return ep.listed, true
}

// tvdbPairing walks every AniDB entry of a /tvdb series. first is the
// series' first AniDB entry (already fetched). Other entries come from the
// cache; missing ones are fetched in the background for the next build, and
// ready is false meanwhile.
func (p *Provider) tvdbPairing(tvdbID, tmdbID int, listSrc string, first *sources.AniDBAnime) (pr tvdbPairing, ready bool) {
	pr = tvdbPairing{map[spot]anidbTitles{}, map[spot]spot{}}
	ready = true
	// an entry of another TMDB show has no place on this one
	onTMDB := func(e mapping.Entry) bool { return e.TMDBTV == 0 || e.TMDBTV == tmdbID }
	listSpot, otherSpot := p.tvdbSpot, p.tmdbSpot
	listOK, otherOK := func(mapping.Entry) bool { return true }, onTMDB
	if listSrc == "tmdb" {
		listSpot, otherSpot = otherSpot, listSpot
		listOK, otherOK = otherOK, listOK
	}
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
		e := p.lists.ByAID(aid)
		if a == nil || !listOK(e) {
			continue
		}
		for _, ae := range a.Episodes {
			sp, ok := listSpot(e, ae.Season, ae.ListIndex())
			if !ok {
				continue
			}
			if _, taken := pr.titles[sp]; !taken { // first entry wins
				pr.titles[sp] = anidbTitles{anidb.FindTitle(ae.Titles, p.cfg.EpisodeLangs, false), anidb.PickTitle(ae.Titles, p.cfg.EpisodeLangs, false)}
			}
			if _, taken := pr.other[sp]; !taken && otherOK(e) {
				if o, ok := otherSpot(e, ae.Season, ae.ListIndex()); ok {
					pr.other[sp] = o
				}
			}
		}
	}
	return pr, ready
}

// tvdbEpisodeTitles adds every title EPISODE_TITLES may pick to a /tvdb show:
// the other of TVDB/TMDB (the episode list's own source is already set),
// paired through AniDB (tvdbPairing), and AniDB's, through every AniDB entry
// of the series. Returns the pairing for tvdbEpisodeCrew, and false while
// some AniDB entries are still missing, or when the other list failed.
func (p *Provider) tvdbEpisodeTitles(ctx context.Context, s *Show, tvdbID, tmdbID int, listSrc string, first *sources.AniDBAnime) (tvdbPairing, bool) {
	order := p.cfg.EpisodeTitles
	otherSrc, otherID, otherOn := "tmdb", tmdbID, p.tmdb.Enabled() && tmdbID > 0
	if listSrc == "tmdb" {
		otherSrc, otherID, otherOn = "tvdb", tvdbID, p.tvdb.Enabled()
	}
	var pr tvdbPairing
	ready := true
	if otherOn || slices.Contains(order, "anidb") { // the other site's crew needs the pairing too
		pr, ready = p.tvdbPairing(tvdbID, tmdbID, listSrc, first)
	}
	if otherOn && slices.Contains(order, otherSrc) {
		x, ok := p.seriesEpisodes(ctx, otherSrc, otherID)
		ready = ready && ok
		s.eachEpisode(func(ep *Ep) {
			if e, ok := x.of(ep, pr.otherAt); ok {
				ep.setTitle(otherSrc, e.Title)
			}
		})
	}
	if slices.Contains(order, "anidb") {
		s.eachEpisode(func(ep *Ep) {
			if f, ok := atListed(pr.titles, ep); ok {
				ep.setTitle("anidb", f.title)
				ep.anyTitle = f.any
			}
		})
	}
	return pr, ready
}

// tvdbEpisodeCrew sets directors and writers on a /tvdb show's episodes from
// its episode list (eps, absolute numbers filled in) and the other of
// TVDB/TMDB, paired through AniDB like the titles (pr). Returns false when
// the other list failed or episode crew is still being fetched.
func (p *Provider) tvdbEpisodeCrew(ctx context.Context, s *Show, tvdbID, tmdbID int, listSrc string, eps []sources.Episode, pr tvdbPairing) bool {
	own := indexEpisodes(eps)
	var tv, tm episodeIndex
	ready := true
	switch {
	case listSrc == "tvdb":
		tv = own
	case p.tvdb.Enabled():
		tv, ready = p.seriesEpisodes(ctx, "tvdb", tvdbID)
	}
	switch {
	case listSrc == "tmdb":
		tm = own
	case p.tmdb.Enabled() && tmdbID > 0:
		var ok bool
		tm, ok = p.seriesEpisodes(ctx, "tmdb", tmdbID)
		ready = ready && ok
	}
	listed := func(ep *Ep) (spot, bool) { return ep.listed, true }
	tvAt, tmAt := listed, pr.otherAt
	if listSrc == "tmdb" {
		tvAt, tmAt = pr.otherAt, listed
	}
	crewOK := p.episodeCrew(ctx, s, tv, tm, tvAt, tmAt)
	return ready && crewOK
}

// episodeCrew sets each episode's directors and writers: TheTVDB's, else
// TheMovieDb's (per field). tvAt and tmAt give an episode's place on each
// list. A TheTVDB v4 list has no crew: it's in one record per episode
// (EpisodeCrew). Returns false while some of those are still being fetched.
func (p *Provider) episodeCrew(ctx context.Context, s *Show, tv, tm episodeIndex, tvAt, tmAt func(*Ep) (spot, bool)) bool {
	var ids []int
	s.eachEpisode(func(ep *Ep) {
		if e, ok := tv.of(ep, tvAt); ok && e.ID > 0 {
			ids = append(ids, e.ID)
		}
	})
	crew, pending := p.tvdb.EpisodeCrew(ctx, ids)
	s.eachEpisode(func(ep *Ep) {
		te, tvOK := tv.of(ep, tvAt)
		me, tmOK := tm.of(ep, tmAt)
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
