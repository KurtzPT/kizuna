package provider

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"kizuna/internal/fetch"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// Match reports (internal/report): what the provider records while it
// builds and answers, so a person can follow one show without the logs.

// SetReports turns the match reports on (nil = off).
func (p *Provider) SetReports(s *report.Store) { p.reports = s }

// reportKey is the report page of a show: its AniDB entry, so /anidb and
// /tvdb builds of one anime share a page; a TVDB series without one keeps
// its own key.
func (p *Provider) reportKey(root string) string {
	m := rootRe.FindStringSubmatch(root)
	if m == nil {
		return ""
	}
	if m[1] == "tvdb" {
		id, _ := strconv.Atoi(m[2])
		if aid := p.lists.FirstAID(id); aid > 0 {
			return fmt.Sprintf("anidb-%d", aid)
		}
	}
	return root
}

// modeOfRoot: the provider a build is reported under when neither a match
// nor a metadata request says (a build started outside a Plex request).
func modeOfRoot(root string) string {
	if strings.HasPrefix(root, "tvdb-") {
		return "tvdb"
	}
	return "anidb"
}

// buildMode: the provider a build is reported under, and whose GUIDs its
// "Sent to Plex" shows: the match request that started it, else the Plex
// request that did (/auto asks for anidb- and tvdb- shows alike).
func buildMode(ctx context.Context, root string) string {
	if m := report.MatchFrom(ctx); m != nil && m.Mode != "" {
		return m.Mode
	}
	if rq := report.RequestFrom(ctx); rq != nil && rq.Mode != "" {
		return rq.Mode
	}
	return modeOfRoot(root)
}

// safeReport runs report work at the end of a build. A bug in it is logged
// and goes no further: the build and its waiters are already done.
func safeReport(root string, work func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("match report panicked", "root", root, "err", r, "stack", string(debug.Stack()))
		}
	}()
	work()
}

var spanKinds = map[fetch.Kind]report.SpanKind{
	fetch.KindNet: report.SpanNet, fetch.KindCache: report.SpanCache, fetch.KindStale: report.SpanStale,
	fetch.KindFail: report.SpanFail, fetch.KindMemory: report.SpanMemory,
}

// observeFetches records every download, cache read and in-memory answer of
// a build in its trace.
func observeFetches(ctx context.Context, rec *report.Recorder) context.Context {
	if rec == nil {
		return ctx
	}
	return fetch.WithObserver(ctx, func(e fetch.Event) {
		rec.Span(sourceOfKey(e.Key), e.Key, errText(e.Err), spanKinds[e.Kind], e.Start, e.Took)
	})
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	if fetch.IsNotFound(err) {
		return "not found"
	}
	return fetch.Redact(err.Error())
}

// keySources: a cache folder or host name (version digits cut off: tvdb3,
// api4) -> the source it belongs to.
var keySources = map[string]string{
	"anidb": "anidb", "tvdb": "tvdb", "thetvdb": "tvdb", "tmdb": "tmdb", "themoviedb": "tmdb",
	"mal": "mal", "myanimelist": "mal", "ann": "ann", "animenewsnetwork": "ann", "mapping": "map",
}

// sourceOfKey names the source of a cache key ("tvdb3/1/series.json", by
// its folder) or an uncached URL (by its host).
func sourceOfKey(key string) string {
	k := strings.ToLower(key)
	names := []string{k[:max(strings.IndexByte(k, '/'), 0)]}
	if _, rest, ok := strings.Cut(k, "://"); ok {
		host, _, _ := strings.Cut(rest, "/")
		names = strings.Split(host, ".")
	}
	for _, n := range names {
		if src, ok := keySources[strings.TrimRight(n, "0123456789")]; ok {
			return src
		}
	}
	return "other"
}

// recordMatch turns a match request and its outcome into the report form.
func (p *Provider) recordMatch(mode Mode, r MatchRequest, title, how string, cands []candidate, seen []report.Candidate) *report.Match {
	m := &report.Match{Time: time.Now(), Mode: mode.Path, Type: matchType(r.Type), Title: title, Year: r.Year, File: r.Filename, GUID: r.GUID,
		Manual: r.Manual == 1, How: how, Min: p.cfg.MinScore, Candidates: seen}
	if len(cands) > 0 {
		m.Root, m.Score = cands[0].root, cands[0].score
	}
	return m
}

// --- end of a build ----------------------------------------------------------

var htmlEntity = regexp.MustCompile(`&(?:[a-zA-Z]{2,8}|#\d{1,6}|#x[0-9a-fA-F]{1,6});`)

