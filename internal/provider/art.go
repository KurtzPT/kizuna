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
// TheTVDB and on TheMovieDb. first: on that site, the series' art comes
// first, then season 1's (an /anidb entry that starts its series there).
type artTarget struct {
	tvdb, tmdb int
	first      firstEntry
}

var seriesTarget = artTarget{tvdb: noSeason, tmdb: noSeason}

// plexSeason is one Plex season and what its art is of.
type plexSeason struct {
	n int
	t artTarget
}

// seriesFirst: the series' art comes before season 1's on src.
func (t artTarget) seriesFirst(src string) bool {
	return (src == "tvdb" || src == "plex") && t.first.tvdb || src == "tmdb" && t.first.tmdb
}

// on: the target's season on src. Plex's seasons are TheTVDB's.
func (t artTarget) on(src string) int {
	switch src {
	case "tvdb", "plex":
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
	t.first = ss.first
	ss.show = t
}

// tvdbTargets sets a /tvdb show's targets: the show is the series (as
// newSources set it), each Plex season the same TheTVDB season. TheMovieDb's
// seasons split differently: pairTMDB adds them once the episodes are paired.
func (ss *showSources) tvdbTargets(seasons []int) {
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
// posters, Plex's (a series') all three kinds.
func (ss *showSources) seasonKind(src string, k artKind) bool {
	switch src {
	case "tvdb":
		return k == posterArt || k == backgroundArt && ss.p.tvdb.API() == "v4"
	case "tmdb":
		return k == posterArt && !ss.movie
	case "plex":
		return ss.plexSeasonList.fetches()
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
	if t.seriesFirst(src) && n == 1 {
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
	if t.seriesFirst(src) && n == 1 {
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

// seasonArt returns a TheTVDB, TheMovieDb or Plex season's art, fetched
// once, on first use (Plex's with its season list).
func (ss *showSources) seasonArt(src string, n int) sources.Art {
	if src == "plex" {
		return ss.plexSeasonOf(n).Art
	}
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
// sources gave them and how many, for the debug line or the report.
func (ss *showSources) pickArt(t artTarget, season string, count bool) (a sources.Art, used artUsed) {
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
			ss.recordArt(name, order, counts, all && len(counts) > 0, func(src string) string { return ss.describeArt(src, k, t) })
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
		// Season 1 of an /anidb entry that starts neither series is of the
		// show's target: its art is the show's. With a match report it is
		// picked again, to note its fields.
		if ps.t == ss.show && ss.rec == nil {
			s.SeasonArt[ps.n] = s.Art
			continue
		}
		if ss.rec == nil {
			s.SeasonArt[ps.n], _ = ss.pickArt(ps.t, "", false)
			continue
		}
		art, by := ss.pickArt(ps.t, seasonTitle(ps.n), true)
		s.SeasonArt[ps.n] = art
		if s.artBy == nil {
			s.artBy = &artSources{}
		}
		if s.artBy.seasons == nil {
			s.artBy.seasons = make(map[int]artUsed, len(ss.seasons))
		}
		s.artBy.seasons[ps.n] = by
	}
}

// artUsed is pickArt's second value: per kind, how many of its images each
// source gave, in the images' order.
type artUsed = [len(artKinds)][]sourceCount

// artSources is which sources gave a show's and its seasons' art, kept on
// the Show for the match report: it lists the images the items carry, and
// those are made from the Show once the build is done.
type artSources struct {
	show    artUsed
	seasons map[int]artUsed
}

// sourceOf names the source of a kind's i-th image ("" = not known).
func sourceOf(counts []sourceCount, i int) string {
	for _, c := range counts {
		if i < c.n {
			return c.src
		}
		i -= c.n
	}
	return ""
}

// warmArt adds jobs that fetch, in parallel, the art each target will ask
// for first: each kind's first source that is asked (artAsked; every listed
// one in "all" mode), of the sources take takes. One job per distinct target
// and source. settled: setAniDB has run (see artAsked).
func (ss *showSources) warmArt(jobs []func(), take func(src string) bool, settled bool) []func() {
	all := ss.p.cfg.ArtAllSources
	asks := func(k artKind, src string) bool { // the kind's order asks src first (or at all)
		order := k.order(ss)
		if all {
			return slices.Contains(order, src)
		}
		for _, o := range order {
			if ss.artAsked(o, k, settled) {
				return o == src
			}
		}
		return false
	}
	add := func(t artTarget) {
		for _, src := range [...]string{"tvdb", "tmdb", "plex", "anidb", "mal", "ann"} {
			if take(src) && (asks(posterArt, src) || asks(backgroundArt, src) || asks(logoArt, src)) {
				jobs = append(jobs, func() {
					for _, k := range artKinds {
						if asks(k, src) {
							ss.artParts(src, k, t)
						}
					}
				})
			}
		}
	}
	add(ss.show)
	for i, s := range ss.seasons {
		seen := s.t == ss.show
		for _, o := range ss.seasons[:i] {
			seen = seen || o.t == s.t
		}
		if !seen {
			add(s.t)
		}
	}
	return jobs
}

// artAsked: pickArt stops at src for images of kind k (all mode aside): it
// can fetch them (TheTVDB, TheMovieDb, MAL, ANN: a key and an id), or has
// them at hand (AniDB's picture, a poster). A source that can't fetch and has
// none is passed over, as images passes over its empty answer. Before
// setAniDB (settled false) AniDB's, MAL's and ANN's aren't known yet: they
// count as asked, so nothing after them is fetched early for nothing.
func (ss *showSources) artAsked(src string, k artKind, settled bool) bool {
	switch src {
	case "tvdb":
		return ss.tvdbArt.fetches()
	case "tmdb":
		return ss.tmdbArt.fetches()
	case "plex":
		return ss.plexShow.fetches()
	case "anidb": // not ss.art: its one-poster list allocates
		return !settled || k == posterArt && ss.a != nil && ss.a.Picture != ""
	case "mal":
		return !settled || ss.mal.fetches()
	case "ann":
		return !settled || ss.ann.fetches()
	}
	return false
}

// plexSeasonOf: Plex's season n (noSeason: none), from its season list.
func (ss *showSources) plexSeasonOf(n int) sources.PlexSeason {
	if n == noSeason {
		return sources.PlexSeason{}
	}
	for _, s := range ss.plexSeasonList.get() {
		if s.Number == n {
			return s
		}
	}
	return sources.PlexSeason{}
}

// plexArt: Plex's pictures of the show. With ART_ALL_SOURCES, every one its
// /images lists too, except its copies of TheTVDB's and TheMovieDb's when
// that site is set up and in the kind's list: the same pictures at the same
// addresses, plus TheMovieDb posters in languages Kizuna leaves out on
// purpose. What stays is what only Plex has (fanart.tv, Gracenote, IMDb).
// The lists are made once: every kind and target asks for them.
func (ss *showSources) plexArt() sources.Art {
	if !ss.plexImages.fetches() {
		return ss.plexShow.get().Art
	}
	ss.plexAllArtOnce.Do(func() {
		a, im := ss.plexShow.get().Art, ss.plexImages.get()
		only := func(own []string, pics []sources.PlexPicture, k artKind) []string {
			out := slices.Clone(own) // the lazy's own list stays as it is
			order := k.order(ss)
			skipTVDB := ss.p.tvdb.Enabled() && slices.Contains(order, "tvdb")
			skipTMDB := ss.p.tmdb.Enabled() && slices.Contains(order, "tmdb")
			for _, x := range pics {
				if x.Source == "tvdb" && skipTVDB || x.Source == "tmdb" && skipTMDB {
					continue
				}
				out = append(out, x.URL)
			}
			return out
		}
		ss.plexAllArt = sources.Art{Posters: only(a.Posters, im.Posters, posterArt),
			Backgrounds: only(a.Backgrounds, im.Backgrounds, backgroundArt), Logos: a.Logos}
	})
	return ss.plexAllArt
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
