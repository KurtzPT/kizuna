package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

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

	tvdbText, tmdbText       lazy[text]
	tvdbArt, tmdbArt         lazy[sources.Art]
	tvdbDetails, tmdbDetails lazy[sources.Details]
	mal                      lazy[sources.MALAnime]
	ann                      lazy[sources.ANNAnime]
}

type text struct{ title, summary string }

func (p *Provider) newSources(ctx context.Context, aid int, a *sources.AniDBAnime, tvdb, tmdb int, movie, series bool) *showSources {
	ss := &showSources{p: p, aid: aid, a: a, tvdb: tvdb, tmdb: tmdb, movie: movie, series: series}
	ss.tvdbDetails.f = func() sources.Details {
		if tvdb == 0 || !p.tvdb.Enabled() {
			return sources.Details{}
		}
		return p.tvdb.Details(ctx, tvdb)
	}
	ss.tmdbDetails.f = func() sources.Details {
		if tmdb == 0 || !p.tmdb.Enabled() {
			return sources.Details{}
		}
		return p.tmdb.Details(ctx, tmdb, movie)
	}
	ss.tvdbText.f = func() text {
		if tvdb == 0 || !p.tvdb.Enabled() {
			return text{}
		}
		n, o := p.tvdb.Overview(ctx, tvdb)
		return text{n, o}
	}
	ss.tmdbText.f = func() text {
		if tmdb == 0 || !p.tmdb.Enabled() {
			return text{}
		}
		n, o := p.tmdb.Text(ctx, tmdb, movie)
		return text{n, o}
	}
	ss.tvdbArt.f = func() sources.Art {
		if tvdb == 0 || !p.tvdb.Enabled() {
			return sources.Art{}
		}
		return p.tvdb.Art(ctx, tvdb)
	}
	ss.tmdbArt.f = func() sources.Art {
		if tmdb == 0 || !p.tmdb.Enabled() {
			return sources.Art{}
		}
		return p.tmdb.Art(ctx, tmdb, movie)
	}
	ss.ann.f = func() sources.ANNAnime {
		id := p.lists.ByAID(aid).ANN // mapping, for when AniDB has no ANN link or no details
		if a != nil && len(a.ANNIDs) > 0 {
			id = a.ANNIDs[0]
		}
		if id == 0 {
			return sources.ANNAnime{}
		}
		n, _ := p.ann.Anime(ctx, id)
		return n
	}
	ss.mal.f = func() sources.MALAnime {
		id := p.lists.ByAID(aid).MAL // mapping, for when AniDB has no MAL link or no details
		if a != nil && len(a.MALIDs) > 0 {
			id = a.MALIDs[0]
		}
		if id == 0 {
			return sources.MALAnime{}
		}
		m, _ := p.mal.Anime(ctx, id)
		return m
	}
	return ss
}

func (ss *showSources) title(src string) string {
	switch src {
	case "anidb":
		titles := ss.p.titles.Of(ss.aid)
		if ss.a != nil {
			titles = ss.a.Titles
		}
		if len(titles) > 0 {
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

func (ss *showSources) summary(src string) string {
	switch src {
	case "anidb":
		if ss.a != nil {
			return ss.a.Description
		}
	case "tvdb":
		return ss.tvdbText.get().summary
	case "tmdb":
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
		if ss.a != nil {
			return ss.a.Details()
		}
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
// line per show says which source supplied each field ("-" = none).
func (ss *showSources) fill(s *Show) {
	cfg := ss.p.cfg
	ss.warm()
	used := []any{"root", s.Root, "first_entry", ss.series}
	note := func(field, src string) {
		used = append(used, field, cmpOr(src, "-"))
	}

	title, src := pick(cfg.TitleSources, ss.title)
	s.Title = firstOf(title, s.Title)
	note("title", src)
	summary, src := pick(cfg.SummarySources, ss.summary)
	s.Summary = firstOf(summary, s.Summary)
	note("summary", src)

	d, src := ss.pickDetails(cfg.GenreSources, func(d sources.Details) bool { return len(d.Genres) > 0 })
	s.Genres = d.Genres
	note("genres", src)
	d, src = ss.pickDetails(cfg.CastSources, func(d sources.Details) bool { return len(d.Cast) > 0 })
	s.Roles = d.Cast
	note("cast", src)
	d, src = ss.pickDetails(cfg.StudioSources, func(d sources.Details) bool { return d.Studio != "" })
	s.Studio = d.Studio
	note("studio", src)
	d, src = ss.pickDetails(cfg.CrewSources, func(d sources.Details) bool { return len(d.Directors)+len(d.Writers) > 0 })
	s.Directors, s.Writers = d.Directors, d.Writers
	note("crew", src)
	// Content rating: a listed country (or a country-less rating) from any
	// source first; only then a rating from some other country.
	d, src = ss.pickDetails(cfg.RatingSources, func(d sources.Details) bool { return d.Rating(cfg.RatingCountries) != "" })
	s.ContentRating = d.Rating(cfg.RatingCountries)
	if s.ContentRating == "" {
		d, src = ss.pickDetails(cfg.RatingSources, func(d sources.Details) bool { return d.AnyRating() != "" })
		s.ContentRating = d.AnyRating()
	}
	note("rating", src)
	d, src = ss.pickDetails(cfg.DateSources, func(d sources.Details) bool { return d.Date != "" })
	s.Date = firstOf(fullDate(d.Date), s.Date)
	note("date", src)

	all := cfg.ArtAllSources
	s.Art.Posters, src = images(cfg.PosterSources, all, func(src string) []string { return ss.art(src).Posters })
	note("posters", src)
	s.Art.Backgrounds, src = images(cfg.BackgroundSources, all, func(src string) []string { return ss.art(src).Backgrounds })
	note("backgrounds", src)
	s.Art.Logos, src = images(cfg.LogoSources, all, func(src string) []string { return ss.art(src).Logos })
	note("logos", src)
	slog.Debug("show fields: source used", used...)
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

func cmpOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
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

// lazy computes f once, on first use; safe for concurrent callers.
type lazy[T any] struct {
	once sync.Once
	f    func() T
	v    T
}

func (l *lazy[T]) get() T {
	l.once.Do(func() { l.v = l.f() })
	return l.v
}
