package provider

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"kizuna/internal/config"
	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// showSources gives each online source's show-level data for one show.
// Every source is fetched at most once and only for a setting that lists
// it. TheTVDB's and TheMovieDb's values are fetched ahead wherever a setting
// lists them (a few small requests); the others only when the sources
// before them have no value, so such a fallback costs nothing.
type showSources struct {
	p     *Provider
	aid   int
	a     *sources.AniDBAnime // nil = AniDB details not available
	tvdb  int
	tmdb  int
	movie bool // tmdb is a movie id
	// first: the show is the first AniDB entry of its series on TheTVDB and
	// on TheMovieDb (both true in /tvdb mode, tmdb for a movie). A site's
	// series-level data is only used when it is there.
	first firstEntry

	tvdbText, tmdbText lazy[text]
	// tvdbSeason, tmdbSeason: the summary of the season a later entry sits
	// in (set only when it isn't the series' first and has one).
	tvdbSeason, tmdbSeason   lazy[string]
	tvdbArt, tmdbArt         lazy[sources.Art]
	tvdbDetails, tmdbDetails lazy[sources.Details]
	anidbOnce                sync.Once // anidbDetails: AniDB's, built from a once, not per field
	anidbDet                 sources.Details
	mal                      lazy[sources.MALAnime]
	ann                      lazy[sources.ANNAnime]

	// Plex's metadata service: plexRef is what it is asked for (a series by
	// its TheTVDB id, a movie by its TheMovieDb or IMDb id); plexShow its
	// title, summary, details and pictures (one request), plexSeasonList each
	// season's summary and pictures (one request), plexImages every
	// picture it lists (ART_ALL_SOURCES only). plexSeasonAt: the season a later
	// entry sits in on TheTVDB, for its summary (noSeason = none).
	plexRef        sources.PlexRef
	plexShow       lazy[sources.PlexShow]
	plexSeasonList lazy[[]sources.PlexSeason]
	plexImages     lazy[sources.PlexImages]
	plexSeasonAt   int
	// plexAllArt: plexArt's answer with ART_ALL_SOURCES, worked out once
	// (setPlex doesn't reset it: it replaces only what wasn't used yet).
	plexAllArtOnce sync.Once
	plexAllArt     sources.Art

	// show, seasons: what the show's and each Plex season's art is of
	// (art.go); set by the build before fill.
	show    artTarget
	seasons []plexSeason
	// seasonArts: TheTVDB and TheMovieDb season art, each fetched once on
	// first use, with seasonArtOf; TheTVDB's by the season ids (sid) its
	// series record gave with tvdbDetails (tvdbSeasonIDs: read it only after
	// tvdbDetails.get).
	tvdbSeasonIDs map[int]int
	seasonMu      sync.Mutex
	seasonArts    []*seasonFetch
	seasonArtOf   func(src string, n, sid int) (sources.Art, error)
	// tmdbPosters: TheMovieDb's season posters, when its episode list
	// brought them (tmdbListed: a /tvdb build, whose list has every season).
	tmdbPosters map[int][]string
	tmdbListed  bool

	rec *report.Recorder // match report of the build; nil = none

	// early: warmEarly started TheTVDB's and TheMovieDb's show fields and
	// art, and Plex's art (in earlyWG), so warm leaves them out.
	early   bool
	earlyWG group
}

type text struct{ title, summary string }

// firstEntry: whether a show is the first AniDB entry of its series on each
// site (mapping.Lists.StartsShow).
type firstEntry struct{ tvdb, tmdb bool }

// newSources: entry is aid's mapping entry, for the season summaries. AniDB's
// details, and the ANN and MAL entries, come with setAniDB, called once per
// build.
func (p *Provider) newSources(ctx context.Context, aid int, entry mapping.Entry, tvdb, tmdb int, movie bool, first firstEntry) *showSources {
	ss := &showSources{p: p, aid: aid, tvdb: tvdb, tmdb: tmdb, movie: movie, first: first, rec: report.RecorderFrom(ctx),
		show: seriesTarget}
	ss.tvdbDetails.from(tvdb, p.tvdb.Enabled(), func() (sources.Details, error) {
		d, ids, err := p.tvdb.DetailsAndSeasons(ctx, tvdb)
		ss.tvdbSeasonIDs = ids
		return d, err
	})
	ss.tmdbDetails.from(tmdb, p.tmdb.Enabled(), func() (sources.Details, error) { return p.tmdb.Details(ctx, tmdb, movie) })
	ss.tvdbText.from(tvdb, p.tvdb.Enabled(), func() (text, error) {
		n, o, err := p.tvdb.Overview(ctx, tvdb)
		return text{n, o}, err
	})
	ss.tmdbText.from(tmdb, p.tmdb.Enabled(), func() (text, error) {
		n, o, err := p.tmdb.Text(ctx, tmdb, movie)
		return text{n, o}, err
	})
	ss.seasonSummaries(ctx, entry)
	ss.tvdbArt.from(tvdb, p.tvdb.Enabled(), func() (sources.Art, error) { return p.tvdb.Art(ctx, tvdb) })
	ss.tmdbArt.from(tmdb, p.tmdb.Enabled(), func() (sources.Art, error) { return p.tmdb.Art(ctx, tmdb, movie) })
	ss.seasonArtOf = func(src string, n, sid int) (sources.Art, error) {
		if src == "tvdb" {
			return p.tvdb.SeasonArt(ctx, tvdb, n, sid)
		}
		return p.tmdb.SeasonArt(ctx, tmdb, n)
	}
	ss.plexSeasonAt = noSeason
	if p.plex.Enabled() { // the closures only when used: they allocate
		switch {
		case movie:
			ss.setPlex(ctx, sources.PlexMovie(tmdb))
		case tvdb > 0:
			ss.setPlex(ctx, sources.PlexSeries(tvdb))
			if n, ok := mappedSeason(entry.TVDB); ok && !first.tvdb {
				ss.plexSeasonAt = n
			}
		}
	}
	return ss
}

