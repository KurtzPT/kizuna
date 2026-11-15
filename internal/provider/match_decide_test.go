package provider

import (
	"reflect"
	"strings"
	"testing"

	"kizuna/internal/anidb"
	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/report"
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
	none := report.MatchBy{}
	tag := report.MatchBy{ID: report.MatchTag, Text: "[anidb-69]"}
	tvdbTag := report.MatchBy{ID: report.MatchTag, Text: "[tvdb-81797]"}
	byGUID := report.MatchBy{ID: report.MatchGUID, Text: guid}
	filmRed := report.ByName{AID: 16983, Sure: "[anidb-16983] or [tmdb-900667]"}
	filmZ := report.ByName{AID: 8857, Sure: "[anidb-8857]"}
	tags := report.MatchBy{ID: report.MatchManyTags, Text: "[anidb-69] {mal-21}"}
	off := report.MatchBy{ID: report.MatchTagOff, Text: "[tvdb3-81797]", Off: "tvdb", Need: "TVDB_API_KEY or TVDB_LEGACY_API_KEY"}
	anidbOff := report.MatchBy{ID: report.MatchTagOff, Text: "[anidb-69]", Off: "anidb", Need: "ANIDB_CLIENT"}
	// by, with what the title search said after it
	found := func(by report.MatchBy) report.MatchBy { by.Search, by.Best = report.SearchFound, 100; return by }
	nothing := func(by report.MatchBy) report.MatchBy { by.Search = report.SearchNone; return by }
	return []struct {
		name string
		d    decision
		how  string
	}{
		{"tag", decision{cands: one, by: tag}, "tag [anidb-69]"},
		{"tag, by name", decision{cands: one, by: tvdbTag, named: filmRed},
			"tag [tvdb-81797], by name: AniDB 16983, may be wrong; use [anidb-16983] or [tmdb-900667] to be sure"},
		{"guid", decision{cands: one, by: byGUID}, "guid " + guid},
		{"guid, by name", decision{cands: one, by: byGUID, named: filmZ},
			"guid " + guid + ", by name: AniDB 8857, may be wrong; use [anidb-8857] to be sure"},
		{"two tags", decision{by: tags}, "id tags [anidb-69] {mal-21}: only one is allowed"},
		{"tag mode off", decision{by: off}, "tag [tvdb3-81797]: /tvdb is off, set TVDB_API_KEY or TVDB_LEGACY_API_KEY"},
		{"tag mode off, anidb", decision{by: anidbOff}, "tag [anidb-69]: /anidb is off, set ANIDB_CLIENT"},
		{"title search", decision{cands: one, by: found(none), min: 80}, "title search"},
		{"title search, nothing", decision{by: nothing(none), min: 80}, "title search: no similar title"},
		{"title search, below min", decision{by: report.MatchBy{Search: report.SearchLow, Best: 62}, min: 80},
			"title search: best score 62 below MIN_SCORE 80"},
		// Fix Match lists: the id's part, then the title search's
		{"list: tag", decision{cands: one, by: found(tag)}, "tag [anidb-69]; title search"},
		{"list: tag, no title", decision{cands: one, by: nothing(tag)}, "tag [anidb-69]; title search: no similar title"},
		{"list: tag, by name", decision{cands: one, by: found(tvdbTag), named: filmRed},
			"tag [tvdb-81797], by name: AniDB 16983, may be wrong; use [anidb-16983] or [tmdb-900667] to be sure; title search"},
		{"list: guid", decision{cands: one, by: found(byGUID)}, "guid " + guid + "; title search"},
		{"list: guid, by name, no title", decision{cands: one, by: nothing(byGUID), named: filmZ},
			"guid " + guid + ", by name: AniDB 8857, may be wrong; use [anidb-8857] to be sure; title search: no similar title"},
		{"list: two tags", decision{cands: one, by: found(tags)}, "id tags [anidb-69] {mal-21}: only one is allowed; title search"},
		{"list: two tags, no title", decision{by: nothing(tags)},
			"id tags [anidb-69] {mal-21}: only one is allowed; title search: no similar title"},
		{"list: tag mode off", decision{cands: one, by: found(anidbOff)}, "tag [anidb-69]: /anidb is off, set ANIDB_CLIENT; title search"},
		{"list: tag mode off, no title", decision{by: nothing(off)},
			"tag [tvdb3-81797]: /tvdb is off, set TVDB_API_KEY or TVDB_LEGACY_API_KEY; title search: no similar title"},
		{"list: title search", decision{cands: one, by: found(none)}, "title search"},
		{"list: title search, nothing", decision{by: nothing(none)}, "title search: no similar title"},
	}
}