// finishReport fills in a finished build: the show's ids, the mapping, the
// episodes, the item Plex gets and the checks.
func (p *Provider) finishReport(rec *report.Recorder, root string, s *Show, err error, took time.Duration) {
	if rec == nil {
		return
	}
	mode, ok := Modes[rec.Mode()] // the provider Plex asked; its GUID scheme is in the output
	if !ok {
		mode = Modes[modeOfRoot(root)]
	}
	head := report.ShowDoc{}
	aids := p.aidsOf(root)
	if s != nil {
		head.Title, head.Orig = s.Title, s.OriginalTitle
	} else if len(aids) > 0 {
		head.Title = p.mainTitle(aids[0])
	}
	head.IDs = p.idsOf(root, aids)
	rec.Finish(took, err, head, func(b *report.Build) {
		b.Mapping = p.mappingRows(aids)
		if s == nil {
			return
		}
		b.Complete = s.Complete
		for _, n := range s.Seasons() {
			b.Seasons = append(b.Seasons, report.Season{Name: seasonTitle(n), Count: len(s.Episodes[n])})
			b.EpsTotal += len(s.Episodes[n])
		}
		var issues []epIssue
		b.Episodes, issues = episodeRows(s)
		b.Output = outputRows(p.showItem(mode, s, false))
		b.Checks = append(b.Checks, buildChecks(s, b, issues)...)
	})
}

// aidsOf returns the AniDB entries a show is built from: the one of an
// /anidb show, every one mapped to a /tvdb series (first entry first).
func (p *Provider) aidsOf(root string) []int {
	m := rootRe.FindStringSubmatch(root)
	if m == nil {
		return nil
	}
	id, _ := strconv.Atoi(m[2])
	if m[1] == "anidb" {
		return []int{id}
	}
	first := p.lists.FirstAID(id)
	out := []int{}
	if first > 0 {
		out = append(out, first)
	}
	for _, aid := range p.lists.AIDsForTVDB(id) {
		if aid != first {
			out = append(out, aid)
		}
	}
	return out
}

func (p *Provider) idsOf(root string, aids []int) []report.ID {
	var out []report.ID
	add := func(site string, v any) {
		s := fmt.Sprint(v)
		if s == "" || s == "0" {
			return
		}
		for _, id := range out {
			if id.Site == site {
				return
			}
		}
		out = append(out, report.ID{Site: site, Value: s})
	}
	if strings.HasPrefix(root, "tvdb-") {
		add("tvdb", strings.TrimPrefix(root, "tvdb-"))
	}
	if len(aids) > 0 {
		e := p.lists.ByAID(aids[0])
		add("anidb", aids[0])
		add("tvdb", e.TVDB.ID)
		add("tmdb", e.TMDB.ID)
		add("tmdbmovie", e.TMDBMovie)
		add("mal", e.MAL)
		add("anilist", e.AniList)
		add("imdb", e.IMDB)
		add("ann", e.ANN)
		add("kitsu", e.Kitsu)
	}
	return out
}

func (p *Provider) mappingRows(aids []int) []report.MapRow {
	var out []report.MapRow
	for _, aid := range aids {
		e := p.lists.ByAID(aid)
		if e.AID == 0 && e.TVDB.ID == 0 && e.TMDB.ID == 0 {
			continue
		}
		r := report.MapRow{AID: aid, Name: p.mainTitle(aid),
			TVDB: e.TVDB.ID, TVDBOffset: e.TVDB.Offset, TMDB: cmp.Or(e.TMDB.ID, e.TMDBMovie),
			Exceptions: len(e.TVDB.Episodes) + len(e.TMDB.Episodes)}
		if e.TVDB.ID > 0 {
			r.TVDBSeason = placementSeason(e.TVDB.Season)
		}
		if e.TMDB.ID > 0 && e.TMDB.Mapped {
			r.TMDBSeason = placementSeason(e.TMDB.Season)
		} else if e.TMDBMovie > 0 && e.TMDB.ID == 0 {
			r.TMDBSeason = "movie"
		}
		out = append(out, r)
	}
	return out
}

func placementSeason(n int) string {
	if n < 0 {
		return "absolute"
	}
	return strconv.Itoa(n)
}

