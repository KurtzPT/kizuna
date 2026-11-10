package provider

import (
	"reflect"
	"strings"
	"testing"

	"kizuna/internal/anidb"
	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/sources"
)

// howForms is every form a decision's reason takes, with its wording. Plex
// users read these lines in the log and on the report page: they must not
// change by accident.
func howForms() []struct {
	name string
	d    decision
	how  string
} {
	const guid = "com.plexapp.agents.thetvdb://81797?lang=en"
	one := []candidate{{"anidb-69", 100}}
	tag := idOutcome{kind: idTag, text: "[anidb-69]"}
	tagByName := idOutcome{kind: idTag, text: "[tvdb-81797]", aid: 16983, sure: "[anidb-16983] or [tmdb-900667]"}
	byGUID := idOutcome{kind: idGUID, text: guid}
	guidByName := idOutcome{kind: idGUID, text: guid, aid: 8857, sure: "[anidb-8857]"}
	tags := idOutcome{kind: idManyTags, text: "[anidb-69] {mal-21}"}
	off := idOutcome{kind: idModeOff, text: "[tvdb3-81797]", path: "tvdb", need: "TVDB_API_KEY or TVDB_LEGACY_API_KEY"}
	anidbOff := idOutcome{kind: idModeOff, text: "[anidb-69]", path: "anidb", need: "ANIDB_CLIENT"}
	found := searchOutcome{kind: searchFound, best: 100, min: 80}
	nothing := searchOutcome{kind: searchNothing, min: 80}
	below := searchOutcome{kind: searchBelowMin, best: 62, min: 80}
	return []struct {
		name string
		d    decision
		how  string
	}{
		{"tag", decision{cands: one, id: tag}, "tag [anidb-69]"},
		{"tag, by name", decision{cands: one, id: tagByName},
			"tag [tvdb-81797], by name: AniDB 16983, may be wrong; use [anidb-16983] or [tmdb-900667] to be sure"},
		{"guid", decision{cands: one, id: byGUID}, "guid " + guid},
		{"guid, by name", decision{cands: one, id: guidByName},
			"guid " + guid + ", by name: AniDB 8857, may be wrong; use [anidb-8857] to be sure"},
		{"two tags", decision{id: tags}, "id tags [anidb-69] {mal-21}: only one is allowed"},
		{"tag mode off", decision{id: off}, "tag [tvdb3-81797]: /tvdb is off, set TVDB_API_KEY or TVDB_LEGACY_API_KEY"},
		{"tag mode off, anidb", decision{id: anidbOff}, "tag [anidb-69]: /anidb is off, set ANIDB_CLIENT"},
		{"title search", decision{cands: one, search: found}, "title search"},
		{"title search, nothing", decision{search: nothing}, "title search: no similar title"},
		{"title search, below min", decision{search: below}, "title search: best score 62 below MIN_SCORE 80"},
		// Fix Match lists: the id's part, then the title search's
		{"list: tag", decision{cands: one, id: tag, search: found}, "tag [anidb-69]; title search"},
		{"list: tag, no title", decision{cands: one, id: tag, search: nothing}, "tag [anidb-69]; title search: no similar title"},
		{"list: tag, by name", decision{cands: one, id: tagByName, search: found},
			"tag [tvdb-81797], by name: AniDB 16983, may be wrong; use [anidb-16983] or [tmdb-900667] to be sure; title search"},
		{"list: guid", decision{cands: one, id: byGUID, search: found}, "guid " + guid + "; title search"},
		{"list: guid, by name, no title", decision{cands: one, id: guidByName, search: nothing},
			"guid " + guid + ", by name: AniDB 8857, may be wrong; use [anidb-8857] to be sure; title search: no similar title"},
		{"list: two tags", decision{cands: one, id: tags, search: found}, "id tags [anidb-69] {mal-21}: only one is allowed; title search"},
		{"list: two tags, no title", decision{id: tags, search: nothing},
			"id tags [anidb-69] {mal-21}: only one is allowed; title search: no similar title"},
		{"list: tag mode off", decision{cands: one, id: anidbOff, search: found}, "tag [anidb-69]: /anidb is off, set ANIDB_CLIENT; title search"},
		{"list: tag mode off, no title", decision{id: off, search: nothing},
			"tag [tvdb3-81797]: /tvdb is off, set TVDB_API_KEY or TVDB_LEGACY_API_KEY; title search: no similar title"},
		{"list: title search", decision{cands: one, search: found}, "title search"},
		{"list: title search, nothing", decision{search: nothing}, "title search: no similar title"},
	}
}

