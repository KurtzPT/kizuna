package provider

import (
	"cmp"
	"context"
	"fmt"
	"kizuna/internal/anidb"
	"kizuna/internal/report"
	"log/slog"
	"regexp"
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
// (then items is empty): a timed-out one answers 503, not "no match".
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
	// Only after the Fix Match list (it skips MIN_SCORE, so it finds
	// candidates for titles that still don't match) and once the show is
	// there: until then the title still has no match in Plex.
	p.reports.Matched(title)
	switch r.Type {
	case 3:
		if it := p.seasonItem(mode, s, deref(r.Index, 1), false); it != nil {
			return []Item{*it}, nil
		}
	case 4:
		if ep := s.Episode(deref(r.ParentIndex, 1), deref(r.Index, 0)); ep != nil {
			return []Item{p.episodeItem(mode, s, ep)}, nil
		}
	default:
		return []Item{p.showItem(mode, s, false)}, nil
	}
	return nil, nil
}

// searchCandidates: an id tag in the show title or the file path (which
// includes the show folder) wins, then the GUID Plex already has, then title
// search. title is the show's title in r (Match picks it by r.Type); how
// says which of them decided. The first title search results are added to
// noted (when not nil) for the match report.
func (p *Provider) searchCandidates(mode Mode, title string, r MatchRequest, noted *[]report.Candidate) ([]candidate, string) {
	year, guid, manual := r.Year, r.GUID, r.Manual == 1
	query := tagRe.ReplaceAllString(title, "")
	for _, text := range []string{title, r.Filename} {
		if m := tagRe.FindStringSubmatch(text); m != nil {
			if root := p.rootForID(p.rules(mode, m[1]), m[1], m[2], query, year); root != "" {
				return []candidate{{root, 100}}, "tag " + m[0]
			}
			slog.Debug("match: id tag not in the mapping", "tag", m[0])
		}
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

var mainLang = []string{"main"}

// mainTitle is an AniDB entry's main title.
func (p *Provider) mainTitle(aid int) string {
	return anidb.PickTitle(p.titles.Of(aid), mainLang, false)
}

// rules returns the mode whose rules a piece of evidence follows. /anidb
// and /tvdb always use their own. /auto lets an AniDB id (tag or GUID)
// bring the /anidb rules and a TVDB id the /tvdb rules; other ids and title
// search use AUTO_DEFAULT. A mode that is off (no AniDB client, no TVDB
// key) gives way to the other one, which rootFor turns the id into.
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