// episodeRows lists the episodes as Plex gets them, and what is wrong with
// each (issues, in the same order).
func episodeRows(s *Show) ([]report.Episode, []epIssue) {
	var out []report.Episode
	var issues []epIssue
	for _, n := range s.Seasons() {
		for i := range s.Episodes[n] {
			ep := &s.Episodes[n][i]
			is, text := episodeIssue(ep)
			out = append(out, report.Episode{S: ep.Season, E: ep.Index, Title: ep.Title, From: ep.titleSrc,
				Titles: ep.titles, Pairs: ep.pairs, Date: ep.Date, Issue: text})
			issues = append(issues, is)
		}
	}
	return out, issues
}

// epIssue is what is wrong with an episode as Plex gets it.
type epIssue struct{ noTitle, placeholder, entity, noDate bool }

// episodeIssue: what is wrong with an episode, and the same in words ("" if
// nothing).
func episodeIssue(ep *Ep) (epIssue, string) {
	var is epIssue
	var text []string
	switch {
	case ep.Title == "":
		is.noTitle = true
		text = append(text, "no title from any source: Plex shows \"Episode "+strconv.Itoa(ep.Index)+"\"")
	case genericTitle.MatchString(ep.Title):
		is.placeholder = true
		text = append(text, "only a placeholder title")
	}
	if htmlEntity.MatchString(ep.Title) || htmlEntity.MatchString(ep.Summary) {
		is.entity = true
		text = append(text, "HTML entity in the text (shown literally by Plex)")
	}
	if ep.Date == "" && ep.Season > 0 {
		is.noDate = true
		text = append(text, "no air date")
	}
	return is, strings.Join(text, "; ")
}