func TestDecisionHow(t *testing.T) {
	for _, c := range howForms() {
		if got := c.d.how(); got != c.how {
			t.Errorf("%s: how %q, want %q", c.name, got, c.how)
		}
	}
}

// The report page reads a decision's facts (report.MatchBy), the log its
// words: both must say the same. A tag rule's no-match starts "id tags " or
// "tag " and names no title search, a match by title search alone is "title
// search", and a pick by name is what follows ", by name: " (in a Fix Match
// list, up to the title search's part).
func TestDecisionFactsMatchWords(t *testing.T) {
	stopped := map[string]bool{"two tags": true, "tag mode off": true, "tag mode off, anidb": true}
	bySearch := map[string]bool{"title search": true, "list: title search": true}
	for _, c := range howForms() {
		by := c.d.by
		if got := by.TagRule(); got != stopped[c.name] {
			t.Errorf("%s: TagRule %v, want %v", c.name, got, stopped[c.name])
		}
		fromWords := len(c.d.cands) == 0 && (strings.HasPrefix(c.how, "id tags ") || strings.HasPrefix(c.how, "tag ")) && !strings.Contains(c.how, "title search")
		if got := by.TagRule(); got != fromWords {
			t.Errorf("%s: TagRule %v, the words say %v", c.name, got, fromWords)
		}
		if got := by.TitleSearch(); got != bySearch[c.name] || got != (c.how == "title search") {
			t.Errorf("%s: TitleSearch %v, want %v", c.name, got, bySearch[c.name])
		}
		_, cut, cutOK := strings.Cut(c.how, ", by name: ")
		if got := c.d.named.AID > 0; got != cutOK {
			t.Errorf("%s: by name %v, the words say %v", c.name, got, cutOK)
			continue
		}
		if !cutOK {
			continue
		}
		// The words after "by name: " are the note, and in a Fix Match list
		// (which is in no report) the title search's part after it.
		want := c.d.named.Note()
		if by.Search != "" {
			want += "; " + c.d.searchHow()
		}
		if cut != want {
			t.Errorf("%s: the words say %q, want %q", c.name, cut, want)
		}
	}
	if d := (decision{}); d.how() != "" || d.by.TagRule() || d.by.TitleSearch() {
		t.Errorf("an empty decision: how %q, facts %+v; want none", d.how(), d.by)
	}
}

