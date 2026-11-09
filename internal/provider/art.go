package provider

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/sources"
)

// Season art: a show's or a Plex season's posters, backgrounds and logos
// come from the TheTVDB and TheMovieDb season they are of, where those sites
// have art of the kind for seasons (SEASON_ART_FALLBACK: their series' art
// when the season has none); every other source gives its series' art.

// artKind is one of the image kinds Kizuna sends Plex.
type artKind int

const (
	posterArt artKind = iota
	backgroundArt
	logoArt
)

var artKinds = [...]artKind{posterArt, backgroundArt, logoArt}

func (k artKind) of(a sources.Art) []string {
	switch k {
	case posterArt:
		return a.Posters
	case backgroundArt:
		return a.Backgrounds
	}
	return a.Logos
}

func (k artKind) name() string {
	return [...]string{"Posters", "Backgrounds", "Logos"}[k]
}

// order is the kind's source setting (POSTER_SOURCES...).
func (k artKind) order(ss *showSources) []string {
	cfg := ss.p.cfg
	return [...][]string{cfg.PosterSources, cfg.BackgroundSources, cfg.LogoSources}[k]
}

// noSeason: a target without a season of its own on a site (absolute order,
// not mapped, no id, a TheMovieDb movie): the site's series art.
const noSeason = -1

// artTarget is what a show's or a Plex season's art is of: its season on
// TheTVDB and on TheMovieDb. seriesFirst: the series' art comes first, then
// season 1's (an /anidb entry that starts its series).
type artTarget struct {
	tvdb, tmdb  int
	seriesFirst bool
}

var seriesTarget = artTarget{tvdb: noSeason, tmdb: noSeason}

// plexSeason is one Plex season and what its art is of.
type plexSeason struct {
	n int
	t artTarget
}

func (t artTarget) on(src string) int {
	switch src {
	case "tvdb":
		return t.tvdb
	case "tmdb":
		return t.tmdb
	}
	return noSeason
}

// placed is the season the mapping puts an entry in on a site, if any.
func placed(pl mapping.Placement) int {
	if pl.ID > 0 && pl.Mapped && pl.Season >= 0 {
		return pl.Season
	}
	return noSeason
}

// anidbTargets sets an /anidb show's targets: the show and its Season 1
// are the season the mapping puts the entry in on each site (the series'
// art first for the entry that starts it: the show only), its Specials
// season 0 of both sites.
func (ss *showSources) anidbTargets(entry mapping.Entry, seasons []int) {
	t := artTarget{tvdb: placed(entry.TVDB), tmdb: placed(entry.TMDB)}
	specials := artTarget{tvdb: 0, tmdb: 0}
	if ss.movie { // the movie's art: TheMovieDb has no seasons for it
		t.tmdb, specials.tmdb = noSeason, noSeason
	}
	ss.seasons = make([]plexSeason, len(seasons))
	ss.seasonArts = make([]*seasonFetch, 0, 2*len(seasons)+2) // every season of both sites, at most
	for i, n := range seasons {
		ss.seasons[i] = plexSeason{n, t}
		if n == 0 {
			ss.seasons[i].t = specials
		}
	}
	t.seriesFirst = ss.series
	ss.show = t
}

// tvdbTargets sets a /tvdb show's targets: the show is the series, each
// Plex season the same TheTVDB season. TheMovieDb's seasons split
// differently: pairTMDB adds them once the episodes are paired.
func (ss *showSources) tvdbTargets(seasons []int) {
	ss.show = seriesTarget
	ss.seasons = make([]plexSeason, len(seasons))
	ss.seasonArts = make([]*seasonFetch, 0, len(seasons)) // TheTVDB's; TheMovieDb's come with its episode list
	for i, n := range seasons {
		ss.seasons[i] = plexSeason{n, artTarget{tvdb: n, tmdb: noSeason}}
	}
}

// pairTMDB gives each Plex season of a /tvdb show the TheMovieDb season its
// episodes are paired with (tmdbSeasonPairs), whose posters came with
// TheMovieDb's episode list.
func (ss *showSources) pairTMDB(pairs map[int]int, posters map[int][]string) {
	ss.tmdbPosters, ss.tmdbListed = posters, true
	for i, s := range ss.seasons {
		if m, ok := pairs[s.n]; ok {
			ss.seasons[i].t.tmdb = m
		}
	}
}