// setPlex sets what Plex's service is asked for (an invalid ref: nothing),
// replacing what was set before, which must not have been used yet.
func (ss *showSources) setPlex(ctx context.Context, r sources.PlexRef) {
	p := ss.p
	if !r.Valid() {
		return
	}
	ss.plexRef = r
	ss.plexShow, ss.plexSeasonList, ss.plexImages = lazy[sources.PlexShow]{}, lazy[[]sources.PlexSeason]{}, lazy[sources.PlexImages]{}
	ss.plexShow.from(1, true, func() (sources.PlexShow, error) { return p.plex.Show(ctx, r) })
	if !r.Movie {
		ss.plexSeasonList.from(1, true, func() ([]sources.PlexSeason, error) { return p.plex.Seasons(ctx, r) })
	}
	if p.cfg.ArtAllSources {
		ss.plexImages.from(1, true, func() (sources.PlexImages, error) { return p.plex.Images(ctx, r) })
	}
}

// plexFirst: Plex's show-level data describes this show: a movie is its own
// entry; a series' data is used, like TheTVDB's, only on the entry that
// starts it there.
func (ss *showSources) plexFirst() bool { return ss.plexRef.Movie || ss.first.tvdb }

// setAniDB sets AniDB's details (nil = none) and the ANN and MAL entries
// they link to (else the mapping's), once per build. A /tvdb build sets them
// once AniDB has answered: before that, only what doesn't depend on its
// answer is asked (warmEarly: TheTVDB's and TheMovieDb's values, Plex's art).
func (ss *showSources) setAniDB(ctx context.Context, a *sources.AniDBAnime, entry mapping.Entry) {
	p := ss.p
	ss.a = a
	annID, malID := entry.ANN, entry.MAL
	if a != nil && len(a.ANNIDs) > 0 {
		annID = a.ANNIDs[0]
	}
	if a != nil && len(a.MALIDs) > 0 {
		malID = a.MALIDs[0]
	}
	if annID != 0 { // the closures only when used: they allocate
		ss.ann.from(annID, true, func() (sources.ANNAnime, error) { return p.ann.Anime(ctx, annID) })
	}
	if malID != 0 {
		ss.mal.from(malID, p.mal.Enabled(), func() (sources.MALAnime, error) { return p.mal.Anime(ctx, malID) })
	}
	// A movie only AniDB's type says is one (no TheMovieDb movie id) is
	// asked for as a movie, by its IMDb id: known only now. Not in a /tvdb
	// build, whose Plex lazies warmEarly may already use (a series anyway).
	if p.plex.Enabled() && !ss.movie && !ss.early && a != nil && a.IsMovie() {
		ss.setPlex(ctx, sources.PlexIMDbMovie(entry.IMDB)) // none without an IMDb id: the series stays
	}
}

// seasonSummaries sets up the season summaries of an entry that doesn't
// start its series on a site: the TheTVDB and TheMovieDb seasons its mapping
// puts it in. Absolute order (-1), specials (0) or no mapped season: none.
func (ss *showSources) seasonSummaries(ctx context.Context, entry mapping.Entry) {
	p := ss.p
	if n, ok := mappedSeason(entry.TVDB); ok && !ss.first.tvdb {
		ss.tvdbSeason.from(ss.tvdb, p.tvdb.Enabled(), func() (string, error) { return p.tvdb.SeasonOverview(ctx, ss.tvdb, n) })
	}
	if n, ok := mappedSeason(entry.TMDB); ok && !ss.first.tmdb && !ss.movie {
		ss.tmdbSeason.from(ss.tmdb, p.tmdb.Enabled(), func() (string, error) { return p.tmdb.SeasonOverview(ctx, ss.tmdb, n) })
	}
}