func TestDecisionHow(t *testing.T) {
	for _, c := range howForms() {
		if got := c.d.how(); got != c.how {
			t.Errorf("%s: how %q, want %q", c.name, got, c.how)
		}
	}
}

// The predicates say what reading the reason's words said before them: a
// tag rule's no-match starts "id tags " or "tag " and names no title
// search, a match by title search alone is "title search", and a pick by
// name is what follows ", by name: ".
func TestDecisionPredicates(t *testing.T) {
	stopped := map[string]bool{"two tags": true, "tag mode off": true, "tag mode off, anidb": true}
	bySearch := map[string]bool{"title search": true, "list: title search": true}
	notes := map[string]string{
		"tag, by name":                  "AniDB 16983, may be wrong; use [anidb-16983] or [tmdb-900667] to be sure",
		"guid, by name":                 "AniDB 8857, may be wrong; use [anidb-8857] to be sure",
		"list: tag, by name":            "AniDB 16983, may be wrong; use [anidb-16983] or [tmdb-900667] to be sure; title search",
		"list: guid, by name, no title": "AniDB 8857, may be wrong; use [anidb-8857] to be sure; title search: no similar title",
	}
	for _, c := range howForms() {
		if got := c.d.tagRuleStopped(); got != stopped[c.name] {
			t.Errorf("%s: tagRuleStopped %v, want %v", c.name, got, stopped[c.name])
		}
		fromWords := len(c.d.cands) == 0 && (strings.HasPrefix(c.how, "id tags ") || strings.HasPrefix(c.how, "tag ")) && !strings.Contains(c.how, "title search")
		if got := c.d.tagRuleStopped(); got != fromWords {
			t.Errorf("%s: tagRuleStopped %v, the words say %v", c.name, got, fromWords)
		}
		if got := c.d.byTitleSearch(); got != bySearch[c.name] || got != (c.how == "title search") {
			t.Errorf("%s: byTitleSearch %v, want %v", c.name, got, bySearch[c.name])
		}
		want, wantOK := notes[c.name]
		if got, ok := c.d.byNameNote(); got != want || ok != wantOK {
			t.Errorf("%s: byNameNote %q %v, want %q %v", c.name, got, ok, want, wantOK)
		}
		_, cut, cutOK := strings.Cut(c.how, ", by name: ")
		if got, ok := c.d.byNameNote(); got != cut || ok != cutOK {
			t.Errorf("%s: byNameNote %q %v, the words say %q %v", c.name, got, ok, cut, cutOK)
		}
	}
	if d := (decision{}); d.how() != "" || d.tagRuleStopped() || d.byTitleSearch() {
		t.Errorf("an empty decision: how %q, tag rule %v, title search %v; want none", d.how(), d.tagRuleStopped(), d.byTitleSearch())
	}
}