// tmdbSeasonPairs pairs the TheTVDB seasons of a /tvdb show with
// TheMovieDb's where that is clear: every episode of the season found on
// TheMovieDb is in one season there, which holds no other TheTVDB season's
// episodes. Seasons that are split or merged differently have no pair.
func tmdbSeasonPairs(s *Show, x episodeIndex, pr tvdbPairing) map[int]int {
	const mixed = -2
	to, from := map[int]int{}, map[int]int{} // TVDB -> TMDB season, and back
	join := func(m map[int]int, k, v int) {
		if old, ok := m[k]; ok && old != v {
			v = mixed
		}
		m[k] = v
	}
	for n, eps := range s.Episodes {
		for i := range eps {
			sp, ok := pr.otherAt(&eps[i])
			if !ok {
				continue
			}
			if e, ok := x.at(sp); ok {
				join(to, n, e.Season)
				join(from, e.Season, n)
			}
		}
	}
	for n, m := range to {
		if m == mixed || from[m] != n {
			delete(to, n)
		}
	}
	return to
}

// seasonKind: a source has art of the kind for seasons. TheTVDB's seasons
// have posters and backgrounds (the legacy key: posters only), TheMovieDb's
// posters; no site has season logos.
func (ss *showSources) seasonKind(src string, k artKind) bool {
	switch src {
	case "tvdb":
		return k == posterArt || k == backgroundArt && ss.p.tvdb.API() == "v4"
	case "tmdb":
		return k == posterArt && !ss.movie
	}
	return false
}

// artParts: one source's images of a kind for a target, in order. A source
// with season art of the kind gives its season's, then its series' if
// SEASON_ART_FALLBACK allows: when the season has none, or with every image
// listed (ART_ALL_SOURCES). The entry that starts its series gets the
// series' art, then season 1's. Any other source, or a target without a
// season on the site, gives the series' art.
func (ss *showSources) artParts(src string, k artKind, t artTarget) (first, then []string) {
	n := t.on(src)
	if n == noSeason || !ss.seasonKind(src, k) {
		return k.of(ss.art(src)), nil
	}
	season := k.of(ss.seasonArt(src, n))
	if t.seriesFirst && n == 1 {
		return k.of(ss.art(src)), season
	}
	if cfg := ss.p.cfg; cfg.SeasonArtFallback && (cfg.ArtAllSources || len(season) == 0) {
		return season, k.of(ss.art(src))
	}
	return season, nil
}

// describeArt says, for the match report, what a source offered:
// "season 3: 4 posters, series: 12 posters". Only reads what artParts
// fetched.
func (ss *showSources) describeArt(src string, k artKind, t artTarget) string {
	first, then := ss.artParts(src, k, t)
	what := strings.ToLower(k.name())
	n := t.on(src)
	if n == noSeason || !ss.seasonKind(src, k) {
		return countOf(len(first), what)
	}
	a, b := strings.ToLower(seasonTitle(n)), "series"
	if t.seriesFirst && n == 1 {
		a, b = b, a
	}
	var parts []string
	for _, p := range []struct {
		of   string
		urls []string
	}{{a, first}, {b, then}} {
		if len(p.urls) > 0 {
			parts = append(parts, p.of+": "+countOf(len(p.urls), what))
		}
	}
	return strings.Join(parts, ", ")
}

// seasonFetch is one TheTVDB or TheMovieDb season's art, fetched once.
type seasonFetch struct {
	src  string
	n    int
	once sync.Once
	art  sources.Art
	err  error
}