func mappedSeason(pl mapping.Placement) (int, bool) {
	return pl.Season, pl.Mapped && pl.Season > 0
}

// failed names the sources fill and fillSeasons read that failed. A source
// that doesn't have the show (404) or isn't configured hasn't failed, nor
// has one only fetched ahead (warm: TheTVDB's and TheMovieDb's) whose value
// no field reached: nothing is missing without it, so the show is not
// rebuilt for it.
func (ss *showSources) failed() []string {
	var out []string
	ss.eachLazy(func(name string, l lazyState) {
		if l.failed() {
			out = append(out, name)
		}
	})
	return append(out, ss.seasonsFailed()...)
}

// lazyState is what failed and unread ask of a lazy value, whatever its
// type.
type lazyState interface {
	failed() bool
	unread()
}

// eachLazy calls f with each lazy value and its name. An array: failed runs
// at every build, with the reports off too.
func (ss *showSources) eachLazy(f func(name string, l lazyState)) {
	for _, l := range [...]struct {
		name string
		lazy lazyState
	}{
		{"tvdb text", &ss.tvdbText}, {"tmdb text", &ss.tmdbText}, {"tvdb art", &ss.tvdbArt}, {"tmdb art", &ss.tmdbArt},
		{"tvdb details", &ss.tvdbDetails}, {"tmdb details", &ss.tmdbDetails}, {"tvdb season", &ss.tvdbSeason},
		{"tmdb season", &ss.tmdbSeason}, {"mal", &ss.mal}, {"ann", &ss.ann}, {"plex", &ss.plexShow},
		{"plex seasons", &ss.plexSeasonList}, {"plex images", &ss.plexImages},
	} {
		f(l.name, l.lazy)
	}
}

// title, summary and detailsOf give src's value for the show, and the lazy
// value it is read from (nil: none, or at hand, as AniDB's are). With
// ahead (warm) they fetch nothing: the value only when it is at hand, so
// warm can fetch that lazy value ahead. One function for both, so warm
// fetches exactly what fill reads.
//
// title: TVDB and TMDB TV names are the series' ("One Piece" for Film Red),
// so each is skipped unless this show starts the series on that site; a
// TMDB movie is its own entry. Sites have no per-season name to stand in.
func (ss *showSources) title(src string, ahead bool) (string, warmer) {
	switch src {
	case "anidb":
		if titles := ss.p.showTitles(ss.aid, ss.a); len(titles) > 0 {
			return ss.p.showTitle(titles), nil
		}
	case "tvdb":
		if ss.first.tvdb {
			return ss.tvdbText.value(ahead).title, &ss.tvdbText
		}
	case "tmdb":
		if ss.first.tmdb {
			return ss.tmdbText.value(ahead).title, &ss.tmdbText
		}
	case "plex":
		if ss.plexFirst() {
			return ss.plexShow.value(ahead).Title, &ss.plexShow
		}
	case "ann":
		return ss.ann.value(ahead).Title, &ss.ann
	}
	return "", nil
}

// summary: TVDB and TMDB TV overviews describe the whole series (its first
// season, mostly), so a later entry there gets its own season's instead.
func (ss *showSources) summary(src string, ahead bool) (string, warmer) {
	switch src {
	case "anidb":
		if ss.a != nil {
			return ss.a.Description, nil
		}
	case "tvdb":
		if !ss.first.tvdb {
			return ss.tvdbSeason.value(ahead), &ss.tvdbSeason
		}
		return ss.tvdbText.value(ahead).summary, &ss.tvdbText
	case "tmdb":
		if !ss.first.tmdb {
			return ss.tmdbSeason.value(ahead), &ss.tmdbSeason
		}
		return ss.tmdbText.value(ahead).summary, &ss.tmdbText
	case "mal":
		return ss.mal.value(ahead).Synopsis, &ss.mal
	case "plex":
		if !ss.plexFirst() { // its season's, from the season list
			if ss.plexSeasonAt == noSeason {
				return "", nil
			}
			if ahead {
				return "", &ss.plexSeasonList
			}
			return ss.plexSeasonOf(ss.plexSeasonAt).Summary, &ss.plexSeasonList
		}
		return ss.plexShow.value(ahead).Summary, &ss.plexShow
	case "ann":
		return ss.ann.value(ahead).Summary, &ss.ann
	}
	return "", nil
}

