package provider

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
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
	"mal": "mal", "myanimelist": "mal", "ann": "ann", "animenewsnetwork": "ann", "mapping": "map", "plex": "plex",
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
func (p *Provider) recordMatch(mode Mode, r MatchRequest, d decision) *report.Match {
	m := &report.Match{Time: time.Now(), Mode: mode.Path, Type: matchType(r.Type), Title: d.title, Year: r.Year, File: r.Filename, GUID: r.GUID,
		Manual: r.Manual == 1, How: d.how(), By: matchBy(d), MinScore: p.cfg.MinScore}
	for _, f := range d.found {
		m.Candidates = append(m.Candidates, report.Candidate{AID: f.aid, Title: f.title, Score: f.score})
	}
	if len(d.cands) > 0 {
		m.Root, m.Score = d.cands[0].root, d.cands[0].score
	}
	if q := strings.TrimSpace(tagRe.ReplaceAllString(d.title, "")); q != d.title {
		m.Query = q
	}
	return m
}

// matchBy is what decided, as the report's facts: what decision.how says
// in words.
func matchBy(d decision) report.MatchBy {
	by := report.MatchBy{Text: d.id.text, Best: d.search.best}
	switch d.id.kind {
	case idTag:
		by.ID = report.MatchTag
	case idGUID:
		by.ID = report.MatchGUID
	case idManyTags:
		by.ID = report.MatchManyTags
	case idModeOff:
		by.ID, by.Off, by.Need = report.MatchTagOff, d.id.path, d.id.need
	}
	if d.id.aid > 0 {
		by.ByName = &report.ByName{AID: d.id.aid, Sure: d.id.sure}
	}
	switch d.search.kind {
	case searchFound:
		by.Search = report.SearchFound
	case searchNothing:
		by.Search = report.SearchNone
	case searchBelowMin:
		by.Search = report.SearchLow
	}
	return by
}

// --- end of a build ----------------------------------------------------------

var htmlEntity = regexp.MustCompile(`&(?:[a-zA-Z]{2,8}|#\d{1,6}|#x[0-9a-fA-F]{1,6});`)

