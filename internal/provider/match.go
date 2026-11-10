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
	Type             int // 2 show, 3 season, 4 episode
	Title            string
	ParentTitle      string
	GrandparentTitle string
	Year             int
	GUID             string
	Index            *int
	ParentIndex      *int
	Filename         string
	Manual           int
	// IncludeChildren 1: a show or season answer carries its seasons or
	// episodes inline, as .../children lists them.
	IncludeChildren int
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
// A manual match (Plex's Fix Match) lists the id match first, then the
// title search results for the typed title.
//
// err is the show build's error when the best candidate couldn't be built
// (then items is empty): only a show that doesn't exist (noShow) is "no
// match"; any other failure answers 503.
func (p *Provider) Match(ctx context.Context, mode Mode, r MatchRequest) (items []Item, err error) {
	list := r.fixMatchList()
	d := p.decide(mode, r, p.reports != nil && !list) // a Fix Match list is in no report
	title, cands, how := d.title, d.cands, d.how()
	attrs := []any{"mode", mode.Path, "type", matchType(r.Type), "title", title, "year", r.Year, "file", r.Filename, "guid", r.GUID, "manual", r.Manual == 1}
	var m *report.Match
	if p.reports != nil && !list {
		m = p.recordMatch(mode, r, d)
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
	if list { // Plex "Fix Match" list: cheap items, no API calls
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
		ctx = report.WithMatch(ctx, m) // the build's report copies it as it is now
	}
	s, err := p.show(ctx, root)
	if err != nil {
		// No rq.Match: the report's latest match would say Plex got the show.
		// The request itself says it wasn't available.
		slog.Debug("match: show not available", "root", root, "err", err)
		return nil, err
	}
	var it *Item
	withChildren := r.IncludeChildren == 1
	switch r.Type {
	case 3:
		it = p.seasonItem(mode, s, deref(r.Index, 1), withChildren)
	case 4:
		if ep := s.Episode(deref(r.ParentIndex, 1), deref(r.Index, 0)); ep != nil {
			e := p.episodeItem(mode, s, ep)
			it = &e
		}
	default:
		sh := p.showItem(mode, s, withChildren)
		it = &sh
	}
	if it == nil { // the show has no such season or episode
		if rq != nil {
			rq.Answer = "no match"
			if m != nil { // the latest match says so, not that Plex got the show
				m.How += "; " + root + " has no " + missingPart(r)
				m.Root, m.Score = "", 0
				rq.Match = m
			}
		}
		return nil, nil
	}
	if rq != nil {
		rq.Match = m
	}
	// Only after the Fix Match list (it skips MIN_SCORE, so it finds
	// candidates for titles that still don't match) and once there is an
	// item: until then the title still has no match in Plex.
	p.reports.Matched(title)
	return []Item{*it}, nil
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
func (p *Provider) mainTitle(aid int) string { return mainTitle(p.titles, aid) }

func mainTitle(titles *anidb.Titles, aid int) string {
	return anidb.PickTitle(titles.Of(aid), mainLang, false)
}

// rules returns the mode whose rules a GUID or the title search follows.
// /anidb and /tvdb always use their own. /auto lets an AniDB id bring the
// /anidb rules and a TVDB id the /tvdb rules; other ids and title search use
// AUTO_DEFAULT. A mode that is off (no AniDB client, no TVDB key) gives way
// to the other one, which rootFor turns the id into.
func (m matcher) rules(mode Mode, source string) Mode {
	if !mode.Auto {
		return mode
	}
	want, other := Modes["anidb"], Modes["tvdb"]
	switch strings.ToLower(source) {
	case "anidb":
	case "tvdb", "thetvdb":
		want, other = other, want
	default:
		if m.autoDefault == "tvdb" {
			want, other = other, want
		}
	}
	if m.modeOff(want) != "" && m.modeOff(other) == "" {
		return other
	}
	return want
}

// tagRules is rules for an id tag. In /auto an [anidb-N] or [tvdb-N] tag
// always brings its own site's rules, without giving way: when that mode is
// off, need names what to set and the show gets no match. Other tags follow
// rules.
func (m matcher) tagRules(mode Mode, source string) (rules Mode, need string) {
	if !mode.Auto {
		return mode, ""
	}
	switch strings.ToLower(source) {
	case "anidb":
		rules, need = Modes["anidb"], "ANIDB_CLIENT"
	case "tvdb":
		rules, need = Modes["tvdb"], "TVDB_API_KEY or TVDB_LEGACY_API_KEY"
	default:
		return m.rules(mode, source), ""
	}
	if m.modeOff(rules) == "" {
		need = ""
	}
	return rules, need
}

// missingPart names the season or episode a season or episode match asked
// for, for a show that lacks it.
func missingPart(r MatchRequest) string {
	if r.Type == 4 {
		return fmt.Sprintf("episode S%02dE%02d", deref(r.ParentIndex, 1), deref(r.Index, 0))
	}
	return fmt.Sprintf("season %d", deref(r.Index, 1))
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
func (m matcher) rootForID(mode Mode, source, id, title string, year int) string {
	source, id = strings.ToLower(source), strings.ToLower(id) // [imdb-TT1] is tt1
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
		return m.rootFor(mode, source, n)
	case source == "imdb" || (err == nil && n > 0):
		if aid := m.aidFor(source, id, title, year); aid > 0 {
			root := m.rootFor(mode, "anidb", aid)
			if debugOn(context.Background()) {
				slog.Debug("match: id mapped", "source", source, "id", id, "aid", aid, "show", root)
			}
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
func (m matcher) aidFor(source, id, title string, year int) int {
	if source != "tmdb" {
		return m.lists.AIDFor(source, id)
	}
	tv, movie := m.lists.AIDFor("tmdb", id), m.lists.AIDFor("tmdbmovie", id)
	if tv == 0 || movie == 0 || tv == movie {
		return cmp.Or(tv, movie)
	}
	n, _ := strconv.Atoi(id)
	for _, res := range m.titles.Search(title, year, 20) {
		switch e := m.lists.ByAID(res.AID); {
		case e.TMDB.ID == n:
			return tv
		case e.TMDBMovie == n:
			slog.Debug("match: TMDB id is a TV show and a movie; the title search found the movie", "id", id, "aid", res.AID)
			return res.AID
		}
	}
	return tv
}

// rootFor turns a source id into the show key used by a mode.
//
//	anidb mode: AniDB ids as-is; a TVDB id becomes its first AniDB entry.
//	tvdb mode:  TVDB ids as-is; an AniDB id becomes its TVDB series, or stays
//	            an AniDB show when it has none (movies, many OVAs) or lives
//	            only in the series' specials (mapping.Entry.SpecialOnly).
func (m matcher) rootFor(mode Mode, source string, id int) string {
	switch {
	case source == "anidb" && mode.TVDB:
		if e := m.lists.ByAID(id); e.TVDB.ID > 0 && !e.SpecialOnly() {
			return fmt.Sprintf("tvdb-%d", e.TVDB.ID)
		}
		return fmt.Sprintf("anidb-%d", id)
	case source == "anidb":
		return fmt.Sprintf("anidb-%d", id)
	case mode.TVDB:
		return fmt.Sprintf("tvdb-%d", id)
	default:
		if aid := m.lists.FirstAID(id); aid > 0 {
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