func (ss *showSources) art(src string) sources.Art {
	switch src {
	case "anidb":
		if ss.a != nil && ss.a.Picture != "" {
			return sources.Art{Posters: []string{ss.a.Picture}}
		}
	case "tvdb":
		return ss.tvdbArt.get()
	case "tmdb":
		return ss.tmdbArt.get()
	case "mal":
		if m := ss.mal.get(); m.Poster != "" {
			return sources.Art{Posters: []string{m.Poster}}
		}
	case "ann":
		if n := ss.ann.get(); n.Poster != "" {
			return sources.Art{Posters: []string{n.Poster}}
		}
	case "plex":
		return ss.plexArt()
	}
	return sources.Art{}
}

// details returns a source's genres, people, studio, rating and date.
func (ss *showSources) details(src string) sources.Details {
	d, _ := ss.detailsOf(src, false)
	return d
}

// detailsOf: details and the lazy value they are read from (see title).
// TVDB and TMDB TV data describe the whole series, so each is skipped
// unless this show starts it on that site; a TMDB movie is always its own
// entry.
func (ss *showSources) detailsOf(src string, ahead bool) (sources.Details, warmer) {
	switch src {
	case "anidb":
		return ss.anidbDetails(), nil
	case "tvdb":
		if ss.first.tvdb {
			return ss.tvdbDetails.value(ahead), &ss.tvdbDetails
		}
	case "tmdb":
		if ss.first.tmdb {
			return ss.tmdbDetails.value(ahead), &ss.tmdbDetails
		}
	case "mal":
		return ss.mal.value(ahead).Details, &ss.mal
	case "plex":
		if ss.plexFirst() {
			return ss.plexShow.value(ahead).Details, &ss.plexShow
		}
	case "ann": // per entry, like AniDB: no first-entry guard needed
		return ss.ann.value(ahead).Details, &ss.ann
	}
	return sources.Details{}, nil
}

// anidbDetails: AniDB's details, built from a on first use. A sync.Once,
// not a lazy: a lazy's closure would cost every build an allocation.
func (ss *showSources) anidbDetails() sources.Details {
	ss.anidbOnce.Do(func() {
		if ss.a != nil {
			ss.anidbDet = ss.a.Details()
		}
	})
	return ss.anidbDet
}

// showField is one show field set from a source order. showFields drives
// fill, warm and siteFields, so the sources warm fetches ahead are the ones
// fill reads: a source no list names is asked for nothing. A static table
// of plain functions, so a build allocates nothing for it.
type showField struct {
	name, key string // in the match report; in the debug line
	order     func(c *config.Config) []string
	// of gives src's value and the lazy value it is read from (title).
	of  func(ss *showSources, src string, ahead bool) (fieldValue, warmer)
	has func(v fieldValue, c *config.Config) bool // v has the field
	set func(s *Show, v fieldValue, c *config.Config)
	// describe: what v offers, for the match report (the cast's is
	// recordCast's: nil).
	describe func(v fieldValue, c *config.Config) string
}

// fieldValue is a source's value for a field: a text (title, summary) or
// its details.
type fieldValue struct {
	text string
	d    sources.Details
}

func titleValue(ss *showSources, src string, ahead bool) (fieldValue, warmer) {
	t, l := ss.title(src, ahead)
	return fieldValue{text: t}, l
}

func summaryValue(ss *showSources, src string, ahead bool) (fieldValue, warmer) {
	t, l := ss.summary(src, ahead)
	return fieldValue{text: t}, l
}

func detailsValue(ss *showSources, src string, ahead bool) (fieldValue, warmer) {
	d, l := ss.detailsOf(src, ahead)
	return fieldValue{d: d}, l
}

func hasText(v fieldValue, _ *config.Config) bool          { return v.text != "" }
func textOf(v fieldValue, _ *config.Config) string         { return v.text }
func hasRating(v fieldValue, c *config.Config) bool        { return v.d.Rating(c.RatingCountries) != "" }
func hasAnyRating(v fieldValue, _ *config.Config) bool     { return v.d.AnyRating() != "" }
func describeCrewOf(v fieldValue, _ *config.Config) string { return describeCrew(v.d) }

// The keys fill gives explicit code to: the cast-photo swap and the
// cast's report line, the rating's second pass.
const (
	castKey   = "cast"
	ratingKey = "rating"
)