// decide fills in the whole decision: the title by the request's type, the
// candidates, the facts, the report's title results and the warning.
func TestDecide(t *testing.T) {
	l, titles := onePiece()
	both := matcher{lists: l, titles: titles, minScore: 80, anidbOn: true, tvdbOn: true}
	tvdbOnly := matcher{lists: l, titles: titles, minScore: 80, tvdbOn: true}
	const guid = "com.plexapp.agents.thetvdb://81797?lang=en"
	const sure = "[anidb-16983] or [tmdb-900667]"
	two := 2
	found := report.MatchBy{Search: report.SearchFound}
	cases := []struct {
		name  string
		m     matcher
		mode  string
		r     MatchRequest
		title string
		cands []candidate
		by    report.MatchBy // without Best
		named report.ByName
		warn  matchWarning
		how   string
	}{
		{"tag", both, "anidb", MatchRequest{Type: 2, Title: "One Piece [anidb-69]"}, "One Piece [anidb-69]",
			[]candidate{{"anidb-69", 100}}, report.MatchBy{ID: report.MatchTag, Text: "[anidb-69]"}, report.ByName{}, matchWarning{}, "tag [anidb-69]"},
		{"season: the parent's title", both, "tvdb", MatchRequest{Type: 3, Title: "Season 2", ParentTitle: "One Piece", Index: &two}, "One Piece",
			[]candidate{{"tvdb-81797", 100}}, found, report.ByName{}, matchWarning{}, "title search"},
		{"episode: the grandparent's title", both, "anidb", MatchRequest{Type: 4, Title: "Episode", ParentTitle: "Season 1", GrandparentTitle: "One Piece Film Z"}, "One Piece Film Z",
			[]candidate{{"anidb-8857", 100}}, found, report.ByName{}, matchWarning{}, "title search"},
		{"tag, by name", both, "tvdb", MatchRequest{Type: 2, Title: "One Piece Film Red [tvdb-81797]"}, "One Piece Film Red [tvdb-81797]",
			[]candidate{{"anidb-16983", 100}}, report.MatchBy{ID: report.MatchTag, Text: "[tvdb-81797]"}, report.ByName{AID: 16983, Sure: sure},
			matchWarning{kind: warnByName, title: "One Piece Film Red [tvdb-81797]", id: "[tvdb-81797]", aid: 16983, score: 100, sure: sure},
			"tag [tvdb-81797], by name: AniDB 16983, may be wrong; use " + sure + " to be sure"},
		{"guid, by name", both, "tvdb", MatchRequest{Type: 2, Title: "One Piece Film Red", GUID: guid}, "One Piece Film Red",
			[]candidate{{"anidb-16983", 100}}, report.MatchBy{ID: report.MatchGUID, Text: guid}, report.ByName{AID: 16983, Sure: sure},
			matchWarning{kind: warnByName, title: "One Piece Film Red", id: guid, aid: 16983, score: 100, sure: sure},
			"guid " + guid + ", by name: AniDB 16983, may be wrong; use " + sure + " to be sure"},
		{"guid", both, "anidb", MatchRequest{Type: 2, Title: "One Piece", GUID: guid}, "One Piece",
			[]candidate{{"anidb-69", 100}}, report.MatchBy{ID: report.MatchGUID, Text: guid}, report.ByName{}, matchWarning{}, "guid " + guid},
		{"two tags", both, "anidb", MatchRequest{Type: 2, Title: "One Piece [anidb-69]", Filename: "/a/One Piece {mal-21}/01.mkv"}, "One Piece [anidb-69]",
			nil, report.MatchBy{ID: report.MatchManyTags, Text: "[anidb-69] {mal-21}"}, report.ByName{},
			matchWarning{kind: warnTags, title: "One Piece [anidb-69]", id: "[anidb-69] {mal-21}"},
			"id tags [anidb-69] {mal-21}: only one is allowed"},
		{"two tags, Fix Match", both, "anidb", MatchRequest{Type: 2, Title: "One Piece Film Z [anidb-69] {mal-21}", Manual: 1}, "One Piece Film Z [anidb-69] {mal-21}",
			[]candidate{{"anidb-8857", 100}}, report.MatchBy{ID: report.MatchManyTags, Text: "[anidb-69] {mal-21}", Search: report.SearchFound}, report.ByName{},
			matchWarning{kind: warnTags, title: "One Piece Film Z [anidb-69] {mal-21}", id: "[anidb-69] {mal-21}"},
			"id tags [anidb-69] {mal-21}: only one is allowed; title search"},
		{"tag mode off", tvdbOnly, "auto", MatchRequest{Type: 2, Title: "One Piece [anidb-69]"}, "One Piece [anidb-69]",
			nil, report.MatchBy{ID: report.MatchTagOff, Text: "[anidb-69]", Off: "anidb", Need: "ANIDB_CLIENT"}, report.ByName{},
			matchWarning{kind: warnModeOff, id: "[anidb-69]", path: "anidb", need: "ANIDB_CLIENT"},
			"tag [anidb-69]: /anidb is off, set ANIDB_CLIENT"},
		{"tag mode off, manual episode", tvdbOnly, "auto", MatchRequest{Type: 4, GrandparentTitle: "One Piece [anidb-69]", Manual: 1}, "One Piece [anidb-69]",
			nil, report.MatchBy{ID: report.MatchTagOff, Text: "[anidb-69]", Off: "anidb", Need: "ANIDB_CLIENT"}, report.ByName{},
			matchWarning{kind: warnModeOff, id: "[anidb-69]", path: "anidb", need: "ANIDB_CLIENT"},
			"tag [anidb-69]: /anidb is off, set ANIDB_CLIENT"},
		{"title search", both, "tvdb", MatchRequest{Type: 2, Title: "One Piece Film Red"}, "One Piece Film Red",
			[]candidate{{"anidb-16983", 100}}, found, report.ByName{}, matchWarning{}, "title search"},
		{"tag not in the mapping: title search", both, "anidb", MatchRequest{Type: 2, Title: "One Piece [mal-7]"}, "One Piece [mal-7]",
			[]candidate{{"anidb-69", 100}}, found, report.ByName{}, matchWarning{}, "title search"},
	}
	for _, c := range cases {
		d := c.m.decide(Modes[c.mode], c.r, false, nil)
		got := d.cands
		if c.r.fixMatchList() { // more title results follow the first
			got = got[:min(len(got), len(c.cands))]
		}
		by := d.by
		by.Best = 0
		if d.title != c.title || !reflect.DeepEqual(got, c.cands) || by != c.by || d.named != c.named || d.warn != c.warn || d.found != nil {
			t.Errorf("%s: got %+v", c.name, d)
		}
		if d.by.Search != "" && (d.min != 80 || d.by.Best <= 0) {
			t.Errorf("%s: MIN_SCORE %d, best %d; want 80 and the best result's score", c.name, d.min, d.by.Best)
		}
		if got := d.how(); got != c.how {
			t.Errorf("%s: how %q, want %q", c.name, got, c.how)
		}
	}

	// note keeps the title search's first results, also those a match turns down.
	d := both.decide(Modes["anidb"], MatchRequest{Type: 2, Title: "One Piece Film Red"}, true, nil)
	if len(d.found) == 0 || d.found[0] != (report.Candidate{AID: 16983, Title: "One Piece Film Red", Score: 100}) || len(d.found) > 5 {
		t.Errorf("found %+v, want Film Red first and at most five", d.found)
	}
	if d := both.decide(Modes["anidb"], MatchRequest{Type: 2, Title: "One Piece [anidb-69]"}, true, nil); d.found != nil {
		t.Errorf("found %+v with no title search, want none", d.found)
	}
}

