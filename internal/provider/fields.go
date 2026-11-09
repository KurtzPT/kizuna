package provider

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// showSources gives each online source's show-level data for one show.
// Every source is fetched at most once and only when a setting reaches it,
// so a fallback costs nothing while the sources before it have a value.
type showSources struct {
	p     *Provider
	aid   int
	a     *sources.AniDBAnime // nil = AniDB details not available
	tvdb  int
	tmdb  int
	movie bool // tmdb is a movie id
	// series: the show is the first AniDB entry of its TVDB/TMDB series (always
	// true in /tvdb mode). Series-level details are only used when it is.
	series bool

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

	// early: warmEarly started TheTVDB's and TheMovieDb's art (in earlyWG),
	// so warm leaves them out.
	early   bool
	earlyWG sync.WaitGroup
}

type text struct{ title, summary string }

// newSources: entry is aid's mapping entry, for the ANN and MAL ids when
// AniDB has no link to them or no details.
func (p *Provider) newSources(ctx context.Context, aid int, a *sources.AniDBAnime, entry mapping.Entry, tvdb, tmdb int, movie, series bool) *showSources {
	ss := &showSources{p: p, aid: aid, a: a, tvdb: tvdb, tmdb: tmdb, movie: movie, series: series, rec: report.RecorderFrom(ctx),
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
	if !series {
		ss.seasonSummaries(ctx, entry)
	}
	ss.tvdbArt.from(tvdb, p.tvdb.Enabled(), func() (sources.Art, error) { return p.tvdb.Art(ctx, tvdb) })
	ss.tmdbArt.from(tmdb, p.tmdb.Enabled(), func() (sources.Art, error) { return p.tmdb.Art(ctx, tmdb, movie) })
	ss.seasonArtOf = func(src string, n, sid int) (sources.Art, error) {
		if src == "tvdb" {
			return p.tvdb.SeasonArt(ctx, tvdb, n, sid)
		}
		return p.tmdb.SeasonArt(ctx, tmdb, n)
	}
	ss.setAniDB(ctx, a, entry)
	return ss
}

// setAniDB sets AniDB's details (nil = none) and the ANN and MAL entries
// they link to (else the mapping's). A /tvdb build sets them once AniDB has
// answered: only after that may the sources be asked anything but art
// (warmEarly).
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
	ss.ann.f, ss.mal.f = nil, nil
	if annID != 0 { // the closures only when used: they allocate
		ss.ann.from(annID, true, func() (sources.ANNAnime, error) { return p.ann.Anime(ctx, annID) })
	}
	if malID != 0 {
		ss.mal.from(malID, p.mal.Enabled(), func() (sources.MALAnime, error) { return p.mal.Anime(ctx, malID) })
	}
}

// seasonSummaries sets up the season summaries of an entry that doesn't
// start its series: the TheTVDB and TheMovieDb seasons its mapping puts it
// in. Absolute order (-1), specials (0) or no mapped season: none.
func (ss *showSources) seasonSummaries(ctx context.Context, entry mapping.Entry) {
	p := ss.p
	if n, ok := mappedSeason(entry.TVDB); ok {
		ss.tvdbSeason.from(ss.tvdb, p.tvdb.Enabled(), func() (string, error) { return p.tvdb.SeasonOverview(ctx, ss.tvdb, n) })
	}
	if n, ok := mappedSeason(entry.TMDB); ok && !ss.movie {
		ss.tmdbSeason.from(ss.tmdb, p.tmdb.Enabled(), func() (string, error) { return p.tmdb.SeasonOverview(ctx, ss.tmdb, n) })
	}
}

func mappedSeason(pl mapping.Placement) (int, bool) {
	return pl.Season, pl.Mapped && pl.Season > 0
}

