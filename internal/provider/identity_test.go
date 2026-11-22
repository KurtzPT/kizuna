package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"kizuna/internal/anidb"
	"kizuna/internal/mapping"
)

// What Plex keeps of a show is its identity: the GUID and rating key the
// provider names it by, its type, and the external GUIDs (tvdb://, tmdb://,
// imdb://) on the show and on each episode paired with a TheTVDB episode (a
// special-only show's). Plex matches and merges by them and keeps them when
// a refresh brings other titles or summaries, so a build made while AniDB's
// details are missing (no ANIDB_CLIENT, a ban, a 24 h block, a backoff, a
// failing call) must name the show the same as the build with them. The
// mapping alone decides which show it is.
//
// The grid below builds each kind of mapping entry under every way the
// details can be missing and with them (a TV series and a movie), asks every
// mode (the Fix Match answers by title and by id tags, and the show's own
// pages, as Plex reads them over HTTP) and compares the answers with the
// build with a TV series' details. A comparison passes when both builds are
// wrong the same way, so identityWants also pins the show's GUIDs, S01E01's
// GUIDs and whether it is the movie, for some cells, to values written out
// from the requirement.

// The ids of the grid's entry.
const (
	identityAID                                     = 100
	identityTVDB, identityTMDBTV, identityTMDBMovie = 500, 77, 99
	identityIMDB                                    = "tt0000001"
)

// identityEntry is one kind of mapping entry; the grid below crosses them.
type identityEntry struct {
	name  string
	entry mapping.Entry
	// others: entries of the same TheTVDB series, for the kinds that are
	// not the first.
	others []mapping.Entry
}

// identityEntries is the grid of entries: where the entry sits on TheTVDB
// (none, first, a later entry, absolute order, specials only, specials with
// an episode moved to a regular season, specials with an episode that has no
// counterpart) crossed with its TheMovieDb TV show (none, an id only, a
// mapped season) and a TheMovieDb movie id (not with a mapped season): 40
// kinds. IMDb only adds an imdb:// GUID, so two more kinds carry it. No kind
// has Ranges, which SpecialOnly also reads.
func identityEntries() []identityEntry {
	tv := func(season, offset int, eps ...mapping.EpisodeMap) mapping.Placement {
		return mapping.Placement{ID: identityTVDB, Season: season, Offset: offset, Mapped: true, Episodes: eps}
	}
	first := mapping.Entry{AID: 50, TVDB: tv(1, 0), TMDB: mapping.Placement{ID: identityTMDBTV}}
	tvdbKinds := []struct {
		name   string
		p      mapping.Placement
		others []mapping.Entry
	}{
		{"no TVDB series", mapping.Placement{}, nil},
		{"first entry", tv(1, 0), nil},
		{"later entry", tv(1, 12), []mapping.Entry{first}},
		{"later season", tv(2, 0), []mapping.Entry{first}},
		{"absolute order", tv(-1, 0), nil},
		{"special-only", tv(0, 0), nil},
		{"specials with an episode moved to a season", tv(0, 0, mapping.EpisodeMap{AniDBSeason: 1, AniDBEp: 1, Season: 1, Ep: 3}), nil},
		{"specials with an episode without a counterpart", tv(0, 0, mapping.EpisodeMap{AniDBSeason: 1, AniDBEp: 1, Season: 0, Ep: 0}), nil},
	}
	tmdbKinds := []struct {
		name string
		p    mapping.Placement
	}{
		{"no TMDB show", mapping.Placement{}},
		{"TMDB show id", mapping.Placement{ID: identityTMDBTV}},
		{"TMDB show season", mapping.Placement{ID: identityTMDBTV, Season: 1, Mapped: true}},
	}
	var out []identityEntry
	add := func(name string, tvdb, tmdb mapping.Placement, others []mapping.Entry, movieID int, imdb string) {
		if movieID > 0 {
			name += ", TMDB movie"
		}
		if imdb != "" {
			name += ", IMDb"
		}
		out = append(out, identityEntry{name: name, others: others, entry: mapping.Entry{
			AID: identityAID, TVDB: tvdb, TMDB: tmdb, TMDBMovie: movieID, IMDB: imdb}})
	}
	for _, tk := range tvdbKinds {
		for _, mk := range tmdbKinds {
			for _, movieID := range []int{0, identityTMDBMovie} {
				if movieID > 0 && mk.p.Mapped {
					continue // a movie is told by the TV show's id alone, not its season
				}
				add(tk.name+", "+mk.name, tk.p, mk.p, tk.others, movieID, "")
			}
		}
	}
	add("first entry, TMDB show id", tv(1, 0), mapping.Placement{ID: identityTMDBTV}, nil, 0, identityIMDB)
	add("special-only, no TMDB show", tv(0, 0), mapping.Placement{}, nil, identityTMDBMovie, identityIMDB)
	return out
}

