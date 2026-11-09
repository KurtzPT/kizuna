package provider

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
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

	rec *report.Recorder // match report of the build; nil = none
}

type text struct{ title, summary string }

// newSources: entry is aid's mapping entry, for the ANN and MAL ids when
// AniDB has no link to them or no details.
func (p *Provider) newSources(ctx context.Context, aid int, a *sources.AniDBAnime, entry mapping.Entry, tvdb, tmdb int, movie, series bool) *showSources {
	ss := &showSources{p: p, aid: aid, a: a, tvdb: tvdb, tmdb: tmdb, movie: movie, series: series, rec: report.RecorderFrom(ctx)}
	ss.tvdbDetails.from(tvdb, p.tvdb.Enabled(), func() (sources.Details, error) { return p.tvdb.Details(ctx, tvdb) })
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
	annID, malID := entry.ANN, entry.MAL
	if a != nil && len(a.ANNIDs) > 0 {
		annID = a.ANNIDs[0]
	}
	if a != nil && len(a.MALIDs) > 0 {
		malID = a.MALIDs[0]
	}
	ss.ann.from(annID, true, func() (sources.ANNAnime, error) { return p.ann.Anime(ctx, annID) })
	ss.mal.from(malID, p.mal.Enabled(), func() (sources.MALAnime, error) { return p.mal.Anime(ctx, malID) })
	return ss
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
	return out
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
// line per show says which source supplied each field ("-" = none). Returns
// false when a source it asked failed (an error or the build's time running
// out), so the show is rebuilt soon instead of being kept an hour without it.
func (ss *showSources) fill(s *Show) bool {
	cfg := ss.p.cfg
	done := ss.rec.Step("Show fields and artwork")
	ss.warm()
	used := []any{"root", s.Root, "first_entry", ss.series}
	note := func(field, src string) {
		used = append(used, field, cmp.Or(src, "-"))
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

	all := cfg.ArtAllSources
	artField := func(name string, order []string, pick func(sources.Art) []string) ([]string, string) {
		urls, counts := images(order, all, func(src string) []string { return pick(ss.art(src)) })
		ss.recordField(name, order, counts, all && counts != "", func(src string) string {
			return countOf(len(pick(ss.art(src))), strings.ToLower(name))
		})
		return urls, counts
	}
	s.Art.Posters, src = artField("Posters", cfg.PosterSources, func(a sources.Art) []string { return a.Posters })
	note("posters", src)
	s.Art.Backgrounds, src = artField("Backgrounds", cfg.BackgroundSources, func(a sources.Art) []string { return a.Backgrounds })
	note("backgrounds", src)
	s.Art.Logos, src = artField("Logos", cfg.LogoSources, func(a sources.Art) []string { return a.Logos })
	note("logos", src)
	slog.Debug("show fields: source used", used...)
	if failed := ss.failed(); len(failed) > 0 {
		slog.Info("show sources failed; using the others, rebuilt in 2 min", "root", s.Root, "failed", failed)
		if ss.rec != nil { // the notes are built only for a match report
			list := strings.Join(failed, ", ")
			done("failed: " + list)
			ss.rec.Check(report.Check{Level: report.LevelWarn, Title: "A source failed: " + list, Cause: true,
				Text: "The other sources filled in. Kizuna builds the show again in 2 minutes.", Section: report.SectionFields})
		}
		return false
	}
	done("")
	return true
}

// warm fetches, in parallel, everything fill will ask for whatever the
// answers: the first source of every field (fallbacks are fetched only when
// reached), and the artwork used (every listed source in "all" mode,
// otherwise each image type's first choice). The lazy values make fill's
// own calls free afterwards.
func (ss *showSources) warm() {
	cfg := ss.p.cfg
	var jobs []func()
	first := func(order []string, get func(string)) {
		if len(order) > 0 {
			jobs = append(jobs, func() { get(order[0]) })
		}
	}
	first(cfg.TitleSources, func(src string) { ss.title(src) })
	first(cfg.SummarySources, func(src string) { ss.summary(src) })
	for _, order := range [][]string{cfg.GenreSources, cfg.CastSources, cfg.StudioSources, cfg.CrewSources, cfg.RatingSources, cfg.DateSources} {
		first(order, func(src string) { ss.details(src) })
	}
	for _, order := range [][]string{cfg.PosterSources, cfg.BackgroundSources, cfg.LogoSources} {
		if cfg.ArtAllSources {
			for _, src := range order {
				jobs = append(jobs, func() { ss.art(src) })
			}
		} else {
			first(order, func(src string) { ss.art(src) })
		}
	}
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Go(job)
	}
	wg.Wait()
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
// source that has any (all=false). Duplicates are dropped. The second value
// counts images per source, e.g. "tmdb:12,tvdb:5".
func images(order []string, all bool, get func(src string) []string) ([]string, string) {
	var out, counts []string
	seen := map[string]bool{}
	for _, src := range order {
		n := 0
		for _, u := range get(src) {
			if u != "" && !seen[u] {
				seen[u] = true
				out = append(out, u)
				n++
			}
		}
		if n > 0 {
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