// failed names the sources fill fetched that failed. A source that doesn't
// have the show (404) or isn't configured hasn't failed.
func (ss *showSources) failed() []string {
	var out []string
	for _, l := range []struct {
		name string
		lazy interface{ failed() bool }
	}{
		{"tvdb text", &ss.tvdbText}, {"tmdb text", &ss.tmdbText}, {"tvdb art", &ss.tvdbArt}, {"tmdb art", &ss.tmdbArt},
		{"tvdb details", &ss.tvdbDetails}, {"tmdb details", &ss.tmdbDetails}, {"tvdb season", &ss.tvdbSeason},
		{"tmdb season", &ss.tmdbSeason}, {"mal", &ss.mal}, {"ann", &ss.ann},
	} {
		if l.lazy.failed() {
			out = append(out, l.name)
		}
	}
	return append(out, ss.seasonsFailed()...)
}

func (ss *showSources) title(src string) string {
	switch src {
	case "anidb":
		if titles := ss.p.showTitles(ss.aid, ss.a); len(titles) > 0 {
			return ss.p.showTitle(titles)
		}
	case "tvdb":
		return ss.tvdbText.get().title
	case "tmdb":
		return ss.tmdbText.get().title
	case "ann":
		return ss.ann.get().Title
	}
	return ""
}

// summary: TVDB and TMDB TV overviews describe the whole series (its first
// season, mostly), so a later entry gets its own season's instead.
func (ss *showSources) summary(src string) string {
	switch src {
	case "anidb":
		if ss.a != nil {
			return ss.a.Description
		}
	case "tvdb":
		if !ss.series {
			return ss.tvdbSeason.get()
		}
		return ss.tvdbText.get().summary
	case "tmdb":
		if !ss.series && !ss.movie {
			return ss.tmdbSeason.get()
		}
		return ss.tmdbText.get().summary
	case "mal":
		return ss.mal.get().Synopsis
	case "ann":
		return ss.ann.get().Summary
	}
	return ""
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
	}
	return sources.Art{}
}