// seasonArt returns a TheTVDB or TheMovieDb season's art, fetched once, on
// first use.
func (ss *showSources) seasonArt(src string, n int) sources.Art {
	id, on := ss.tvdb, ss.p.tvdb.Enabled()
	if src == "tmdb" {
		id, on = ss.tmdb, ss.p.tmdb.Enabled() && !ss.movie
	}
	if id == 0 || !on || ss.seasonArtOf == nil {
		return sources.Art{}
	}
	if src == "tmdb" && ss.tmdbListed {
		return sources.Art{Posters: ss.tmdbPosters[n]}
	}
	sid := 0 // the legacy key goes by the number
	if src == "tvdb" && ss.p.tvdb.API() == "v4" {
		// The season ids come with the series record's details: a season it
		// doesn't list (or no record) has no art, and no fetch.
		if ss.tvdbDetails.get(); ss.tvdbSeasonIDs[n] == 0 {
			return sources.Art{}
		}
		sid = ss.tvdbSeasonIDs[n]
	}
	ss.seasonMu.Lock()
	i := slices.IndexFunc(ss.seasonArts, func(f *seasonFetch) bool { return f.src == src && f.n == n })
	if i < 0 {
		i = len(ss.seasonArts)
		ss.seasonArts = append(ss.seasonArts, &seasonFetch{src: src, n: n})
	}
	f := ss.seasonArts[i]
	ss.seasonMu.Unlock()
	f.once.Do(func() { f.art, f.err = ss.seasonArtOf(src, n, sid) })
	return f.art
}

// seasonsFailed names the season art fetches that failed (not with a 404),
// sorted. Call it only after the fetches are done.
func (ss *showSources) seasonsFailed() []string {
	ss.seasonMu.Lock()
	defer ss.seasonMu.Unlock()
	var out []string
	for _, f := range ss.seasonArts {
		if f.err != nil && !fetch.IsNotFound(f.err) {
			out = append(out, fmt.Sprintf("%s season %d art", f.src, f.n))
		}
	}
	slices.Sort(out)
	return out
}

// pickArt picks one target's posters, backgrounds and logos, each kind
// from its source order. season names a Plex season's fields in the match
// report ("Season 3 posters"); "" = the show's (only the show is noted
// with the reports off). count: also say which
// sources gave them ("tvdb:3,tmdb:9"), for the debug line or the report.
func (ss *showSources) pickArt(t artTarget, season string, count bool) (a sources.Art, used [len(artKinds)]string) {
	all := ss.p.cfg.ArtAllSources
	for _, k := range artKinds {
		order := k.order(ss)
		urls, counts := images(order, all, count, func(src string) ([]string, []string) { return ss.artParts(src, k, t) })
		switch k {
		case posterArt:
			a.Posters = urls
		case backgroundArt:
			a.Backgrounds = urls
		default:
			a.Logos = urls
		}
		used[k] = counts
		if ss.rec != nil {
			name := k.name()
			if season != "" {
				name = season + " " + strings.ToLower(name)
			}
			ss.recordField(name, order, counts, all && counts != "", func(src string) string { return ss.describeArt(src, k, t) })
		}
	}
	return a, used
}

// fillSeasons sets the art of each Plex season (Show.SeasonArt).
func (ss *showSources) fillSeasons(s *Show) {
	if len(ss.seasons) == 0 {
		return
	}
	s.SeasonArt = make(map[int]sources.Art, len(ss.seasons))
	for _, ps := range ss.seasons {
		name := ""
		if ss.rec != nil {
			name = seasonTitle(ps.n)
		}
		s.SeasonArt[ps.n], _ = ss.pickArt(ps.t, name, ss.rec != nil)
	}
}

// warmArt adds jobs that fetch, in parallel, the art each target will ask
// for first: each kind's first source (every listed one in "all" mode). One
// job per distinct target.
func (ss *showSources) warmArt(jobs []func()) []func() {
	all := ss.p.cfg.ArtAllSources
	warm := func(t artTarget) {
		for _, k := range artKinds {
			for i, src := range k.order(ss) {
				if i > 0 && !all {
					break
				}
				ss.artParts(src, k, t)
			}
		}
	}
	jobs = append(jobs, func() { warm(ss.show) })
	for i, s := range ss.seasons {
		seen := s.t == ss.show
		for _, o := range ss.seasons[:i] {
			seen = seen || o.t == s.t
		}
		if !seen {
			jobs = append(jobs, func() { warm(s.t) })
		}
	}
	return jobs
}

// plexSeasons: the seasons Plex gets for a show (seasonItem): those with
// episodes, and Specials for placeholders.
func plexSeasons(s *Show) []int {
	out := s.Seasons()
	if s.Placeholders && !slices.Contains(out, 0) {
		out = append([]int{0}, out...)
	}
	return out
}