// showFields, in fill's order (the match report's and the debug line's).
// The content rating's has is its first pass (fill).
var showFields = [...]showField{
	{name: "Title", key: "title", order: func(c *config.Config) []string { return c.TitleSources },
		of: titleValue, has: hasText,
		set:      func(s *Show, v fieldValue, _ *config.Config) { s.Title = firstOf(v.text, s.Title) },
		describe: textOf},
	{name: "Summary", key: "summary", order: func(c *config.Config) []string { return c.SummarySources },
		of: summaryValue, has: hasText,
		set:      func(s *Show, v fieldValue, _ *config.Config) { s.Summary = firstOf(v.text, s.Summary) },
		describe: textOf},
	{name: "Genres", key: "genres", order: func(c *config.Config) []string { return c.GenreSources },
		of:       detailsValue,
		has:      func(v fieldValue, _ *config.Config) bool { return len(v.d.Genres) > 0 },
		set:      func(s *Show, v fieldValue, _ *config.Config) { s.Genres = v.d.Genres },
		describe: func(v fieldValue, _ *config.Config) string { return strings.Join(v.d.Genres, ", ") }},
	{name: "Cast", key: castKey, order: func(c *config.Config) []string { return c.CastSources },
		of:  detailsValue,
		has: func(v fieldValue, _ *config.Config) bool { return len(v.d.Cast) > 0 },
		set: func(s *Show, v fieldValue, _ *config.Config) { s.Roles = uniqueCast(v.d.Cast) }},
	{name: "Studio", key: "studio", order: func(c *config.Config) []string { return c.StudioSources },
		of:       detailsValue,
		has:      func(v fieldValue, _ *config.Config) bool { return v.d.Studio != "" },
		set:      func(s *Show, v fieldValue, _ *config.Config) { s.Studio = v.d.Studio },
		describe: func(v fieldValue, _ *config.Config) string { return v.d.Studio }},
	{name: "Crew", key: "crew", order: func(c *config.Config) []string { return c.CrewSources },
		of:       detailsValue,
		has:      func(v fieldValue, _ *config.Config) bool { return len(v.d.Directors)+len(v.d.Writers) > 0 },
		set:      func(s *Show, v fieldValue, _ *config.Config) { s.Directors, s.Writers = v.d.Directors, v.d.Writers },
		describe: describeCrewOf},
	{name: "Content rating", key: ratingKey, order: func(c *config.Config) []string { return c.RatingSources },
		of: detailsValue, has: hasRating,
		set: func(s *Show, v fieldValue, c *config.Config) { s.ContentRating = v.d.Rating(c.RatingCountries) },
		describe: func(v fieldValue, c *config.Config) string {
			return cmp.Or(v.d.Rating(c.RatingCountries), v.d.AnyRating())
		}},
	{name: "First aired", key: "date", order: func(c *config.Config) []string { return c.DateSources },
		of:       detailsValue,
		has:      func(v fieldValue, _ *config.Config) bool { return v.d.Date != "" },
		set:      func(s *Show, v fieldValue, _ *config.Config) { s.Date = firstOf(fullDate(v.d.Date), s.Date) },
		describe: func(v fieldValue, _ *config.Config) string { return v.d.Date }},
}

// pick returns f's first value in source order that has the field (has),
// and its source ("" = none).
func (ss *showSources) pick(f *showField, order []string, has func(fieldValue, *config.Config) bool) (fieldValue, string) {
	for _, src := range order {
		if v, _ := f.of(ss, src, false); has(v, &ss.p.cfg) {
			return v, src
		}
	}
	return fieldValue{}, ""
}

// fill sets every source-selectable field from the configured source orders.
// The title, summary and date already on s are kept when no source has one;
// the other fields and the art are replaced, so none left means empty. At
// debug, one line per show says which source supplied each field ("-" =
// none). Whether a source failed is told by finish, after fillSeasons.
func (ss *showSources) fill(s *Show) {
	cfg := &ss.p.cfg
	done := ss.rec.Step("Show fields and artwork")
	ss.warm()
	debug := debugOn(context.Background())
	var used []any // the debug line, built only when it is logged
	if debug {
		used = []any{"root", s.Root, "first_tvdb", ss.first.tvdb, "first_tmdb", ss.first.tmdb}
	}
	note := func(field, src string) {
		if debug {
			used = append(used, field, cmp.Or(src, "-"))
		}
	}

	for i := range showFields {
		f := &showFields[i]
		order := f.order(cfg)
		v, src := ss.pick(f, order, f.has)
		f.set(s, v, cfg)
		photos := -1 // the cast's: the swap didn't run
		switch f.key {
		case castKey:
			// CAST_PHOTOS_FROM_PLEX: Plex's photos for the people it has.
			// Plex's show is read only for a cast of another source, so
			// without one a Plex that failed leaves the show complete. Read
			// directly, not through details: the photos apply to later
			// entries too (plexFirst).
			if cfg.CastPhotosFromPlex && len(s.Roles) > 0 && src != "plex" && ss.plexShow.fetches() {
				photos = castPhotos(s.Roles, ss.plexShow.get().People)
			}
		case ratingKey:
			// Content rating: a listed country (or a country-less rating)
			// from any source first; only then a rating from some other
			// country.
			if s.ContentRating == "" {
				v, src = ss.pick(f, order, hasAnyRating)
				s.ContentRating = v.d.AnyRating()
			}
		}
		note(f.key, src)
		if f.key == castKey {
			ss.recordCast(order, src, s.Roles, photos)
		} else {
			ss.recordShowField(f, order, src)
		}
	}

	art, by := ss.pickArt(ss.show, "", debug || ss.rec != nil)
	s.Art = art
	if ss.rec != nil {
		s.artBy = &artSources{show: by}
	}
	if debug {
		note("posters", countsText(by[posterArt]))
		note("backgrounds", countsText(by[backgroundArt]))
		note("logos", countsText(by[logoArt]))
		slog.Debug("show fields: source used", used...)
	}
	why := ""
	if ss.rec != nil { // the step notes what failed so far; finish tells the rest
		if failed := ss.failed(); len(failed) > 0 {
			why = "failed: " + strings.Join(failed, ", ")
		}
	}
	done(why)
}

