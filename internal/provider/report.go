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

// SetExtras sets what is called, with the show's GUID, each time Plex asks
// for a show's extras, which it does on every metadata refresh of the show.
// Call it before serving.
func (p *Provider) SetExtras(f func(guid string)) { p.extras = f }

// reportKey is the report page of a show: its AniDB entry, so /anidb and
// /tvdb builds of one anime share a page; a TVDB series without one keeps
// its own key.
func (p *Provider) reportKey(root string) string {
	site, id, ok := parseRoot(root)
	if !ok {
		return ""
	}
	if site == "tvdb" {
		if aid := p.lists.FirstAID(id); aid > 0 {
			return fmt.Sprintf("anidb-%d", aid)
		}
	}
	return root
}

// buildMode: the provider a build is reported under, and whose GUIDs its
// "Sent to Plex" shows: the match request that started it, else the Plex
// request that did (/auto asks for anidb- and tvdb- shows alike), else the
// show's own (a build started outside a Plex request).
func buildMode(ctx context.Context, root string) string {
	if m := report.MatchFrom(ctx); m != nil && m.Mode != "" {
		return m.Mode
	}
	if rq := report.RequestFrom(ctx); rq != nil && rq.Mode != "" {
		return rq.Mode
	}
	if strings.HasPrefix(root, "tvdb-") {
		return "tvdb"
	}
	return "anidb"
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

// observeFetches records every download, cache read and in-memory answer of
// a build in its trace.
func observeFetches(ctx context.Context, rec *report.Recorder) context.Context {
	if rec == nil {
		return ctx
	}
	return fetch.WithObserver(ctx, func(e fetch.Event) {
		rec.Span(sourceOfKey(e.Key), e.Key, errText(e.Err), report.SpanKind(e.Kind), e.Start, e.Took)
	})
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	if fetch.IsNotFound(err) {
		return "not found"
	}
	return err.Error() // URLs in it were redacted where the error was made
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
		Manual: r.Manual == 1, How: how, MinScore: p.cfg.MinScore, Candidates: seen}
	if len(cands) > 0 {
		m.Root, m.Score = cands[0].root, cands[0].score
	}
	// The tag rules' reasons (idCandidate's stop) start "id tags " or
	// "tag "; a reason that names the title search means it ran.
	m.TagRule = len(cands) == 0 && (strings.HasPrefix(how, "id tags ") || strings.HasPrefix(how, "tag ")) && !strings.Contains(how, "title search")
	if q := strings.TrimSpace(tagRe.ReplaceAllString(title, "")); q != title {
		m.Query = q
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
	mode := Modes[rec.Mode()] // the provider Plex asked (buildMode); its GUID scheme is in the output
	head := report.ShowHead{}
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
		var with [][]string
		b.Episodes, with = episodeRows(s)
		b.Output = outputRows(p.showItem(mode, s, false))
		b.Checks = append(b.Checks, buildChecks(s, b, with)...)
	})
}

