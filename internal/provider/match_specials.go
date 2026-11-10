package provider

import (
	"fmt"
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
	sure       string // the id tags that name it exactly (sureTags)
}

// specialByName looks for the special-only entry (mapping.Entry.SpecialOnly)
// of a TheTVDB series that the show title (tags stripped) or the file name
// clearly names: at least MIN_SCORE, and specialMargin points over the
// series' first entry and every other special-only entry. Entries closer
// than that are told apart by Plex's year against the entry's; still tied,
// or anything weaker, and the series stays (ok false). Only for a series with more than one AniDB
// entry, and from the title dump alone: no AniDB calls.
func (m matcher) specialByName(tvdbID int, query, file string, year int) (b byName, ok bool) {
	aids := m.lists.AIDsForTVDB(tvdbID)
	if len(aids) < 2 { // nothing to choose from
		return byName{}, false
	}
	names := []string{query, fileName(file)}
	first := m.lists.FirstAID(tvdbID)
	scores := map[int]int{} // the special-only entries' scores
	top := 0
	for _, aid := range aids {
		if aid != first && m.lists.ByAID(aid).SpecialOnly() {
			scores[aid] = m.nameScore(aid, names)
			top = max(top, scores[aid])
		}
	}
	if top < m.minScore || top < m.nameScore(first, names)+specialMargin {
		return byName{}, false
	}
	var best []int // the entries within the margin of the top: told apart by the year
	for _, aid := range aids {
		if s, ok := scores[aid]; ok && s > top-specialMargin {
			best = append(best, aid)
		}
	}
	if len(best) > 1 {
		best = m.ofYear(best, nameYear(year, names))
	}
	if len(best) != 1 {
		return byName{}, false
	}
	return byName{aid: best[0], score: scores[best[0]], sure: sureTags(m.lists.ByAID(best[0]))}, true
}

// nameScore is an entry's best score against any of names.
func (m matcher) nameScore(aid int, names []string) int {
	titles := m.titles.Of(aid)
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
func (m matcher) ofYear(aids []int, year int) []int {
	if year <= 0 {
		return aids
	}
	want := strconv.Itoa(year)
	var out []int
	for _, aid := range aids {
		if m.entryYear(aid) == want {
			out = append(out, aid)
		}
	}
	return out
}

func (m matcher) entryYear(aid int) string {
	if m.peek != nil {
		if a, _ := m.peek.Peek(aid); a != nil && len(a.StartDate) >= 4 {
			return a.StartDate[:4]
		}
	}
	for _, t := range m.titles.Of(aid) {
		if y := titleYearRe.FindString(t.Text); y != "" {
			return y[1:5]
		}
	}
	return ""
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

// specialRoot is specialByName for the id a tag or GUID gives (ok false
// when it is no TheTVDB id).
func (m matcher) specialRoot(id, query, file string, year int) (byName, bool) {
	tvdbID, err := strconv.Atoi(id)
	if err != nil || tvdbID <= 0 {
		return byName{}, false
	}
	return m.specialByName(tvdbID, query, file, year)
}