// MIN_SCORE holds for every match but Plex's Fix Match list of a show: a
// manual season or episode match is one answer, like an automatic one.
func TestManualMatchMinScore(t *testing.T) {
	l, titles := onePiece()
	const query = "One Piece Film"
	results := titles.Search(query, 0, 20)
	if len(results) == 0 || results[0].Score <= 0 || results[0].Score >= 100 {
		t.Fatalf("title search for %q: %+v, want a best score between 0 and 100", query, results)
	}
	best := results[0].Score
	above := matcher{lists: l, titles: titles, minScore: best + 1, anidbOn: true} // every result is below it
	at := matcher{lists: l, titles: titles, minScore: best, anidbOn: true}
	cases := []struct {
		name   string
		m      matcher
		r      MatchRequest
		cands  int // how many candidates
		search report.SearchResult
	}{
		{"show, below", above, MatchRequest{Type: 2, Title: query}, 0, report.SearchLow},
		{"Fix Match list, below", above, MatchRequest{Type: 2, Title: query, Manual: 1}, min(len(results), 10), report.SearchFound},
		{"manual season, below", above, MatchRequest{Type: 3, ParentTitle: query, Manual: 1}, 0, report.SearchLow},
		{"manual episode, below", above, MatchRequest{Type: 4, GrandparentTitle: query, Manual: 1}, 0, report.SearchLow},
		{"manual season, at", at, MatchRequest{Type: 3, ParentTitle: query, Manual: 1}, 1, report.SearchFound},
		{"manual episode, at", at, MatchRequest{Type: 4, GrandparentTitle: query, Manual: 1}, 1, report.SearchFound},
	}
	for _, c := range cases {
		d := c.m.decide(Modes["anidb"], c.r, false, nil)
		if len(d.cands) != c.cands || d.by.Search != c.search {
			t.Errorf("%s: %d candidates, search %q; want %d, %q", c.name, len(d.cands), d.by.Search, c.cands, c.search)
		}
		if len(d.cands) > 0 && d.cands[0].score != best {
			t.Errorf("%s: first candidate %+v, want score %d", c.name, d.cands[0], best)
		}
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
		if first.warn.kind == warnNone || !reflect.DeepEqual(first, p.matcher().decide(Modes[c.mode], c.r, false, nil)) {
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
	if d := m.decide(Modes["tvdb"], r, false, nil); len(d.cands) != 1 || d.cands[0].root != "anidb-3" {
		t.Errorf("nothing cached: got %v (%s), want anidb-3 by its title's year", d.cands, d.how())
	}
	m.peek = fakePeek{3: {StartDate: "2010-04-01"}, 4: {StartDate: "2020-07-11"}}
	if d := m.decide(Modes["tvdb"], r, false, nil); len(d.cands) != 1 || d.cands[0].root != "anidb-4" {
		t.Errorf("start dates cached: got %v (%s), want anidb-4", d.cands, d.how())
	}
}