// finish reports whether every source fill and fillSeasons asked answered.
// When one failed (an error or the build's time running out), it is logged
// and noted in the match report, and the show is rebuilt soon instead of
// being kept an hour without it. Call it after fillSeasons.
func (ss *showSources) finish(s *Show) bool {
	failed := ss.failed()
	if len(failed) == 0 {
		return true
	}
	slog.Info("show sources failed; using the others, rebuilt in 2 min", "root", s.Root, "failed", failed)
	if ss.rec != nil {
		ss.rec.Check(report.Check{Level: report.LevelWarn, Title: "A source failed: " + strings.Join(failed, ", "), Cause: true,
			Text: "The other sources filled in. Kizuna builds the show again in 2 minutes.", Section: report.SectionFields})
	}
	return false
}

// warm fetches, in parallel, everything fill may ask for. TheTVDB's and
// TheMovieDb's values wherever a field lists them (siteFields, and their
// art in warmArt): a few small requests, fetched without waiting to see
// whether the sources before them have the field. Of the others (Plex,
// MAL, ANN), per field, the first in its order that needs a fetch, past the
// sources whose answer is at hand and empty (AniDB's without details...),
// which fill passes over too: they are slower or stricter services, asked
// later only when fill reaches them. The artwork likewise (warmArt), the
// Plex seasons' too. Each value is warmed once. The lazy values make fill's
// and fillSeasons' own calls free afterwards. What warm read doesn't count
// as read (unread): failed tells only what fill and fillSeasons read, so a
// value fetched ahead that no field needed can't fail the show.
func (ss *showSources) warm() {
	cfg := &ss.p.cfg
	var w warmSet
	if !ss.early { // else warmEarly started them
		ss.siteFields(&w)
	}
	// Per field, the first source of its order that would be fetched,
	// unless a source before it has the field at hand. TheTVDB's and
	// TheMovieDb's are siteFields' already.
	for i := range showFields {
		f := &showFields[i]
		for _, src := range f.order(cfg) {
			v, l := f.of(ss, src, true)
			if l != nil && l.fetches() {
				if !siteSource(src) {
					w.add(l)
				}
				break
			}
			if f.has(v, cfg) {
				break
			}
		}
	}
	w.jobs = ss.warmArt(w.jobs, ss.notEarly, true)
	var wg group
	for _, job := range w.jobs {
		wg.Go(job)
	}
	wg.Wait()
	ss.earlyWG.Wait()
	ss.unread()
}

// siteFields adds TheTVDB's and TheMovieDb's values that some field lists
// them for: title, summary (for a later entry, its season's) and details.
// One that can't apply (not the entry that starts the series on the site,
// no id, not set up) isn't asked for: fill wouldn't read it. With
// CAST_PHOTOS_FROM_PLEX, Plex's show too, whatever the lists say: its cast
// gives the photos. Not with CAST_SOURCES=none: there is no cast to put a
// photo on, and fill never reads it for that. /anidb must call this after
// setAniDB, because AniDB's answer can change which Plex item is meant (a
// movie by its IMDb id). /tvdb can call it first: there AniDB never changes
// it.
func (ss *showSources) siteFields(w *warmSet) {
	cfg := &ss.p.cfg
	if cfg.CastPhotosFromPlex && len(cfg.CastSources) > 0 {
		w.add(&ss.plexShow) // nothing to ask (no id, no token): add skips it
	}
	for _, src := range [...]string{"tvdb", "tmdb"} {
		for i := range showFields {
			if f := &showFields[i]; slices.Contains(f.order(cfg), src) {
				_, l := f.of(ss, src, true)
				w.add(l) // the details' fields share one: added once
			}
		}
	}
}

// warmSet is warm's jobs: one per lazy value added, then warmArt's.
type warmSet struct {
	jobs  []func()
	asked [12]warmer // the lazies added so far
	n     int
}