// decide fills in the whole decision: the title by the request's type, the
// candidates, both outcomes, the report's title results and the warning.
func TestDecide(t *testing.T) {
	l, titles := onePiece()
	both := matcher{lists: l, titles: titles, minScore: 80, anidbOn: true, tvdbOn: true}
	tvdbOnly := matcher{lists: l, titles: titles, minScore: 80, tvdbOn: true}
	const guid = "com.plexapp.agents.thetvdb://81797?lang=en"
	const sure = "[anidb-16983] or [tmdb-900667]"
	two := 2
	cases := []struct {
		name  string
		m     matcher
		mode  string
		r     MatchRequest
		title string
		cands []candidate
		id    idOutcome
		kind  searchKind
		warn  matchWarning
		how   string
	}{
		{"tag", both, "anidb", MatchRequest{Type: 2, Title: "One Piece [anidb-69]"}, "One Piece [anidb-69]",
			[]candidate{{"anidb-69", 100}}, idOutcome{kind: idTag, text: "[anidb-69]"}, searchNotRun, matchWarning{}, "tag [anidb-69]"},
		{"season: the parent's title", both, "tvdb", MatchRequest{Type: 3, Title: "Season 2", ParentTitle: "One Piece", Index: &two}, "One Piece",
			[]candidate{{"tvdb-81797", 100}}, idOutcome{}, searchFound, matchWarning{}, "title search"},
		{"episode: the grandparent's title", both, "anidb", MatchRequest{Type: 4, Title: "Episode", ParentTitle: "Season 1", GrandparentTitle: "One Piece Film Z"}, "One Piece Film Z",
			[]candidate{{"anidb-8857", 100}}, idOutcome{}, searchFound, matchWarning{}, "title search"},
		{"tag, by name", both, "tvdb", MatchRequest{Type: 2, Title: "One Piece Film Red [tvdb-81797]"}, "One Piece Film Red [tvdb-81797]",
			[]candidate{{"anidb-16983", 100}}, idOutcome{kind: idTag, text: "[tvdb-81797]", aid: 16983, sure: sure}, searchNotRun,
			matchWarning{kind: warnByName, title: "One Piece Film Red [tvdb-81797]", id: "[tvdb-81797]", aid: 16983, score: 100, sure: sure},
			"tag [tvdb-81797], by name: AniDB 16983, may be wrong; use " + sure + " to be sure"},
		{"guid, by name", both, "tvdb", MatchRequest{Type: 2, Title: "One Piece Film Red", GUID: guid}, "One Piece Film Red",
			[]candidate{{"anidb-16983", 100}}, idOutcome{kind: idGUID, text: guid, aid: 16983, sure: sure}, searchNotRun,
			matchWarning{kind: warnByName, title: "One Piece Film Red", id: guid, aid: 16983, score: 100, sure: sure},
			"guid " + guid + ", by name: AniDB 16983, may be wrong; use " + sure + " to be sure"},
		{"guid", both, "anidb", MatchRequest{Type: 2, Title: "One Piece", GUID: guid}, "One Piece",
			[]candidate{{"anidb-69", 100}}, idOutcome{kind: idGUID, text: guid}, searchNotRun, matchWarning{}, "guid " + guid},
		{"two tags", both, "anidb", MatchRequest{Type: 2, Title: "One Piece [anidb-69]", Filename: "/a/One Piece {mal-21}/01.mkv"}, "One Piece [anidb-69]",
			nil, idOutcome{kind: idManyTags, text: "[anidb-69] {mal-21}"}, searchNotRun,
			matchWarning{kind: warnTags, title: "One Piece [anidb-69]", id: "[anidb-69] {mal-21}"},
			"id tags [anidb-69] {mal-21}: only one is allowed"},
		{"two tags, Fix Match", both, "anidb", MatchRequest{Type: 2, Title: "One Piece Film Z [anidb-69] {mal-21}", Manual: 1}, "One Piece Film Z [anidb-69] {mal-21}",
			[]candidate{{"anidb-8857", 100}}, idOutcome{kind: idManyTags, text: "[anidb-69] {mal-21}"}, searchFound,
			matchWarning{kind: warnTags, title: "One Piece Film Z [anidb-69] {mal-21}", id: "[anidb-69] {mal-21}"},
			"id tags [anidb-69] {mal-21}: only one is allowed; title search"},
		{"tag mode off", tvdbOnly, "auto", MatchRequest{Type: 2, Title: "One Piece [anidb-69]"}, "One Piece [anidb-69]",
			nil, idOutcome{kind: idModeOff, text: "[anidb-69]", path: "anidb", need: "ANIDB_CLIENT"}, searchNotRun,
			matchWarning{kind: warnModeOff, id: "[anidb-69]", path: "anidb", need: "ANIDB_CLIENT"},
			"tag [anidb-69]: /anidb is off, set ANIDB_CLIENT"},
		{"tag mode off, manual episode", tvdbOnly, "auto", MatchRequest{Type: 4, GrandparentTitle: "One Piece [anidb-69]", Manual: 1}, "One Piece [anidb-69]",
			nil, idOutcome{kind: idModeOff, text: "[anidb-69]", path: "anidb", need: "ANIDB_CLIENT"}, searchNotRun,
			matchWarning{kind: warnModeOff, id: "[anidb-69]", path: "anidb", need: "ANIDB_CLIENT"},
			"tag [anidb-69]: /anidb is off, set ANIDB_CLIENT"},
		{"title search", both, "tvdb", MatchRequest{Type: 2, Title: "One Piece Film Red"}, "One Piece Film Red",
			[]candidate{{"anidb-16983", 100}}, idOutcome{}, searchFound, matchWarning{}, "title search"},
		{"tag not in the mapping: title search", both, "anidb", MatchRequest{Type: 2, Title: "One Piece [mal-7]"}, "One Piece [mal-7]",
			[]candidate{{"anidb-69", 100}}, idOutcome{}, searchFound, matchWarning{}, "title search"},
	}
	for _, c := range cases {
		d := c.m.decide(Modes[c.mode], c.r, false)
		got := d.cands
		if c.r.fixMatchList() { // more title results follow the first
			got = got[:min(len(got), len(c.cands))]
		}
		if d.title != c.title || !reflect.DeepEqual(got, c.cands) || d.id != c.id || d.search.kind != c.kind || d.warn != c.warn || d.found != nil {
			t.Errorf("%s: got %+v", c.name, d)
		}
		if d.search.kind != searchNotRun && d.search.min != 80 {
			t.Errorf("%s: search %+v, want MIN_SCORE 80", c.name, d.search)
		}
		if got := d.how(); got != c.how {
			t.Errorf("%s: how %q, want %q", c.name, got, c.how)
		}
	}

	// note keeps the title search's first results, also those a match turns down.
	d := both.decide(Modes["anidb"], MatchRequest{Type: 2, Title: "One Piece Film Red"}, true)
	if len(d.found) == 0 || d.found[0] != (foundTitle{aid: 16983, score: 100, title: "One Piece Film Red"}) || len(d.found) > 5 {
		t.Errorf("found %+v, want Film Red first and at most five", d.found)
	}
	if d := both.decide(Modes["anidb"], MatchRequest{Type: 2, Title: "One Piece [anidb-69]"}, true); d.found != nil {
		t.Errorf("found %+v with no title search, want none", d.found)
	}
}

