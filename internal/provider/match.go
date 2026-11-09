package provider

import (
	"cmp"
	"context"
	"fmt"
	"kizuna/internal/anidb"
	"kizuna/internal/report"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MatchRequest is the body Plex POSTs to .../matches.
type MatchRequest struct {
	Type             int    `json:"type"` // 2 show, 3 season, 4 episode
	Title            string `json:"title"`
	ParentTitle      string `json:"parentTitle"`
	GrandparentTitle string `json:"grandparentTitle"`
	Year             int    `json:"year"`
	GUID             string `json:"guid"`
	Index            *int   `json:"index"`
	ParentIndex      *int   `json:"parentIndex"`
	Filename         string `json:"filename"`
	Manual           int    `json:"manual"`
}

var (
	// [anidb-123] / [tvdb-456] / {mal-21} / [imdb-tt0388629] in a folder or
	// file name (a digit after the source is allowed: [anidb2-123])
	tagRe = regexp.MustCompile(`(?i)[\[{](anidb|tvdb|tmdb|imdb|mal|anilist|kitsu|ann)\d?-(tt\d+|\d+)[\]}]`)
	// our own GUIDs (tv.plex.agents.custom.kizuna.anidb://show/anidb-1) and
	// other agents' anidb-N / tvdb-N GUIDs
	guidRe = regexp.MustCompile(`(?i)(?:/show/|agents\.hama://)(anidb|tvdb)\d?-(\d+)`)
	// other agents' GUIDs: com.plexapp.agents.thetvdb://81797?lang=en,
	// tmdb://37854, imdb://tt0388629, ...myanimelist://21
	otherGUIDRe = regexp.MustCompile(`(?i)(?:^|agents\.)(thetvdb|tvdb|themoviedb|tmdb|imdb|myanimelist|mal|anilist|kitsu|animenewsnetwork|ann)://(tt\d+|\d+)`)

	// idAliases: other names for a source in GUIDs.
	idAliases = map[string]string{"thetvdb": "tvdb", "themoviedb": "tmdb", "myanimelist": "mal", "animenewsnetwork": "ann"}
)

type candidate struct {
	root  string
	score int
}

// Match answers a Plex match request. Order of evidence (first wins):
//  1. an id tag in the folder or file name: [anidb-N], [tvdb-N], [mal-N]...
//  2. a GUID Plex already has
//  3. AniDB title search
//
// err is the show build's error when the best candidate couldn't be built
// (then items is empty): only a show that doesn't exist (noShow) is "no
// match"; any other failure answers 503.
func (p *Provider) Match(ctx context.Context, mode Mode, r MatchRequest) (items []Item, err error) {
	title := r.Title
	switch r.Type {
	case 3:
		title = r.ParentTitle
	case 4:
		title = r.GrandparentTitle
	}
	var noted *[]report.Candidate // the title search results, for the match report
	if p.reports != nil {
		noted = new([]report.Candidate)
	}
	cands, how := p.searchCandidates(mode, title, r, noted)
	attrs := []any{"mode", mode.Path, "type", matchType(r.Type), "title", title, "year", r.Year, "file", r.Filename, "guid", r.GUID, "manual", r.Manual == 1}
	var m *report.Match
	if noted != nil {
		m = p.recordMatch(mode, r, title, how, cands, *noted)
	}
	rq := report.RequestFrom(ctx) // nil with the reports off
	if len(cands) == 0 {
		slog.Info("match: nothing found", append(attrs, "how", how)...)
		if m != nil && r.Manual != 1 {
			key := p.reports.Unmatched(*m)
			if rq != nil {
				rq.Key = key
			}
		}
		if rq != nil {
			rq.Answer, rq.Note = "no match", how
		}
		return nil, nil
	}
	slog.Info("match", append(attrs, "show", cands[0].root, "score", cands[0].score, "how", how, "candidates", len(cands))...)
	if rq != nil {
		rq.Key = p.reportKey(cands[0].root)
		rq.Note = fmt.Sprintf("%s match: %s, score %d → %s", matchType(r.Type), how, cands[0].score, cands[0].root)
	}
	if r.Manual == 1 && r.Type <= 2 { // Plex "Fix Match" list: cheap items, no API calls
		if rq != nil {
			rq.Answer, rq.Note = "Fix Match list", fmt.Sprintf("%d candidates, best %s (score %d)", len(cands), cands[0].root, cands[0].score)
		}
		var out []Item
		for _, c := range cands {
			out = append(out, p.lightItem(mode, c))
		}
		return out, nil
	}
	root := cands[0].root
	if m != nil {
		if rq != nil {
			rq.Match = m
		}
		ctx = report.WithMatch(ctx, m)
	}
	s, err := p.show(ctx, root)
	if err != nil {
		slog.Debug("match: show not available", "root", root, "err", err)
		return nil, err
	}
	var it *Item
	switch r.Type {
	case 3:
		it = p.seasonItem(mode, s, deref(r.Index, 1), false)
	case 4:
		if ep := s.Episode(deref(r.ParentIndex, 1), deref(r.Index, 0)); ep != nil {
			e := p.episodeItem(mode, s, ep)
			it = &e
		}
	default:
		sh := p.showItem(mode, s, false)
		it = &sh
	}
	if it == nil { // the show has no such season or episode
		if rq != nil {
			rq.Answer = "no match"
		}
		return nil, nil
	}
	// Only after the Fix Match list (it skips MIN_SCORE, so it finds
	// candidates for titles that still don't match) and once there is an
	// item: until then the title still has no match in Plex.
	p.reports.Matched(title)
	return []Item{*it}, nil
}

// searchCandidates: an id tag in the show title or the file path (which
// includes the show folder) wins, then the GUID Plex already has, then title
// search. Only one id tag is allowed: with two different ones the show gets
// no match. In /auto, an AniDB or TVDB tag whose mode is off is no match.
// title is the show's title in r (Match picks it by r.Type); how says which
// of them decided. The first title search results are added to noted (when
// not nil) for the match report.
func (p *Provider) searchCandidates(mode Mode, title string, r MatchRequest, noted *[]report.Candidate) ([]candidate, string) {
	year, guid, manual := r.Year, r.GUID, r.Manual == 1
	query := tagRe.ReplaceAllString(title, "")
	switch tags := idTags(title, r.Filename); {
	case len(tags) > 1: // which one the user meant is a guess: no tag, GUID or title search
		names := make([]string, len(tags))
		for i, m := range tags {
			names[i] = m[0]
		}
		list := strings.Join(names, " ")
		if p.tagsLogged.due(title) {
			slog.Warn("match: more than one id tag; only one is allowed, the show gets no match", "title", title, "tags", list)
		}
		return nil, "id tags " + list + ": only one is allowed"
	case len(tags) == 1:
		m := tags[0]
		rules, need := p.tagRules(mode, m[1])
		if need != "" { // the tag is the user's choice: no other mode, GUID or title search
			if p.tagOffLogged.due(rules.Path) {
				slog.Warn("match: id tag needs /"+rules.Path+", which is off", "tag", m[0], "set", need)
			}
			return nil, fmt.Sprintf("tag %s: /%s is off, set %s", m[0], rules.Path, need)
		}
		if root := p.rootForID(rules, m[1], m[2], query, year); root != "" {
			return []candidate{{root, 100}}, "tag " + m[0]
		}
		slog.Debug("match: id tag not in the mapping", "tag", m[0])
	}
	for _, re := range []*regexp.Regexp{guidRe, otherGUIDRe} {
		if m := re.FindStringSubmatch(guid); m != nil {
			if root := p.rootForID(p.rules(mode, m[1]), m[1], m[2], query, year); root != "" {
				return []candidate{{root, 100}}, "guid " + guid
			}
			slog.Debug("match: GUID id not in the mapping", "guid", guid)
		}
	}

	limit := 1
	if manual {
		limit = 10
	}
	var out []candidate
	seen := map[string]bool{}
	titleRules := p.rules(mode, "")
	results := p.titles.Search(query, year, 20)
	for i, m := range results {
		if i < 5 {
			t := p.mainTitle(m.AID)
			slog.Debug("match: title search result", "query", query, "year", year, "rank", i+1, "aid", m.AID, "score", m.Score, "title", t)
			if noted != nil {
				*noted = append(*noted, report.Candidate{AID: m.AID, Title: t, Score: m.Score})
			}
		}
		if !manual && m.Score < p.cfg.MinScore {
			slog.Debug("match: below MIN_SCORE", "aid", m.AID, "score", m.Score, "min", p.cfg.MinScore)
			break
		}
		root := p.rootFor(titleRules, "anidb", m.AID)
		if root != "" && !seen[root] {
			seen[root] = true
			out = append(out, candidate{root, m.Score})
		}
		if len(out) >= limit {
			break
		}
	}
	if len(results) == 0 {
		return nil, "title search: no similar title"
	}
	if len(out) == 0 {
		return nil, fmt.Sprintf("title search: best score %d below MIN_SCORE %d", results[0].Score, p.cfg.MinScore)
	}
	return out, "title search"
}

// idTags returns the different id tags in texts (tagRe submatches), in
// order. A tag written again, as the show folder's tag in the file path or
// in the file name, is the same tag: same site and id, whatever the
// brackets, case, site digit ([tvdb3-…]) or leading zeros.
func idTags(texts ...string) [][]string {
	var out [][]string
	var seen []string
	for _, text := range texts {
		for _, m := range tagRe.FindAllStringSubmatch(text, -1) {
			id := strings.ToLower(m[2])
			if n, err := strconv.Atoi(id); err == nil {
				id = strconv.Itoa(n)
			}
			if key := strings.ToLower(m[1]) + "-" + id; !slices.Contains(seen, key) {
				seen = append(seen, key)
				out = append(out, m)
			}
		}
	}
	return out
}

var mainLang = []string{"main"}

// mainTitle is an AniDB entry's main title.
func (p *Provider) mainTitle(aid int) string {
	return anidb.PickTitle(p.titles.Of(aid), mainLang, false)
}

// rules returns the mode whose rules a GUID or the title search follows.
// /anidb and /tvdb always use their own. /auto lets an AniDB id bring the
// /anidb rules and a TVDB id the /tvdb rules; other ids and title search use
// AUTO_DEFAULT. A mode that is off (no AniDB client, no TVDB key) gives way
// to the other one, which rootFor turns the id into.
func (p *Provider) rules(mode Mode, source string) Mode {
	if !mode.Auto {
		return mode
	}
	want, other := Modes["anidb"], Modes["tvdb"]
	switch strings.ToLower(source) {
	case "anidb":
	case "tvdb", "thetvdb":
		want, other = other, want
	default:
		if p.cfg.AutoDefault == "tvdb" {
			want, other = other, want
		}
	}
	if p.modeOff(want) != "" && p.modeOff(other) == "" {
		return other
	}
	return want
}

// tagRules is rules for an id tag. In /auto an [anidb-N] or [tvdb-N] tag
// always brings its own site's rules, without giving way: when that mode is
// off, need names what to set and the show gets no match. Other tags follow
// rules.
func (p *Provider) tagRules(mode Mode, source string) (rules Mode, need string) {
	if !mode.Auto {
		return mode, ""
	}
	switch strings.ToLower(source) {
	case "anidb":
		rules, need = Modes["anidb"], "ANIDB_CLIENT"
	case "tvdb":
		rules, need = Modes["tvdb"], "TVDB_API_KEY or TVDB_LEGACY_API_KEY"
	default:
		return p.rules(mode, source), ""
	}
	if p.modeOff(rules) == "" {
		need = ""
	}
	return rules, need
}

func matchType(t int) string {
	switch t {
	case 2:
		return "show"
	case 3:
		return "season"
	case 4:
		return "episode"
	}
	return strconv.Itoa(t)
}

// rootForID turns any site's id into the show key used by a mode. AniDB
// and TVDB ids are used directly; the others (TMDB, IMDb, MAL, AniList,
// Kitsu, ANN) are mapped to their AniDB entry first (see aidFor; title and
// year only settle a TMDB id). "" if unknown.
func (p *Provider) rootForID(mode Mode, source, id, title string, year int) string {
	source, id = strings.ToLower(source), strings.TrimSpace(id)
	if to, ok := idAliases[source]; ok {
		source = to
	}
	if source == "imdb" && !strings.HasPrefix(id, "tt") {
		return ""
	}
	n, err := strconv.Atoi(id)
	switch {
	case source == "anidb" || source == "tvdb":
		if err != nil || n <= 0 {
			return ""
		}
		return p.rootFor(mode, source, n)
	case source == "imdb" || (err == nil && n > 0):
		if aid := p.aidFor(source, id, title, year); aid > 0 {
			root := p.rootFor(mode, "anidb", aid)
			slog.Debug("match: id mapped", "source", source, "id", id, "aid", aid, "show", root)
			return root
		}
	}
	return ""
}

// aidFor maps another site's id to its AniDB entry (0 if unknown).
//
// A TMDB id (tag or GUID) doesn't say whether it is a TV show or a movie,
// and TheMovieDb numbers both from 1, so one id can be an anime's TV show
// and another anime's movie. It means the TV show (as {tmdb-N} does in a
// Plex TV library) unless only a movie has it. When both have it, the
// title search picks the one whose entry it finds first; when it finds
// neither, the TV show wins.
func (p *Provider) aidFor(source, id, title string, year int) int {
	if source != "tmdb" {
		return p.lists.AIDFor(source, id)
	}
	tv, movie := p.lists.AIDFor("tmdb", id), p.lists.AIDFor("tmdbmovie", id)
	if tv == 0 || movie == 0 || tv == movie {
		return cmp.Or(tv, movie)
	}
	n, _ := strconv.Atoi(id)
	for _, m := range p.titles.Search(title, year, 20) {
		switch e := p.lists.ByAID(m.AID); {
		case e.TMDB.ID == n:
			return tv
		case e.TMDBMovie == n:
			slog.Debug("match: TMDB id is a TV show and a movie; the title search found the movie", "id", id, "aid", m.AID)
			return m.AID
		}
	}
	return tv
}

// rootFor turns a source id into the show key used by a mode.
//
//	anidb mode: AniDB ids as-is; a TVDB id becomes its first AniDB entry.
//	tvdb mode:  TVDB ids as-is; an AniDB id becomes its TVDB series, or stays
//	            an AniDB show when it has none (movies, many OVAs).
func (p *Provider) rootFor(mode Mode, source string, id int) string {
	switch {
	case source == "anidb" && mode.TVDB:
		if e := p.lists.ByAID(id); e.TVDB.ID > 0 {
			return fmt.Sprintf("tvdb-%d", e.TVDB.ID)
		}
		return fmt.Sprintf("anidb-%d", id)
	case source == "anidb":
		return fmt.Sprintf("anidb-%d", id)
	case mode.TVDB:
		return fmt.Sprintf("tvdb-%d", id)
	default:
		if aid := p.lists.FirstAID(id); aid > 0 {
			return fmt.Sprintf("anidb-%d", aid)
		}
	}
	return ""
}

func deref(n *int, def int) int {
	if n == nil {
		return def
	}
	return *n
}