// details returns a source's genres, people, studio, rating and date.
// TVDB and TMDB TV data describe the whole series, so they are skipped
// unless this show starts it; a TMDB movie is always its own entry.
func (ss *showSources) details(src string) sources.Details {
	switch src {
	case "anidb":
		return ss.anidbDetails()
	case "tvdb":
		if ss.series {
			return ss.tvdbDetails.get()
		}
	case "tmdb":
		if ss.series || ss.movie {
			return ss.tmdbDetails.get()
		}
	case "mal":
		return ss.mal.get().Details
	case "ann": // per entry, like AniDB: no first-entry guard needed
		return ss.ann.get().Details
	}
	return sources.Details{}
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

// pickDetails returns the first source's details that has the field.
func (ss *showSources) pickDetails(order []string, has func(sources.Details) bool) (sources.Details, string) {
	for _, src := range order {
		if d := ss.details(src); has(d) {
			return d, src
		}
	}
	return sources.Details{}, ""
}

// fill sets every source-selectable field from the configured source orders.
// A value already on s is kept only when no source has one. At debug, one
// line per show says which source supplied each field ("-" = none). Whether
// a source failed is told by finish, after fillSeasons.
func (ss *showSources) fill(s *Show) {
	cfg := ss.p.cfg
	done := ss.rec.Step("Show fields and artwork")
	ss.warm()
	debug := debugOn(context.Background())
	var used []any // the debug line, built only when it is logged
	if debug {
		used = []any{"root", s.Root, "first_entry", ss.series}
	}
	note := func(field, src string) {
		if debug {
			used = append(used, field, cmp.Or(src, "-"))
		}
	}

	title, src := pick(cfg.TitleSources, ss.title)
	s.Title = firstOf(title, s.Title)
	note("title", src)
	ss.recordField("Title", cfg.TitleSources, src, false, ss.title)
	summary, src := pick(cfg.SummarySources, ss.summary)
	s.Summary = firstOf(summary, s.Summary)
	note("summary", src)
	ss.recordField("Summary", cfg.SummarySources, src, false, ss.summary)
	detail := func(name string, order []string, src string, describe func(sources.Details) string) {
		ss.recordField(name, order, src, false, func(src string) string { return describe(ss.details(src)) })
	}

	d, src := ss.pickDetails(cfg.GenreSources, func(d sources.Details) bool { return len(d.Genres) > 0 })
	s.Genres = d.Genres
	note("genres", src)
	detail("Genres", cfg.GenreSources, src, func(d sources.Details) string { return strings.Join(d.Genres, ", ") })
	d, src = ss.pickDetails(cfg.CastSources, func(d sources.Details) bool { return len(d.Cast) > 0 })
	s.Roles = uniqueCast(d.Cast)
	note("cast", src)
	detail("Cast", cfg.CastSources, src, func(d sources.Details) string { return describeCast(d.Cast) })
	d, src = ss.pickDetails(cfg.StudioSources, func(d sources.Details) bool { return d.Studio != "" })
	s.Studio = d.Studio
	note("studio", src)
	detail("Studio", cfg.StudioSources, src, func(d sources.Details) string { return d.Studio })
	d, src = ss.pickDetails(cfg.CrewSources, func(d sources.Details) bool { return len(d.Directors)+len(d.Writers) > 0 })
	s.Directors, s.Writers = d.Directors, d.Writers
	note("crew", src)
	detail("Crew", cfg.CrewSources, src, describeCrew)
	// Content rating: a listed country (or a country-less rating) from any
	// source first; only then a rating from some other country.
	d, src = ss.pickDetails(cfg.RatingSources, func(d sources.Details) bool { return d.Rating(cfg.RatingCountries) != "" })
	s.ContentRating = d.Rating(cfg.RatingCountries)
	if s.ContentRating == "" {
		d, src = ss.pickDetails(cfg.RatingSources, func(d sources.Details) bool { return d.AnyRating() != "" })
		s.ContentRating = d.AnyRating()
	}
	note("rating", src)
	detail("Content rating", cfg.RatingSources, src, func(d sources.Details) string {
		return cmp.Or(d.Rating(cfg.RatingCountries), d.AnyRating())
	})
	d, src = ss.pickDetails(cfg.DateSources, func(d sources.Details) bool { return d.Date != "" })
	s.Date = firstOf(fullDate(d.Date), s.Date)
	note("date", src)
	detail("First aired", cfg.DateSources, src, func(d sources.Details) string { return d.Date })

	art, by := ss.pickArt(ss.show, "", debug || ss.rec != nil)
	s.Art = art
	note("posters", by[posterArt])
	note("backgrounds", by[backgroundArt])
	note("logos", by[logoArt])
	if debug {
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

// warm fetches, in parallel, everything fill will ask for whatever the
// answers: per field, the first source in its order that needs a fetch,
// past the sources whose answer is at hand and empty (AniDB's without
// details, TheTVDB's details for a later entry...), which fill passes over
// too (later fallbacks are fetched only when reached); and the artwork used,
// the Plex seasons' too (every listed source in "all" mode, otherwise each
// image type's first choice). Each source is warmed once. The lazy values
// make fill's and fillSeasons' own calls free afterwards.
func (ss *showSources) warm() {
	cfg := ss.p.cfg
	var jobs []func()
	var asked [10]warmer // the lazies warmed so far: one job each
	na := 0
	add := func(w warmer) {
		if w == nil || slices.Contains(asked[:na], w) {
			return
		}
		if na < len(asked) {
			asked[na] = w
			na++
		}
		jobs = append(jobs, w.warm)
	}
	// first: the first source of order that would be fetched, unless a
	// source before it has the field at hand.
	first := func(order []string, of func(string) warmer, has func(string) bool) {
		for _, src := range order {
			if w := of(src); w != nil && w.fetches() {
				add(w)
				return
			}
			if has(src) {
				return
			}
		}
	}
	first(cfg.TitleSources, ss.titleLazy, func(src string) bool { return ss.title(src) != "" })
	first(cfg.SummarySources, ss.summaryLazy, func(src string) bool { return ss.summary(src) != "" })
	for _, f := range [...]struct {
		order []string
		has   func(sources.Details) bool
	}{
		{cfg.GenreSources, func(d sources.Details) bool { return len(d.Genres) > 0 }},
		{cfg.CastSources, func(d sources.Details) bool { return len(d.Cast) > 0 }},
		{cfg.StudioSources, func(d sources.Details) bool { return d.Studio != "" }},
		{cfg.CrewSources, func(d sources.Details) bool { return len(d.Directors)+len(d.Writers) > 0 }},
		{cfg.RatingSources, func(d sources.Details) bool { return d.Rating(cfg.RatingCountries) != "" }},
		{cfg.DateSources, func(d sources.Details) bool { return d.Date != "" }},
	} {
		first(f.order, ss.detailsLazy, func(src string) bool { return f.has(ss.details(src)) })
	}
	jobs = ss.warmArt(jobs, ss.notEarly)
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Go(job)
	}
	wg.Wait()
	ss.earlyWG.Wait()
}

// warmEarly starts warming the art of the sources that don't depend on
// AniDB's details, TheTVDB's and TheMovieDb's, before those are in
// (setAniDB). Set the targets first; warm waits for it.
func (ss *showSources) warmEarly() {
	ss.early = true
	for _, job := range ss.warmArt(nil, earlySource) {
		ss.earlyWG.Go(job)
	}
}

func earlySource(src string) bool { return src == "tvdb" || src == "tmdb" }

// notEarly: a source warmEarly didn't warm.
func (ss *showSources) notEarly(src string) bool { return !ss.early || !earlySource(src) }

// warmer is a lazy value warm can fetch ahead.
type warmer interface {
	fetches() bool // getting it downloads something
	warm()
}

// titleLazy, summaryLazy, detailsLazy: what title, summary and details ask
// src for, when that is a fetch (nil: the answer is at hand, AniDB's, or
// none). Keep them in step with those three.
func (ss *showSources) titleLazy(src string) warmer {
	switch src {
	case "tvdb":
		return &ss.tvdbText
	case "tmdb":
		return &ss.tmdbText
	case "ann":
		return &ss.ann
	}
	return nil
}

func (ss *showSources) summaryLazy(src string) warmer {
	switch src {
	case "tvdb":
		if !ss.series {
			return &ss.tvdbSeason
		}
		return &ss.tvdbText
	case "tmdb":
		if !ss.series && !ss.movie {
			return &ss.tmdbSeason
		}
		return &ss.tmdbText
	case "mal":
		return &ss.mal
	case "ann":
		return &ss.ann
	}
	return nil
}

func (ss *showSources) detailsLazy(src string) warmer {
	switch src {
	case "tvdb":
		if ss.series {
			return &ss.tvdbDetails
		}
	case "tmdb":
		if ss.series || ss.movie {
			return &ss.tmdbDetails
		}
	case "mal":
		return &ss.mal
	case "ann":
		return &ss.ann
	}
	return nil
}

// pick returns the first non-empty value in source order, and its source.
func pick(order []string, get func(src string) string) (string, string) {
	for _, src := range order {
		if v := get(src); v != "" {
			return v, src
		}
	}
	return "", ""
}

// images joins image URLs in source order (all=true) or returns the first
// source that has any (all=false). get gives a source's images in two parts
// (a season's and its series', artParts), both used. Duplicates are
// dropped. With count, the second value counts images per source, e.g.
// "tmdb:12,tvdb:5".
func images(order []string, all, count bool, get func(src string) (a, b []string)) ([]string, string) {
	var out, counts []string
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
			counts = append(counts, fmt.Sprintf("%s:%d", src, n))
		}
		if !all && len(out) > 0 {
			break
		}
	}
	return out, strings.Join(counts, ",")
}

// lazy computes f once, on first use; safe for concurrent callers. Without
// f it gives the zero value.
type lazy[T any] struct {
	once sync.Once
	f    func() (T, error)
	v    T
	err  error
}

func (l *lazy[T]) get() T {
	l.once.Do(func() {
		if l.f != nil {
			l.v, l.err = l.f()
		}
	})
	return l.v
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

// failed: f ran and failed, other than with a 404. Call it only after the
// get calls are done.
func (l *lazy[T]) failed() bool { return l.err != nil && !fetch.IsNotFound(l.err) }

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