// aidsOf returns the AniDB entries a show is built from: the one of an
// /anidb show, every one mapped to a /tvdb series (first entry first).
func (p *Provider) aidsOf(root string) []int {
	site, id, ok := parseRoot(root)
	if !ok {
		return nil
	}
	if site == "anidb" {
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
	if site, id, _ := parseRoot(root); site == "tvdb" {
		add("tvdb", id)
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

// episodeRows lists the episodes as Plex gets them and, per
// episodeProblems, the episodes with that problem ("S01E05").
func episodeRows(s *Show) ([]report.Episode, [][]string) {
	var out []report.Episode
	with := make([][]string, len(episodeProblems))
	for _, n := range s.Seasons() {
		for i := range s.Episodes[n] {
			ep := &s.Episodes[n][i]
			var issues []string
			for j, pr := range episodeProblems {
				if text := pr.issue(ep); text != "" {
					issues = append(issues, text)
					with[j] = append(with[j], fmt.Sprintf("S%02dE%02d", ep.Season, ep.Index))
				}
			}
			out = append(out, report.Episode{Season: ep.Season, Index: ep.Index, Title: ep.Title, From: ep.titleSrc,
				Titles: ep.titles, Pairs: ep.pairs, Date: ep.Date, Issue: strings.Join(issues, "; ")})
		}
	}
	return out, with
}

// episodeProblems is what can be wrong with an episode as Plex gets it:
// issue says it on the episode's row ("" if fine), and one check lists
// every episode with it.
var episodeProblems = []struct {
	issue       func(ep *Ep) string
	level       report.Level
	title, text string // the check's, after "N episodes have" and after the episode list
}{
	{func(ep *Ep) string {
		switch {
		case ep.Title == "":
			return "no title from any source: Plex shows \"Episode " + strconv.Itoa(ep.Index) + "\""
		case genericTitle.MatchString(ep.Title):
			return "only a placeholder title"
		}
		return ""
	}, report.LevelWarn, " no real title", `. No source in EPISODE_TITLES has a title for them, or only a placeholder like "Episode 5".`},
	{func(ep *Ep) string {
		if htmlEntity.MatchString(ep.Title) || htmlEntity.MatchString(ep.Summary) {
			return "HTML entity in the text (shown literally by Plex)"
		}
		return ""
	}, report.LevelWarn, " an HTML entity in the text", `. Plex shows it literally ("&lt;3" instead of "<3").`},
	{func(ep *Ep) string {
		if ep.Date == "" && ep.Season > 0 {
			return "no air date"
		}
		return ""
	}, report.LevelInfo, " no air date", "."},
}

// buildChecks: what in a finished build is worth a look. b.Checks holds the
// ones recorded while it ran; with is episodeRows' episodes per problem.
func buildChecks(s *Show, b *report.Build, with [][]string) []report.Check {
	var out []report.Check
	if s.Placeholders {
		bare := "every episode is a bare \"Episode N\""
		if slices.ContainsFunc(s.Episodes[1], func(e Ep) bool { return e.tvdbEp > 0 }) { // a special-only show: see finishSpecial
			bare = "every episode but the ones from TheTVDB's specials is a bare \"Episode N\""
		}
		text := "AniDB's details were not available, so " + bare + " until the next build."
		switch {
		case !s.anidbBlocked.IsZero(): // complete: no details to wait for until the block ends
			text = "AniDB answered this anime's last call with an error, and AniDB's rules block asking again until " +
				s.anidbBlocked.Format("2006-01-02 15:04 MST") + ", so " + bare + ". " +
				"The first build after that asks AniDB again."
		case s.Complete: // without ANIDB_CLIENT: no details to wait for, no rebuild
			text = "AniDB isn't set up (no ANIDB_CLIENT), so " + bare + ". Set ANIDB_CLIENT for the real episode list."
		}
		out = append(out, report.Check{Level: report.LevelWarn, Title: "Placeholder episodes", Cause: true,
			Text: text, Section: report.SectionEpisodes})
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
	if m := b.Match; m != nil {
		if _, note, ok := strings.Cut(m.How, ", by name: "); ok { // see specialByName
			out = append(out, report.Check{Level: report.LevelWarn, Title: "Matched by name inside a TheTVDB id",
				Text:    "The TheTVDB id names a whole series; the title or file name picked one of its movies or specials: " + note + ".",
				Section: report.SectionMatch})
		}
	}
	for j, codes := range with {
		if pr := episodeProblems[j]; len(codes) > 0 {
			out = append(out, report.Check{Level: pr.level, Title: plural(len(codes), "episode has", "episodes have") + pr.title,
				Text: joinShort(codes, 6) + pr.text, Section: report.SectionEpisodes})
		}
	}
	if htmlEntity.MatchString(s.Title) || htmlEntity.MatchString(s.Summary) {
		out = append(out, report.Check{Level: report.LevelWarn, Title: "HTML entity in the show's title or summary",
			Text: "Plex shows it literally.", Section: report.SectionFields})
	}
	for _, f := range b.Fields {
		if len(f.Used) == 0 && len(f.Order) > 0 && (f.Name == "Title" || f.Name == "Summary" || f.Name == "Posters") {
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
	add("Role", firstThree(len(it.Role), func(i int) string {
		if it.Role[i].Role == "" {
			return it.Role[i].Tag
		}
		return it.Role[i].Tag + " as " + it.Role[i].Role
	}))
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
	if order == nil {
		order = []string{} // X_SOURCES=none: an empty list in the page's data, not null
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