// identityWant is what a cell gives in every build, written out from the
// requirement, not from the code: the show's GUIDs, S01E01's GUIDs (a
// special-only show's episode carries TheTVDB's episode's: 14 in the fake
// series) and whether S01E01 is the movie, under the show's title. A build
// without details knows a movie from the mapping alone, and a build with a
// movie's details has the movie whatever the mapping says.
type identityWant struct {
	guids, episodeGuids []string
	movie               bool
}

var identityWants = map[string]identityWant{
	// The five movies that had their own table: the mapping says whether the
	// show is TheMovieDb's movie (a movie id, and no TV show id or no regular
	// TheTVDB season), not AniDB.
	"special-only, no TMDB show, TMDB movie":   {[]string{"tmdb://99"}, []string{"tvdb://14"}, true},
	"special-only, TMDB show id, TMDB movie":   {[]string{"tmdb://99"}, []string{"tvdb://14"}, true},
	"no TVDB series, TMDB show id, TMDB movie": {[]string{"tmdb://99"}, nil, true},
	"later season, TMDB show id, TMDB movie":   {[]string{"tvdb://500", "tmdb://77"}, nil, false},
	"later season, no TMDB show, TMDB movie":   {[]string{"tmdb://99"}, nil, true},
	// A special-only show is not its series: no tvdb://500 on the show, and
	// the episode has its own tvdb://.
	"special-only, TMDB show id":                   {[]string{"tmdb://77"}, []string{"tvdb://14"}, false},
	"special-only, no TMDB show, TMDB movie, IMDb": {[]string{"tmdb://99", "imdb://tt0000001"}, []string{"tvdb://14"}, true},
	// A regular entry has its series' GUID and no episode GUIDs.
	"first entry, TMDB show id":       {[]string{"tvdb://500", "tmdb://77"}, nil, false},
	"first entry, TMDB show id, IMDb": {[]string{"tvdb://500", "tmdb://77", "imdb://tt0000001"}, nil, false},
}

// identityState is one way AniDB's details are missing, or present.
type identityState struct {
	name      string
	anidbType string // the type AniDB's answer gives; "" = no details
	reference bool   // the build the others are compared with
	client    string // ANIDB_CLIENT
	// answer: AniDB's answer to a call for aid; nil = a 500.
	answer func(aid int) (code int, body string)
	state  anidbState // the state file the client starts from
}

func identityStates() []identityState {
	const client = "kizunatest"
	return []identityState{
		{name: "no ANIDB_CLIENT"},
		{name: "backoff", client: client, state: anidbBackingOff()},
		{name: "ban", client: client, answer: func(int) (int, string) { return 200, "<error>banned</error>" }},
		{name: "24 h block", client: client, answer: func(int) (int, string) { return 200, "<error>Anime not found</error>" }},
		{name: "failing call", client: client},
		{name: "details: TV series", anidbType: "TV Series", reference: true, client: client, answer: func(aid int) (int, string) {
			return 200, strings.Replace(anidbXML, `id="100"`, fmt.Sprintf(`id="%d"`, aid), 1)
		}},
		{name: "details: movie", anidbType: "Movie", client: client, answer: func(aid int) (int, string) {
			return 200, movieXML(aid, "Kizuna Test", "The movie.", 90, "2010-05-01")
		}},
	}
}