// buildChecks: what in a finished build is worth a look. b.Checks holds the
// ones recorded while it ran; issues are the problems of b.Episodes.
func buildChecks(s *Show, b *report.Build, issues []epIssue) []report.Check {
	var out []report.Check
	if s.Placeholders {
		out = append(out, report.Check{Level: report.LevelWarn, Title: "Placeholder episodes", Cause: true,
			Text: "AniDB's details were not available, so every episode is a bare \"Episode N\" until the next build.", Section: report.SectionEpisodes})
	}
	cause := func(c report.Check) bool { return c.Cause }
	if !s.Complete && !slices.ContainsFunc(out, cause) && !slices.ContainsFunc(b.Checks, cause) { // else a check already says why
		out = append(out, report.Check{Level: report.LevelWarn, Title: "Built with something missing",
			Text: "A list came back partial, episode crew was still loading, or the build ran out of time. Plex got what was there; Kizuna keeps this build 2 minutes and then builds the show again.", Section: report.SectionTrace})
	}
	if m := b.Match; m != nil && m.How == "title search" && m.Score < 100 {
		out = append(out, report.Check{Level: report.LevelInfo, Title: fmt.Sprintf("Matched by title search with score %d", m.Score),
			Text: "Not an exact title. If it is the wrong show, add an id tag such as [anidb-N] to the folder name.", Section: report.SectionMatch})
	}
	var noTitle, entity, noDate []string
	for i, is := range issues {
		code := fmt.Sprintf("S%02dE%02d", b.Episodes[i].S, b.Episodes[i].E)
		if is.noTitle || is.placeholder {
			noTitle = append(noTitle, code)
		}
		if is.entity {
			entity = append(entity, code)
		}
		if is.noDate {
			noDate = append(noDate, code)
		}
	}
	if len(noTitle) > 0 {
		out = append(out, report.Check{Level: report.LevelWarn, Title: plural(len(noTitle), "episode has", "episodes have") + " no real title",
			Text: joinShort(noTitle, 6) + ". No source in EPISODE_TITLES has a title for them.", Section: report.SectionEpisodes})
	}
	if len(entity) > 0 {
		out = append(out, report.Check{Level: report.LevelWarn, Title: plural(len(entity), "episode has", "episodes have") + " an HTML entity in the text",
			Text: joinShort(entity, 6) + ". Plex shows it literally (\"&lt;3\" instead of \"<3\").", Section: report.SectionEpisodes})
	}
	if htmlEntity.MatchString(s.Title) || htmlEntity.MatchString(s.Summary) {
		out = append(out, report.Check{Level: report.LevelWarn, Title: "HTML entity in the show's title or summary",
			Text: "Plex shows it literally.", Section: report.SectionFields})
	}
	if len(noDate) > 0 {
		out = append(out, report.Check{Level: report.LevelInfo, Title: plural(len(noDate), "episode has", "episodes have") + " no air date",
			Text: joinShort(noDate, 6) + ".", Section: report.SectionEpisodes})
	}
	for _, f := range b.Fields {
		if len(f.Used) == 0 && (f.Name == "Title" || f.Name == "Summary" || f.Name == "Posters") {
			out = append(out, report.Check{Level: report.LevelWarn, Title: "No source had a " + strings.ToLower(strings.TrimSuffix(f.Name, "s")),
				Text: "Asked: " + strings.Join(f.Asked, ", ") + ".", Section: report.SectionFields})
		}
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// outputRows condenses the show item Plex gets.
func outputRows(it Item) [][2]string {
	var out [][2]string
	add := func(k, v string) {
		if v != "" {
			out = append(out, [2]string{k, v})
		}
	}
	tags := func(ts []Tag) string {
		var s []string
		for _, t := range ts {
			s = append(s, t.Tag)
		}
		return strings.Join(s, ", ")
	}
	add("ratingKey", it.RatingKey)
	add("guid", it.GUID)
	add("title", it.Title)
	add("originalTitle", it.OriginalTitle)
	add("summary", it.Summary)
	if it.Year > 0 {
		add("year", strconv.Itoa(it.Year))
	}
	add("originallyAvailableAt", it.OriginallyAvailableAt)
	add("studio", it.Studio)
	add("contentRating", it.ContentRating)
	if it.Duration > 0 {
		add("duration", fmt.Sprintf("%d ms (%d min)", it.Duration, it.Duration/60000))
	}
	add("Genre", tags(it.Genre))
	add("Director", tags(it.Director))
	add("Writer", tags(it.Writer))
	add("Role", firstThree(len(it.Role), func(i int) string { return it.Role[i].Tag + " as " + it.Role[i].Role }))
	var guids []string
	for _, g := range it.Guid {
		guids = append(guids, g.ID)
	}
	add("Guid", strings.Join(guids, ", "))
	add("thumb", it.Thumb)
	add("art", it.Art)
	counts := map[string]int{}
	var order []string
	for _, im := range it.Image {
		if counts[im.Type] == 0 {
			order = append(order, im.Type)
		}
		counts[im.Type]++
	}
	var imgs []string
	for _, t := range order {
		imgs = append(imgs, fmt.Sprintf("%d %s", counts[t], t))
	}
	add("Image", strings.Join(imgs, ", "))
	return out
}

// --- show fields -------------------------------------------------------------

// recordField notes one field: the sources asked (in order up to the one
// used, or all of them when none had a value or every source's images are
// joined), what each offered, and which were used. used is the source, or
// images()'s "tvdb:14,tmdb:9"; it is only split here, so nothing is
// allocated with the reports off. value only reads what the field's pick
// already fetched.
func (ss *showSources) recordField(name string, order []string, used string, all bool, value func(src string) string) {
	if ss.rec == nil {
		return
	}
	f := report.Field{Name: name, Order: order, Offered: map[string]string{}}
	if used != "" {
		for _, c := range strings.Split(used, ",") {
			src, _, _ := strings.Cut(c, ":")
			f.Used = append(f.Used, src)
		}
	}
	for _, src := range order {
		f.Asked = append(f.Asked, src)
		f.Offered[src] = value(src)
		if !all && len(f.Used) > 0 && src == f.Used[0] {
			break
		}
	}
	ss.rec.Field(f)
}

func joinShort(list []string, n int) string {
	if len(list) > n {
		return strings.Join(list[:n], ", ") + fmt.Sprintf(" and %d more", len(list)-n)
	}
	return strings.Join(list, ", ")
}

func describeCast(cast []sources.Person) string {
	return firstThree(len(cast), func(i int) string {
		if cast[i].Role != "" {
			return cast[i].Name + " as " + cast[i].Role
		}
		return cast[i].Name
	})
}

// firstThree condenses a list of n people: "12: A as X, B as Y, C as Z, …".
func firstThree(n int, who func(i int) string) string {
	if n == 0 {
		return ""
	}
	names := make([]string, min(n, 3))
	for i := range names {
		names[i] = who(i)
	}
	s := fmt.Sprintf("%d: %s", n, strings.Join(names, ", "))
	if n > 3 {
		s += ", …"
	}
	return s
}

func describeCrew(d sources.Details) string {
	var parts []string
	if len(d.Directors) > 0 {
		parts = append(parts, "Directors: "+joinShort(d.Directors, 4))
	}
	if len(d.Writers) > 0 {
		parts = append(parts, "Writers: "+joinShort(d.Writers, 4))
	}
	return strings.Join(parts, ". ")
}

func countOf(n int, what string) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d %s", n, what)
}