// add adds l's job, unless l is nil, fetches nothing or was added already.
func (w *warmSet) add(l warmer) {
	if l == nil || !l.fetches() || slices.Contains(w.asked[:w.n], l) {
		return
	}
	if w.n < len(w.asked) {
		w.asked[w.n] = l
		w.n++
	}
	w.jobs = append(w.jobs, l.warm)
}

// unread forgets which values were read so far: after warm, failed counts
// only what fill and fillSeasons read.
func (ss *showSources) unread() {
	ss.eachLazy(func(_ string, l lazyState) { l.unread() })
	ss.seasonMu.Lock()
	for _, f := range ss.seasonArts {
		f.read = false
	}
	ss.seasonMu.Unlock()
}

// warmEarly starts warming what doesn't depend on AniDB's details, before
// those are in (setAniDB): TheTVDB's and TheMovieDb's show fields and art,
// and Plex's art. Set the targets first; warm waits for it.
func (ss *showSources) warmEarly() {
	ss.early = true
	var w warmSet
	ss.siteFields(&w)
	for _, job := range ss.warmArt(w.jobs, earlySource, false) {
		ss.earlyWG.Go(job)
	}
}

func earlySource(src string) bool { return siteSource(src) || src == "plex" }

// siteSource: TheTVDB or TheMovieDb, whose values are fetched ahead
// wherever a setting lists them (warm).
func siteSource(src string) bool { return src == "tvdb" || src == "tmdb" }

// notEarly: a source warmEarly didn't warm.
func (ss *showSources) notEarly(src string) bool { return !ss.early || !earlySource(src) }

// warmer is a lazy value warm can fetch ahead.
type warmer interface {
	fetches() bool // getting it downloads something
	warm()
}

// images joins image URLs in source order (all=true) or returns the first
// source that has any (all=false). get gives a source's images in two parts
// (a season's and its series', artParts), both used. Duplicates are
// dropped. With count, the second value counts the images each source
// gave, in order.
func images(order []string, all, count bool, get func(src string) (a, b []string)) ([]string, []sourceCount) {
	var out []string
	var counts []sourceCount
	seen := map[string]bool{}
	for _, src := range order {
		n := 0
		a, b := get(src)
		for _, part := range [2][]string{a, b} {
			for _, u := range part {
				if u != "" && !seen[u] {
					seen[u] = true
					out = append(out, u)
					n++
				}
			}
		}
		if n > 0 && count {
			counts = append(counts, sourceCount{src, n})
		}
		if !all && len(out) > 0 {
			break
		}
	}
	return out, counts
}

// sourceCount is how many images a source gave.
type sourceCount struct {
	src string
	n   int
}

// countsText is the debug line's form of the counts: "tmdb:12,tvdb:5".
func countsText(counts []sourceCount) string {
	var b strings.Builder
	for i, c := range counts {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(c.src)
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(c.n))
	}
	return b.String()
}

// lazy computes f once, on first use, or ahead in a build's group (start);
// safe for concurrent callers. Without f it gives the zero value.
type lazy[T any] struct {
	once sync.Once
	f    func() (T, error)
	v    T
	err  error
	read atomic.Bool // get was called since the last unread (failed)
	ran  atomic.Bool // f ran: v is set (done)
	// panicked: the panic f ended with. Every later caller panics with it
	// too: f may have run on another goroutine than the one reading (start).
	panicked *fetch.GoroutinePanic
}

func (l *lazy[T]) get() T {
	l.read.Store(true)
	l.fetch()
	return l.v
}

// value is get, or with ahead the zero value, fetching nothing (title).
func (l *lazy[T]) value(ahead bool) T {
	if ahead {
		var zero T
		return zero
	}
	return l.get()
}

// fetch runs f, once, without counting as a read.
func (l *lazy[T]) fetch() {
	l.once.Do(func() {
		if l.f != nil {
			defer l.keepPanic()
			l.v, l.err = l.f()
			l.ran.Store(true)
		}
	})
	if l.panicked != nil {
		panic(l.panicked)
	}
}

// keepPanic, deferred in f's run, keeps its panic, with the stack it had
// there, for fetch to panic with on every caller.
func (l *lazy[T]) keepPanic() {
	if r := recover(); r != nil {
		l.panicked = fetch.AsGoroutinePanic(r)
		panic(l.panicked)
	}
}

// start fetches the value ahead, in g, before anything reads it: a fetch
// alone isn't a read (failed). Nothing to fetch: nothing starts.
func (l *lazy[T]) start(g *group) {
	if l.f != nil {
		g.goFetch(l)
	}
}

// done gives the value only when get has fetched it already: it never
// fetches, and doesn't count as a read (failed).
func (l *lazy[T]) done() (T, bool) {
	if !l.ran.Load() {
		var zero T
		return zero, false
	}
	return l.v, true
}

func (l *lazy[T]) fetches() bool { return l.f != nil }
func (l *lazy[T]) warm()         { l.get() }