// identityProvider is a provider on the fake transport with the entries
// loaded and AniDB in the given state.
func identityProvider(t *testing.T, c identityEntry, st identityState) *Provider {
	t.Helper()
	net := &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		switch path {
		case "api.anidb.net/httpapi":
			if st.answer == nil {
				return 500, ""
			}
			aid, err := strconv.Atoi(q["aid"][0])
			if err != nil {
				t.Errorf("AniDB asked for aid %q", q["aid"][0])
			}
			return st.answer(aid)
		case "api.themoviedb.org/3/movie/" + strconv.Itoa(identityTMDBMovie):
			return 200, `{"title":"TMDB Movie","overview":"From TMDB's movie."}`
		case "api.themoviedb.org/3/movie/" + strconv.Itoa(identityTMDBMovie) + "/images":
			return 200, `{"posters":[{"file_path":"/movie.jpg"}]}`
		}
		return testSeries(path, q)
	}}
	p := newTestProvider(t, net)
	p.anidb = anidbIn(t, t.TempDir(), net, st.client, st.state)
	entries := map[int]mapping.Entry{c.entry.AID: c.entry}
	titles := map[int][]anidb.Title{c.entry.AID: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test"}}}
	for _, o := range c.others {
		entries[o.AID] = o
		titles[o.AID] = []anidb.Title{{Type: "main", Lang: "x-jat", Text: "Kizuna Other"}}
	}
	p.lists.SetForTest(entries)
	p.titles.SetForTest(titles)
	return p
}

// plexAsk sends one request as Plex does and returns the answer's items and
// status; a 200 that isn't JSON fails the test.
func plexAsk(t *testing.T, p *Provider, method, path, body string) ([]Item, int) {
	t.Helper()
	rec := plexRequest(context.Background(), p, method, path, body)
	if rec.Code != 200 {
		return nil, rec.Code
	}
	var c Container
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return c.MediaContainer.Metadata, rec.Code
}

func guidIDs(it Item) []string {
	out := make([]string, len(it.Guid))
	for i, g := range it.Guid {
		out[i] = g.ID
	}
	return out
}

// identityOf lists what Plex would keep of the entry's show, asked through
// every mode, one line per fact. Two lists that agree say the builds agree,
// not what the identity is: that is identityWants'. Movie-ness and the
// season items are not in the lines.
func identityOf(t *testing.T, p *Provider, c identityEntry, anidbOn bool) []string {
	t.Helper()
	var lines []string
	ask := func(method, path, body string) []Item {
		items, code := plexAsk(t, p, method, path, body)
		if code != 200 {
			lines = append(lines, fmt.Sprintf("%s %s: status %d", method, path, code))
		}
		return items
	}
	show := func(label string, it Item) {
		lines = append(lines, fmt.Sprintf("%s: %s %q %s %s guids %v", label, it.Type, it.RatingKey, it.Key, it.GUID, guidIDs(it)))
	}
	aid := fmt.Sprintf("anidb-%d", identityAID)
	tags := []string{"{" + aid + "}"}
	roots := []string{aid}
	if c.entry.TVDB.ID > 0 {
		tags = append(tags, fmt.Sprintf("{tvdb-%d}", identityTVDB))
		roots = append(roots, fmt.Sprintf("tvdb-%d", identityTVDB))
	}
	if c.entry.TMDB.ID > 0 {
		tags = append(tags, fmt.Sprintf("{tmdb-%d}", identityTMDBTV))
	}
	if c.entry.TMDBMovie > 0 {
		tags = append(tags, fmt.Sprintf("{tmdb-%d}", identityTMDBMovie))
	}
	if c.entry.IMDB != "" {
		tags = append(tags, "{imdb-"+c.entry.IMDB+"}")
	}
	for _, mode := range []string{"anidb", "tvdb", "auto"} {
		if mode == "anidb" && !anidbOn {
			continue // the mode is off without ANIDB_CLIENT: 503, whatever the entry
		}
		// /auto matches through /tvdb alone when /anidb is off: another mode's key.
		if mode != "auto" || anidbOn {
			for _, tag := range append([]string{""}, tags...) {
				form := url.Values{"type": {"2"}, "title": {strings.TrimSpace("Kizuna Test " + tag)}}.Encode()
				label := fmt.Sprintf("/%s match %q", mode, tag)
				items := ask("POST", "/"+mode+"/library/metadata/matches", form)
				if len(items) == 0 {
					lines = append(lines, label+": no match")
				}
				for _, it := range items {
					show(label, it)
				}
			}
		}
		for _, root := range roots {
			base := "/" + mode + "/library/metadata/" + root
			label := fmt.Sprintf("/%s %s", mode, root)
			if items := ask("GET", base, ""); len(items) > 0 {
				show(label, items[0])
			}
			for _, ep := range ask("GET", base+"/grandchildren", "") {
				if len(ep.Guid) > 0 { // only a special-only show's episodes carry GUIDs
					lines = append(lines, fmt.Sprintf("%s S%02dE%02d: %v", label, *ep.ParentIndex, *ep.Index, guidIDs(ep)))
				}
			}
		}
	}
	return lines
}