// The provider logs a decision's warning, with the words and fields it
// always had, once per key every 10 minutes.
func TestDecideLogsWarning(t *testing.T) {
	l, titles := onePiece()
	f := fetch.New(t.TempDir())
	newP := func() *Provider {
		p := &Provider{lists: l, titles: titles, tvdb: sources.NewTVDB(f, "key", "", ""),
			tagsLogged: everyTenMinutes{max: 1000}, nameLogged: everyTenMinutes{max: 1000}}
		p.cfg.MinScore = 80
		return p
	}
	for _, c := range []struct {
		mode string
		r    MatchRequest
		line string // the end of the warning's line
	}{
		{"tvdb", MatchRequest{Type: 2, Title: "One Piece [anidb-69] {mal-21}"},
			`level=WARN msg="match: more than one id tag; only one is allowed, the show gets no match" title="One Piece [anidb-69] {mal-21}" tags="[anidb-69] {mal-21}"` + "\n"},
		{"auto", MatchRequest{Type: 2, Title: "One Piece [anidb-69]"},
			`level=WARN msg="match: id tag needs /anidb, which is off" tag=[anidb-69] set=ANIDB_CLIENT` + "\n"},
		{"tvdb", MatchRequest{Type: 2, Title: "One Piece Film Red [tvdb-81797]"},
			`level=WARN msg="match: matched to an AniDB entry by name inside a TheTVDB id; may be wrong" title="One Piece Film Red [tvdb-81797]" id=[tvdb-81797] aid=16983 score=100 to_be_sure="use [anidb-16983] or [tmdb-900667]"` + "\n"},
	} {
		p := newP()
		var logs strings.Builder
		restore := captureLogs(&logs)
		var first decision
		for i := range 3 { // the same request again: the same decision, no new warning
			d := p.decide(Modes[c.mode], c.r, false)
			if i == 0 {
				first = d
			} else if !reflect.DeepEqual(d, first) {
				t.Errorf("%q: decision %+v, then %+v", c.r.Title, first, d)
			}
		}
		restore()
		if first.warn.kind == warnNone || !reflect.DeepEqual(first, p.matcher().decide(Modes[c.mode], c.r, false)) {
			t.Errorf("%q: decision %+v, want the matcher's, with a warning", c.r.Title, first)
		}
		if got := logs.String(); strings.Count(got, "level=WARN") != 1 || !strings.HasSuffix(got, c.line) {
			t.Errorf("%q: logged\n%swant one warning ending\n%s", c.r.Title, got, c.line)
		}
	}
}

