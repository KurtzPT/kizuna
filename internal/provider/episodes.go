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
// AniDB episode, and the mapping gives its place on the other site; without
// an AniDB counterpart, by absolute number (tvdbPairing.otherAt).

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

// pairedList is one site's episode list, paired with a show: at gives each
// of the show's episodes its spot on the list.
type pairedList struct {
	x  episodeIndex
	at func(*Ep) (spot, bool)
}

// of looks up ep at the spot at() gives it on this list.
func (l pairedList) of(ep *Ep) (sources.Episode, bool) {
	if sp, ok := l.at(ep); ok {
		return l.x.at(sp)
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

// spotOn returns where an AniDB episode (season 1 regular, 0 = S special)
// of an entry sits on the series pl places it on (openings, endings,
// trailers… only through a mapping exception: see mapping.Placement.Episode).
// starts: the entry is the first one of that series.
func spotOn(pl mapping.Placement, starts bool, season, index int) (spot, bool) {
	if !pl.HasException(season, index) {
		if season == 0 && (pl.Season == 0 || !starts) {
			// An unlisted special is the special with the same number only
			// on the entry that starts the series. A later entry's S1 would land
			// on the first entry's S00E01, and an entry whose own episodes live
			// in season 0 (OVAs, movies) shares its numbers with them.
			return spot{}, false
		}
		if pl.Season == -1 && pl.Offset == 0 && !starts {
			// "absolute from episode 1" on a later entry of the series: the
			// mapping has no real position for it (a Fribb entry without season
			// info), so it would get the first season's episodes
			return spot{}, false
		}
	}
	s, n, ok := pl.Episode(season, index)
	return spot{s, n}, ok
}

// entrySpots places one entry's AniDB episodes on TheTVDB and TheMovieDb.
// Whether the entry starts each series is looked up once, not per episode.
type entrySpots struct {
	tvdb, tmdb             mapping.Placement
	startsTVDB, startsTMDB bool
}

func (p *Provider) spotsOf(e mapping.Entry) entrySpots {
	return entrySpots{e.TVDB, e.TMDB, p.lists.StartsShow("tvdb", e.AID), p.lists.StartsShow("tmdb", e.AID)}
}

// at returns where an AniDB episode sits on a site ("tvdb" or "tmdb"). On
// TheMovieDb the mapping's TMDB season, offset and exceptions place it, with
// TheTVDB's rules for unlisted specials and later entries; an entry without
// a TMDB season gets TheTVDB's spot, except for the episodes a TMDB
// exception lists.
func (es entrySpots) at(site string, season, index int) (spot, bool) {
	if site == "tmdb" && (es.tmdb.Mapped || es.tmdb.HasException(season, index)) {
		return spotOn(es.tmdb, es.startsTMDB, season, index)
	}
	return spotOn(es.tvdb, es.startsTVDB, season, index)
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

// otherAt: an episode's place on the other site, through AniDB. Without an
// AniDB counterpart (not cached yet, or a TheTVDB-only extra), a regular
// episode goes by its absolute number, which doesn't depend on how either
// site splits seasons (an absolute number the other site doesn't have pairs
// with nothing). A special has no absolute number: it keeps its number in
// season 0, as unlisted specials do everywhere else.
func (pr tvdbPairing) otherAt(ep *Ep) (spot, bool) {
	if sp, ok := atListed(pr.other, ep); ok {
		return sp, true
	}
	if ep.Season > 0 {
		return spot{-1, ep.Index}, true
	}
	return ep.listed, true
}

// tvdbBuild is what the steps of a /tvdb build share.
type tvdbBuild struct {
	tvdbID, tmdbID int                 // tmdbID 0 = no TheMovieDb show
	list, other    string              // the episode list's site ("tvdb" or "tmdb") and the other one
	first          *sources.AniDBAnime // the series' first AniDB entry (nil = not available)
	eps            []sources.Episode   // the episode list, absolute numbers filled in
}

func newTVDBBuild(tvdbID, tmdbID int, list string, first *sources.AniDBAnime, eps []sources.Episode) *tvdbBuild {
	other := "tmdb"
	if list == "tmdb" {
		other = "tvdb"
	}
	return &tvdbBuild{tvdbID: tvdbID, tmdbID: tmdbID, list: list, other: other, first: first, eps: eps}
}

// id is the show's id on a site.
func (b *tvdbBuild) id(site string) int {
	if site == "tmdb" {
		return b.tmdbID
	}
	return b.tvdbID
}

// fetchable reports whether a site's episode list can be had: TheTVDB has
// every /tvdb show, TheMovieDb only one it was found on.
func (b *tvdbBuild) fetchable(p *Provider, site string) bool {
	if site == "tmdb" {
		return p.tmdb.Enabled() && b.tmdbID > 0
	}
	return p.tvdb.Enabled()
}

// onShow reports whether an entry has a place on the show on a site. Every
// entry walked is on the TVDB series; an entry of another TMDB show has no
// place on this one.
func (b *tvdbBuild) onShow(site string, e mapping.Entry) bool {
	return site != "tmdb" || e.TMDB.ID == 0 || e.TMDB.ID == b.tmdbID
}

// tvdbPairing walks every AniDB entry of a /tvdb series. b.first is already
// fetched. Other entries come from the cache; missing ones are fetched in the
// background for the next build, and ready is false meanwhile.
func (p *Provider) tvdbPairing(b *tvdbBuild) (pr tvdbPairing, ready bool) {
	pr = tvdbPairing{map[spot]anidbTitles{}, map[spot]spot{}}
	ready = true
	firstAID := p.lists.FirstAID(b.tvdbID)
	for _, aid := range p.lists.AIDsForTVDB(b.tvdbID) {
		a := b.first
		if aid != firstAID {
			var fresh bool
			if a, fresh = p.anidb.Peek(aid); !fresh && p.anidb.Enabled() {
				p.anidb.Prefetch(aid)
				ready = ready && a != nil
			}
		}
		e := p.lists.ByAID(aid)
		if a == nil || !b.onShow(b.list, e) {
			continue
		}
		es, otherOK := p.spotsOf(e), b.onShow(b.other, e)
		for _, ae := range a.Episodes {
			sp, ok := es.at(b.list, ae.Season, ae.ListIndex())
			if !ok {
				continue
			}
			if _, taken := pr.titles[sp]; !taken { // first entry wins
				pr.titles[sp] = anidbTitles{anidb.FindTitle(ae.Titles, p.cfg.EpisodeLangs, false), anidb.PickTitle(ae.Titles, p.cfg.EpisodeLangs, false)}
			}
			if _, taken := pr.other[sp]; !taken && otherOK {
				if o, ok := es.at(b.other, ae.Season, ae.ListIndex()); ok {
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
func (p *Provider) tvdbEpisodeTitles(ctx context.Context, s *Show, b *tvdbBuild) (tvdbPairing, bool) {
	order := p.cfg.EpisodeTitles
	otherOn := b.fetchable(p, b.other)
	var pr tvdbPairing
	ready := true
	if otherOn || slices.Contains(order, "anidb") { // the other site's crew needs the pairing too
		pr, ready = p.tvdbPairing(b)
	}
	if otherOn && slices.Contains(order, b.other) {
		x, ok := p.seriesEpisodes(ctx, b.other, b.id(b.other))
		ready = ready && ok
		other := pairedList{x, pr.otherAt}
		s.eachEpisode(func(ep *Ep) {
			if e, ok := other.of(ep); ok {
				ep.setTitle(b.other, e.Title)
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
// its episode list and the other of TVDB/TMDB, paired through AniDB like the
// titles (pr). Returns false when the other list failed or episode crew is
// still being fetched.
func (p *Provider) tvdbEpisodeCrew(ctx context.Context, s *Show, b *tvdbBuild, pr tvdbPairing) bool {
	ready := true
	paired := func(site string) pairedList {
		if site == b.list {
			return pairedList{indexEpisodes(b.eps), func(ep *Ep) (spot, bool) { return ep.listed, true }}
		}
		var x episodeIndex
		if b.fetchable(p, site) {
			var ok bool
			x, ok = p.seriesEpisodes(ctx, site, b.id(site))
			ready = ready && ok
		}
		return pairedList{x, pr.otherAt}
	}
	tv := paired("tvdb")
	tm := paired("tmdb")
	crewOK := p.episodeCrew(ctx, s, tv, tm)
	return ready && crewOK
}

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
