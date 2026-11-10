package provider

import (
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strconv"
	"strings"

	"kizuna/internal/anidb"
	"kizuna/internal/mapping"
)

// A [tvdb-N] tag names a whole TheTVDB series, but folders of its movies and
// OVAs, which Anime-Lists files in the series' specials, are often tagged
// with it too. Inside such a tag the show title and the file name pick the
// entry, when they clearly name one of those (specialByName).

// specialMargin: how many points a special-only entry's name score must beat
// the series' first entry's, and every other special-only entry's, by.
const specialMargin = 10

// byName is a special-only entry picked by name inside a TheTVDB id.
type byName struct {
	aid, score int
	note       string // for the match line and report: how, and how to be sure
}

// specialByName looks for the special-only entry (mapping.Entry.SpecialOnly)
// of a TheTVDB series that the show title (tags stripped) or the file name
// clearly names: at least MIN_SCORE, and specialMargin points over the
// series' first entry and every other special-only entry. Entries closer
// than that are told apart by Plex's year against the entry's; still tied,
// or anything weaker, and the series stays (ok false). Only for a series with more than one AniDB
// entry, and from the title dump alone: no AniDB calls.
func (p *Provider) specialByName(tvdbID int, query, file string, year int) (b byName, ok bool) {
	aids := p.lists.AIDsForTVDB(tvdbID)
	if len(aids) < 2 { // nothing to choose from
		return byName{}, false
	}
	names := []string{query, fileName(file)}
	first := p.lists.FirstAID(tvdbID)
	scores := map[int]int{} // the special-only entries' scores
	top := 0
	for _, aid := range aids {
		if aid != first && p.lists.ByAID(aid).SpecialOnly() {
			scores[aid] = p.nameScore(aid, names)
			top = max(top, scores[aid])
		}
	}
	if top < p.cfg.MinScore || top < p.nameScore(first, names)+specialMargin {
		return byName{}, false
	}
	var best []int // the entries within the margin of the top: told apart by the year
	for _, aid := range aids {
		if s, ok := scores[aid]; ok && s > top-specialMargin {
			best = append(best, aid)
		}
	}
	if len(best) > 1 {
		best = p.ofYear(best, nameYear(year, names))
	}
	if len(best) != 1 {
		return byName{}, false
	}
	return byName{aid: best[0], score: scores[best[0]], note: p.byNameNote(best[0])}, true
}

// nameScore is an entry's best score against any of names.
func (p *Provider) nameScore(aid int, names []string) int {
	titles := p.titles.Of(aid)
	best := 0
	for _, n := range names {
		best = max(best, anidb.Score(n, titles))
	}
	return best
}

// fileName is a file path's last part, without its extension and id tags
// ("" for none).
func fileName(file string) string {
	if i := strings.LastIndexAny(file, `/\`); i >= 0 {
		file = file[i+1:]
	}
	return tagRe.ReplaceAllString(strings.TrimSuffix(file, path.Ext(file)), "")
}

var titleYearRe = regexp.MustCompile(`\((?:19|20)\d\d\)`)

// nameYear is Plex's year, else a "(2012)" in one of the names (0 = none).
func nameYear(year int, names []string) int {
	for _, n := range names {
		if year > 0 {
			break
		}
		if m := titleYearRe.FindString(n); m != "" {
			year, _ = strconv.Atoi(m[1:5])
		}
	}
	return year
}

// ofYear keeps the entries of the year (all for year 0).
// An entry's year is its AniDB start date when the details are cached,
// else a "(2012)" in one of its titles.
func (p *Provider) ofYear(aids []int, year int) []int {
	if year <= 0 {
		return aids
	}
	want := strconv.Itoa(year)
	var out []int
	for _, aid := range aids {
		if p.entryYear(aid) == want {
			out = append(out, aid)
		}
	}
	return out
}

func (p *Provider) entryYear(aid int) string {
	if a, _ := p.anidb.Peek(aid); a != nil && len(a.StartDate) >= 4 {
		return a.StartDate[:4]
	}
	for _, t := range p.titles.Of(aid) {
		if m := titleYearRe.FindString(t.Text); m != "" {
			return m[1:5]
		}
	}
	return ""
}

// byNameNote says how the entry was picked and which tag makes it sure.
func (p *Provider) byNameNote(aid int) string {
	return fmt.Sprintf("by name: AniDB %d, may be wrong; use %s to be sure", aid, sureTags(p.lists.ByAID(aid)))
}

// sureTags are the id tags that name an entry exactly: its AniDB id, and
// its TheMovieDb movie when it has one.
func sureTags(e mapping.Entry) string {
	tags := fmt.Sprintf("[anidb-%d]", e.AID)
	if e.TMDBMovie > 0 {
		tags += fmt.Sprintf(" or [tmdb-%d]", e.TMDBMovie)
	}
	return tags
}

// specialRoot is specialByName for a match: the entry's show, with a warning
// (once per title every 10 minutes) that it may be wrong. src is the tag or
// GUID that named the series.
func (p *Provider) specialRoot(src, id, title, query, file string, year int) (candidate, string, bool) {
	tvdbID, err := strconv.Atoi(id)
	if err != nil || tvdbID <= 0 {
		return candidate{}, "", false
	}
	b, ok := p.specialByName(tvdbID, query, file, year)
	if !ok {
		return candidate{}, "", false
	}
	if p.nameLogged.due(title) {
		slog.Warn("match: matched to an AniDB entry by name inside a TheTVDB id; may be wrong", "title", title, "id", src,
			"aid", b.aid, "score", b.score, "to_be_sure", "use "+sureTags(p.lists.ByAID(b.aid)))
	}
	return candidate{fmt.Sprintf("anidb-%d", b.aid), b.score}, b.note, true
}