// from sets f, unless the source can't have the show: no id for it (0), or
// not configured (on false).
func (l *lazy[T]) from(id int, on bool, f func() (T, error)) {
	if id != 0 && on {
		l.f = f
	}
}

// failed: f ran and failed, other than with a 404, and the value was read
// since the last unread. Call it only after the get calls are done.
func (l *lazy[T]) failed() bool { return l.read.Load() && l.err != nil && !fetch.IsNotFound(l.err) }

func (l *lazy[T]) unread() { l.read.Store(false) }

// uniqueCast drops repeated cast entries (same person, same character, as
// sources sometimes list them twice), keeping the first one's place and the
// first photo found.
func uniqueCast(cast []sources.Person) []sources.Person {
	type who struct{ name, role string }
	at := map[who]int{}
	var out []sources.Person
	for _, p := range cast {
		k := who{strings.ToLower(strings.TrimSpace(p.Name)), strings.ToLower(strings.TrimSpace(p.Role))}
		if i, ok := at[k]; ok {
			if out[i].Photo == "" {
				out[i].Photo = p.Photo
			}
			continue
		}
		at[k] = len(out)
		out = append(out, p)
	}
	return out
}

// castPhotos gives each person of cast the photo Plex has for them (plex:
// Plex's people for the show), paired by nameKey, and returns how many it
// gave. Name, character and order stay. A person Plex doesn't have, or has
// without a photo, keeps their own. Two Plex people with one key and
// different photos give none: which one is meant can't be told. The same
// person twice, on either side, is no conflict: an actor with two
// characters gets the photo on both rows.
func castPhotos(cast, plex []sources.Person) int {
	photo := map[string]string{} // key -> photo; "" = different photos
	addPhotos(photo, plex, nameKey)
	n := 0
	for i := range cast {
		if k := nameKey(cast[i].Name); k != "" && photo[k] != "" {
			cast[i].Photo = photo[k]
			n++
		}
	}
	return n
}

// addPhotos adds the photos of people to photo (key -> photo), each under
// its key (an empty key gets none). Two different photos under one key
// leave "": which one is meant can't be told, and a third doesn't decide.
// The same photo twice is no conflict.
func addPhotos(photo map[string]string, people []sources.Person, key func(string) string) {
	for _, p := range people {
		if p.Photo == "" {
			continue
		}
		k := key(p.Name)
		if k == "" {
			continue
		}
		if have, ok := photo[k]; ok && have != p.Photo {
			photo[k] = ""
			continue
		}
		photo[k] = p.Photo
	}
}

// nameAccents: the accented vowels romanised Japanese names use, without
// their accent. A fixed table: the standard library can't take accents off,
// and Kizuna has no dependencies.
var nameAccents = map[rune]rune{
	'ā': 'a', 'â': 'a', 'á': 'a', 'ī': 'i', 'î': 'i', 'í': 'i', 'ū': 'u', 'û': 'u', 'ú': 'u',
	'ē': 'e', 'ê': 'e', 'é': 'e', 'è': 'e', 'ō': 'o', 'ô': 'o', 'ó': 'o',
}

// nameFolds: the spellings of one long vowel, folded one after another, each
// over the whole name. Not in one pass (strings.NewReplacer): that turns
// "kouhei" into "kohei" but "kohei" into "koei", and the two must pair.
var nameFolds = [...][2]string{{"ou", "o"}, {"oo", "o"}, {"oh", "o"}, {"ow", "o"}, {"uu", "u"}}

// nameKey is what castPhotos and the crew photos pair a name by:
// lower-cased, accents off, only letters and spaces, long vowels folded,
// words sorted. The sources spell one person differently ("Yamadera
// Kouichi", "Koichi Yamadera", "Kōichi Yamadera"); a key that close is
// exact, never fuzzy: nearly the same name is often someone else. "" never
// pairs. Each rune is lowered as strings.ToLower would, without its copy of
// the name: every crew name of a build gets a key.
func nameKey(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		r = unicode.ToLower(r)
		if plain, ok := nameAccents[r]; ok {
			r = plain
		}
		switch {
		case unicode.IsSpace(r): // a tab or a no-break space splits words too
			b.WriteByte(' ')
		case unicode.IsLetter(r):
			b.WriteRune(r)
		}
	}
	s := b.String()
	for _, f := range nameFolds {
		s = strings.ReplaceAll(s, f[0], f[1])
	}
	var most [8]string // most names have fewer words: no slice to allocate
	words, size := most[:0], 0
	for w := range strings.FieldsSeq(s) {
		words, size = append(words, w), size+len(w)+1
	}
	if size-1 == len(s) && slices.IsSorted(words) {
		return s // already the key: one space between sorted words
	}
	slices.Sort(words)
	return strings.Join(words, " ")
}