// finishReport fills in a finished build: the show's ids, the mapping, the
// seasons, episodes and images as their items send them, the show item
// Plex gets and the checks.
func (p *Provider) finishReport(rec *report.Recorder, root string, s *Show, err error, took time.Duration) {
	if rec == nil {
		return
	}
	mode := Modes[rec.Mode()] // the provider Plex asked (buildMode); its GUID scheme is in the item and the episodes' GUIDs
	aids := p.aidsOf(root)
	res := report.Result{Mapping: p.mappingRows(aids)}
	if s != nil {
		res.Head.Title, res.Head.Orig = s.Title, s.OriginalTitle
	} else if len(aids) > 0 {
		res.Head.Title = p.mainTitle(aids[0])
	}
	res.Head.IDs = p.idsOf(root, aids)
	if s != nil {
		res.Complete = s.Complete
		show := p.showItem(mode, s, false)
		res.Item = itemJSON(show)
		res.Art = artGroups(nil, "show", nil, show, s.artBy.of(nil))
		res.ItemSrc = itemSources(show, rec.Facts().Fields, res.Art)
		for _, n := range plexSeasons(s) {
			it := p.seasonItem(mode, s, n, false)
			if it == nil {
				continue
			}
			res.Art = artGroups(res.Art, it.Title, &n, *it, s.artBy.of(&n))
			if _, ok := s.Episodes[n]; !ok {
				continue // placeholders' Specials: Plex gets the season, the build has no list for it
			}
			res.Seasons = append(res.Seasons, report.Season{Name: it.Title, Count: len(s.Episodes[n]),
				Index: n, Thumb: it.Thumb, Art: it.Art, Date: it.OriginallyAvailableAt})
			res.EpsTotal += len(s.Episodes[n])
		}
		var with [][]string
		res.Episodes, with = p.episodeRows(mode, s)
		res.Checks = buildChecks(s, rec.Facts(), with)
	}
	rec.Finish(took, err, res)
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

// itemJSON is an item as the handlers write it (writeJSON).
func itemJSON(it Item) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(it); err != nil {
		return nil
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// of returns the sources of the show's art (n nil) or a season's; a nil
// artSources (a show not built by a recorded build) knows none.
func (a *artSources) of(n *int) artUsed {
	switch {
	case a == nil:
		return artUsed{}
	case n == nil:
		return a.show
	}
	return a.seasons[*n]
}

// imageKinds: Plex's image types (artImages) as the report's kinds.
var imageKinds = map[string]struct {
	name string
	kind artKind
}{"coverPoster": {"poster", posterArt}, "background": {"background", backgroundArt}, "clearLogo": {"logo", logoArt}}

// artGroups adds the images an item carries (artImages: up to 10 of a kind,
// each kind's in a row) to out, one group per kind, with the source that
// gave each: by counts every image picked, in the same order. The item's
// thumb and art are marked. A season item without art of its own carries no
// images, so it adds no group: its thumb is the show's poster (seasonItem),
// which the season's row has (report.Season.Thumb).
func artGroups(out []report.ArtGroup, level string, season *int, it Item, by artUsed) []report.ArtGroup {
	var g *report.ArtGroup
	for _, im := range it.Image {
		k, ok := imageKinds[im.Type]
		if !ok {
			continue
		}
		if g == nil || g.Kind != k.name {
			out = append(out, report.ArtGroup{Level: level, Season: season, Kind: k.name})
			g = &out[len(out)-1]
		}
		primary := k.kind == posterArt && im.URL == it.Thumb || k.kind == backgroundArt && im.URL == it.Art
		g.Images = append(g.Images, report.ArtImage{URL: im.URL, Src: sourceOf(by[k.kind], len(g.Images)), Primary: primary})
	}
	return out
}

// itemFields: the show fields fill records and the item keys showItem
// sends each one as.
var itemFields = [...]struct {
	field string
	keys  []string
}{
	{"Title", []string{"title"}}, {"Summary", []string{"summary"}}, {"Genres", []string{"Genre"}}, {"Cast", []string{"Role"}},
	{"Studio", []string{"studio"}}, {"Crew", []string{"Director", "Writer"}}, {"Content rating", []string{"contentRating"}},
	{"First aired", []string{"originallyAvailableAt", "year"}},
}

// itemSources names the source of each value of the show item it, by item
// key, for the keys with one source: the recorded show fields' (fields) and
// the images' (art: the show's own groups, as artGroups made them from it).
// A value no source gave (a title kept from the title dump, the ids, the
// duration) gets no entry, and neither does a key the item doesn't have.
func itemSources(it Item, fields []report.Field, art []report.ArtGroup) map[string]string {
	has := map[string]bool{
		"title": it.Title != "", "summary": it.Summary != "", "Genre": len(it.Genre) > 0, "Role": len(it.Role) > 0,
		"studio": it.Studio != "", "Director": len(it.Director) > 0, "Writer": len(it.Writer) > 0,
		"contentRating": it.ContentRating != "", "originallyAvailableAt": it.OriginallyAvailableAt != "", "year": it.Year != 0,
	}
	out := map[string]string{}
	for _, f := range itemFields {
		i := slices.IndexFunc(fields, func(x report.Field) bool { return x.Name == f.field })
		if i < 0 || len(fields[i].Used) != 1 {
			continue
		}
		for _, k := range f.keys {
			if has[k] {
				out[k] = fields[i].Used[0]
			}
		}
	}
	// The groups hold the item's images of the three kinds in the order
	// sent, so the n-th of them is the n-th such image of the item.
	var sent []report.ArtImage
	for _, g := range art {
		if g.Season != nil {
			continue
		}
		sent = append(sent, g.Images...)
		for _, im := range g.Images {
			if key := map[string]string{"poster": "thumb", "background": "art"}[g.Kind]; im.Primary && im.Src != "" && key != "" {
				out[key] = im.Src
			}
		}
	}
	n := 0
	for i, im := range it.Image {
		if _, ok := imageKinds[im.Type]; !ok {
			continue
		}
		if n < len(sent) && sent[n].URL == im.URL && sent[n].Src != "" {
			out["Image."+strconv.Itoa(i)] = sent[n].Src
		}
		n++
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// episodeRows lists the episodes as Plex gets them (what episodeItem sends,
// each value with its source) and, per episodeProblems, the episodes with
// that problem ("S01E05").
func (p *Provider) episodeRows(mode Mode, s *Show) ([]report.Episode, [][]string) {
	var out []report.Episode
	with := make([][]string, len(episodeProblems))
	for _, n := range s.Seasons() {
		for i := range s.Episodes[n] {
			ep := &s.Episodes[n][i]
			it := p.episodeItem(mode, s, ep)
			var issues []string
			for j, pr := range episodeProblems {
				if text := pr.issue(ep, &it); text != "" {
					issues = append(issues, text)
					with[j] = append(with[j], fmt.Sprintf("S%02dE%02d", ep.Season, ep.Index))
				}
			}
			out = append(out, sentEpisode(it, ep, strings.Join(issues, "; ")))
		}
	}
	return out, with
}

// sentEpisode is an episode's row. Its values are read off it, the item Plex
// gets for ep, so the row can't differ from what was sent; ep says where each
// came from. The title stays ep's: the item says "Episode N" for an episode
// without one, the report says it has none.
func sentEpisode(it Item, ep *Ep, issue string) report.Episode {
	e := report.Episode{Season: ep.Season, Index: ep.Index, Title: ep.Title, From: ep.titleSrc, Titles: ep.titles, Pairs: ep.pairs,
		Summary: it.Summary, Date: it.OriginallyAvailableAt, Thumb: it.Thumb, Minutes: it.Duration / 60000, Issue: issue}
	for _, d := range it.Director {
		e.Directors = append(e.Directors, d.Tag)
	}
	for _, w := range it.Writer {
		e.Writers = append(e.Writers, w.Tag)
	}
	for _, g := range it.Guid {
		e.Guids = append(e.Guids, g.ID)
	}
	for _, f := range [...]struct {
		name string
		has  bool
		src  srcCode
	}{
		{"summary", e.Summary != "", ep.src.summary}, {"date", e.Date != "", ep.src.date}, {"thumb", e.Thumb != "", ep.src.thumb},
		{"minutes", e.Minutes > 0, ep.src.minutes}, {"directors", len(e.Directors) > 0, ep.src.directors},
		{"writers", len(e.Writers) > 0, ep.src.writers},
	} {
		if f.has && f.src != srcNone {
			if e.Src == nil {
				e.Src = map[string]string{}
			}
			e.Src[f.name] = f.src.name()
		}
	}
	return e
}

// episodeProblems is what can be wrong with an episode as Plex gets it:
// issue says it on the episode's row ("" if fine; sent is the episode's
// item), and one check lists every episode with it.
var episodeProblems = []struct {
	issue       func(ep *Ep, sent *Item) string
	level       report.Level
	title, text string // the check's, after "N episodes have" and after the episode list
}{
	{func(ep *Ep, _ *Item) string {
		switch {
		case ep.Title == "":
			return "no title from any source: Plex shows \"Episode " + strconv.Itoa(ep.Index) + "\""
		case genericTitle.MatchString(ep.Title):
			return "only a placeholder title"
		}
		return ""
	}, report.LevelWarn, " no real title", `. No source in EPISODE_TITLES has a title for them, or only a placeholder like "Episode 5".`},
	{func(ep *Ep, _ *Item) string {
		if htmlEntity.MatchString(ep.Title) || htmlEntity.MatchString(ep.Summary) {
			return "HTML entity in the text (shown literally by Plex)"
		}
		return ""
	}, report.LevelWarn, " an HTML entity in the text", `. Plex shows it literally ("&lt;3" instead of "<3").`},
	// What was sent decides: a date too short to send ("200") is no date.
	{func(ep *Ep, sent *Item) string {
		if sent.OriginallyAvailableAt == "" && ep.Season > 0 {
			return "no air date"
		}
		return ""
	}, report.LevelInfo, " no air date", "."},
}

// buildChecks: what in a finished build is worth a look, after the checks
// recorded while it ran. facts is what the build recorded; with is
// episodeRows' episodes per problem.
func buildChecks(s *Show, facts report.Facts, with [][]string) []report.Check {
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
	if !s.Complete && !s.Placeholders && !facts.Cause { // else a check already says why
		out = append(out, report.Check{Level: report.LevelWarn, Title: "Built with something missing",
			Text: "A list came back partial, episode crew was still loading, or the build ran out of time. Plex got what was there; Kizuna keeps this build 2 minutes and then builds the show again.", Section: report.SectionTrace})
	}
	if m := facts.Match; m != nil && m.By.TitleSearch() && m.Score < 100 {
		out = append(out, report.Check{Level: report.LevelInfo, Title: fmt.Sprintf("Matched by title search with score %d", m.Score),
			Text: "Not an exact title. If it is the wrong show, add an id tag such as [anidb-N] to the folder name.", Section: report.SectionMatch})
	}
	// See specialByName. The note is all of the reason after the tag or
	// GUID: a title search's part follows only in a Fix Match list, which is
	// in no report.
	if m := facts.Match; m != nil && m.By.ByName != nil {
		out = append(out, report.Check{Level: report.LevelWarn, Title: "Matched by name inside a TheTVDB id",
			Text:    "The TheTVDB id names a whole series; the title or file name picked one of its movies or specials: " + m.By.ByName.Note() + ".",
			Section: report.SectionMatch})
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
	for _, f := range facts.Fields {
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

// --- show fields -------------------------------------------------------------

// recordField notes one field: the sources asked (in order up to the one
// used, or all of them when none had a value), what each offered, and the
// one used ("" = none). value only reads what the field's pick already
// fetched. It is called with the reports off too: used is a plain string,
// so the call allocates nothing then.
func (ss *showSources) recordField(name string, order []string, used string, value func(src string) string) {
	if ss.rec == nil {
		return
	}
	var list []string
	if used != "" {
		list = []string{used}
	}
	ss.record(name, order, list, false, value)
}

// recordArt is recordField for images: used is images()'s count per source,
// and with all every source was asked (their images are joined).
func (ss *showSources) recordArt(name string, order []string, used []sourceCount, all bool, value func(src string) string) {
	if ss.rec == nil {
		return
	}
	var list []string
	for _, c := range used {
		list = append(list, c.src)
	}
	ss.record(name, order, list, all, value)
}

func (ss *showSources) record(name string, order, used []string, all bool, value func(src string) string) {
	if order == nil {
		order = []string{} // X_SOURCES=none: an empty list in the page's data, not null
	}
	f := report.Field{Name: name, Order: order, Offered: map[string]string{}, Used: used}
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