// checkIdentityWant checks the show's page and S01E01's against w, in every
// mode that is on.
func checkIdentityWant(t *testing.T, p *Provider, st identityState, w identityWant) {
	modes := []string{"tvdb"}
	if st.client != "" {
		modes = append(modes, "anidb")
	}
	for _, mode := range modes {
		get := func(key string) Item {
			items, code := plexAsk(t, p, "GET", "/"+mode+"/library/metadata/"+key, "")
			if len(items) != 1 {
				t.Fatalf("%s: /%s %s: status %d, %d items", st.name, mode, key, code, len(items))
			}
			return items[0]
		}
		show, ep := get(fmt.Sprintf("anidb-%d", identityAID)), get(fmt.Sprintf("anidb-%d-s1-e1", identityAID))
		if g := guidIDs(show); !slices.Equal(g, w.guids) {
			t.Errorf("%s: /%s show guids %v, want %v", st.name, mode, g, w.guids)
		}
		if g := guidIDs(ep); !slices.Equal(g, w.episodeGuids) {
			t.Errorf("%s: /%s S01E01 guids %v, want %v", st.name, mode, g, w.episodeGuids)
		}
		// A TV series' two episodes are no movie, whatever the mapping says.
		if (st.anidbType == "Movie" || (w.movie && st.anidbType == "")) && ep.Title != show.Title {
			t.Errorf("%s: /%s S01E01 is %q, want the movie under the show's title %q", st.name, mode, ep.Title, show.Title)
		}
	}
}

func TestIdentityIsTheSameWithAndWithoutAniDBDetails(t *testing.T) {
	t.Cleanup(captureLogs(io.Discard))
	states := identityStates()
	ref := states[slices.IndexFunc(states, func(s identityState) bool { return s.reference })]
	pinned := map[string]bool{}
	for _, c := range identityEntries() {
		w, isPinned := identityWants[c.name]
		pinned[c.name] = true
		t.Run(c.name, func(t *testing.T) {
			// Parallel is safe while the logs are captured above, no subtest
			// sets an environment variable and none changes a fetch global
			// (build_test.go warns the logger is global).
			t.Parallel()
			refP := identityProvider(t, c, ref)
			if len(c.others) > 0 {
				// The first read starts the other entry's fetch in the
				// background: let it land, so the reference is read the same
				// way every run.
				identityOf(t, refP, c, true)
				refP.anidb.WaitIdle()
			}
			wantOn, wantOff := identityOf(t, refP, c, true), identityOf(t, refP, c, false)
			for _, st := range states {
				p := refP
				if st.name != ref.name {
					p = identityProvider(t, c, st)
					want := wantOn
					if st.client == "" {
						want = wantOff
					}
					if got := identityOf(t, p, c, st.client != ""); !slices.Equal(got, want) {
						t.Errorf("%s: identity differs from %s:\n%s", st.name, ref.name, identityDiff(want, got))
					}
				}
				if isPinned {
					checkIdentityWant(t, p, st, w)
				}
			}
		})
	}
	for name := range identityWants {
		if !pinned[name] {
			t.Errorf("identityWants has %q, which is no cell of the grid", name)
		}
	}
}

// identityDiff shows the lines only one side has.
func identityDiff(want, got []string) string {
	var out []string
	for _, l := range got {
		if !slices.Contains(want, l) {
			out = append(out, "  now:  "+l)
		}
	}
	for _, l := range want {
		if !slices.Contains(got, l) {
			out = append(out, "  with details: "+l)
		}
	}
	if len(out) > 6 {
		out = append(out[:6], fmt.Sprintf("  ... %d more", len(out)-6))
	}
	return strings.Join(out, "\n")
}