// fakePeek is cached AniDB details by aid.
type fakePeek map[int]*sources.AniDBAnime

func (f fakePeek) Peek(aid int) (*sources.AniDBAnime, bool) { return f[aid], true }

// Two special-only entries the name can't tell apart: the cached AniDB
// start date decides before a year in the title does.
func TestEntryYearFromCache(t *testing.T) {
	l := &mapping.Lists{}
	l.SetForTest(map[int]mapping.Entry{
		1: {AID: 1, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}},
		3: {AID: 3, TVDB: mapping.Placement{ID: 500, Season: 0, Offset: 1, Mapped: true}},
		4: {AID: 4, TVDB: mapping.Placement{ID: 500, Season: 0, Offset: 2, Mapped: true}},
	})
	titles := &anidb.Titles{}
	titles.SetForTest(map[int][]anidb.Title{
		1: {{Type: "main", Lang: "x-jat", Text: "Kizuna Movies"}},
		3: {{Type: "main", Lang: "x-jat", Text: "Kizuna Gekijouban"}, {Type: "synonym", Lang: "x-jat", Text: "Kizuna Gekijouban (2020)"}},
		4: {{Type: "main", Lang: "x-jat", Text: "Kizuna Gekijouban"}},
	})
	r := MatchRequest{Type: 2, Title: "Kizuna Gekijouban [tvdb-500]", Year: 2020}
	m := matcher{lists: l, titles: titles, minScore: 80, tvdbOn: true}
	if d := m.decide(Modes["tvdb"], r, false); len(d.cands) != 1 || d.cands[0].root != "anidb-3" {
		t.Errorf("nothing cached: got %v (%s), want anidb-3 by its title's year", d.cands, d.how())
	}
	m.peek = fakePeek{3: {StartDate: "2010-04-01"}, 4: {StartDate: "2020-07-11"}}
	if d := m.decide(Modes["tvdb"], r, false); len(d.cands) != 1 || d.cands[0].root != "anidb-4" {
		t.Errorf("start dates cached: got %v (%s), want anidb-4", d.cands, d.how())
	}
}
