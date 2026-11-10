package provider

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"kizuna/internal/anidb"
	"kizuna/internal/fetch"
	"kizuna/internal/mapping"
	"kizuna/internal/sources"
)

// A show or season match with includeChildren=1 carries its seasons or
// episodes inline, the same items .../children lists; without it, none.
func TestMatchIncludeChildren(t *testing.T) {
	p := newTestProvider(t, &fakeNet{answer: testSeries})
	serve := func(method, path, body string) Container {
		t.Helper()
		rec := plexRequest(context.Background(), p, method, path, body)
		var c Container
		if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil || rec.Code != 200 {
			t.Fatalf("%s %s: HTTP %d %s (%v)", method, path, rec.Code, rec.Body, err)
		}
		return c
	}
	const matches = "/tvdb/library/metadata/matches"
	for _, c := range []struct{ body, children string }{
		{`{"type":2,"title":"Kizuna Test","includeChildren":1}`, "/tvdb/library/metadata/tvdb-500/children"},
		{`{"type":3,"parentTitle":"Kizuna Test","index":1,"includeChildren":"1"}`, "/tvdb/library/metadata/tvdb-500-s1/children"},
	} {
		got := serve("POST", matches, c.body).MediaContainer.Metadata
		if len(got) != 1 || got[0].Children == nil {
			t.Fatalf("%s: got %+v, want one item with children", c.body, got)
		}
		want := serve("GET", c.children, "").MediaContainer.Metadata
		if len(want) == 0 || got[0].Children.Size != len(want) || !reflect.DeepEqual(got[0].Children.Metadata, want) {
			t.Errorf("%s: children %+v, want %+v", c.body, got[0].Children, want)
		}
	}
	for _, body := range []string{`{"type":2,"title":"Kizuna Test"}`, `{"type":3,"parentTitle":"Kizuna Test","index":1,"includeChildren":0}`} {
		if got := serve("POST", matches, body).MediaContainer.Metadata; len(got) != 1 || got[0].Children != nil {
			t.Errorf("%s: got %+v, want one item without children", body, got)
		}
	}
}

// A manual match (Plex's Fix Match) lists the show the GUID or an id tag
// names first, then the typed title's search results, each show once and
// at most 10. An automatic match keeps the id match alone. A strict tag
// whose mode is off still gives no id match, but the title results are
// listed.
func TestManualMatchListsTitleResults(t *testing.T) {
	entries := map[int]mapping.Entry{69: {AID: 69, TVDB: mapping.Placement{ID: 81797, Season: -1, Mapped: true}}}
	names := map[int][]anidb.Title{
		69: {{Type: "main", Lang: "x-jat", Text: "One Piece"}},
		70: {{Type: "main", Lang: "x-jat", Text: "Correct Title"}},
	}
	for aid := 71; aid < 83; aid++ {
		names[aid] = []anidb.Title{{Type: "main", Lang: "x-jat", Text: "Common Name " + string(rune('A'+aid-71))}}
	}
	l := &mapping.Lists{}
	l.SetForTest(entries)
	titles := &anidb.Titles{}
	titles.SetForTest(names)
	dir := t.TempDir()
	f := fetch.New(dir)
	p := &Provider{lists: l, titles: titles}
	tvdbOn := &Provider{lists: l, titles: titles, tvdb: sources.NewTVDB(f, "key", "", "")}
	guid := "tv.plex.agents.custom.kizuna.anidb://show/anidb-69"
	roots := func(cs []candidate) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.root)
		}
		return out
	}
	cases := []struct {
		name  string
		p     *Provider
		mode  Mode
		r     MatchRequest
		want  []string
		first int    // how many leading roots to compare (0: all)
		size  int    // the list's length when first is set
		how   string // the start of how
	}{
		{"guid, manual", p, Modes["anidb"], MatchRequest{Title: "Correct Title", GUID: guid, Manual: 1},
			[]string{"anidb-69", "anidb-70"}, 0, 0, "guid " + guid + "; title search"},
		{"guid, automatic", p, Modes["anidb"], MatchRequest{Title: "Correct Title", GUID: guid},
			[]string{"anidb-69"}, 0, 0, "guid " + guid},
		{"guid and title name one show", p, Modes["anidb"], MatchRequest{Title: "One Piece", GUID: guid, Manual: 1},
			[]string{"anidb-69"}, 0, 0, "guid"},
		{"tag, manual", p, Modes["anidb"], MatchRequest{Title: "Correct Title", Filename: "/a/One Piece [anidb-69]/01.mkv", Manual: 1},
			[]string{"anidb-69", "anidb-70"}, 0, 0, "tag [anidb-69]; title search"},
		{"at most 10", p, Modes["anidb"], MatchRequest{Title: "Common Name", GUID: guid, Manual: 1},
			[]string{"anidb-69"}, 1, 10, "guid"},
		{"tag mode off, manual", tvdbOn, Modes["auto"], MatchRequest{Title: "Correct Title [anidb-69]", Manual: 1},
			[]string{"anidb-70"}, 0, 0, "tag [anidb-69]: /anidb is off, set ANIDB_CLIENT; title search"},
		{"tag mode off, automatic", tvdbOn, Modes["auto"], MatchRequest{Title: "Correct Title [anidb-69]"},
			nil, 0, 0, "tag [anidb-69]: /anidb is off"},
		{"two tags, manual", p, Modes["anidb"], MatchRequest{Title: "Correct Title [anidb-69] [anidb-5]", Manual: 1},
			[]string{"anidb-70"}, 0, 0, "id tags [anidb-69] [anidb-5]: only one is allowed; title search"},
		{"two tags, manual season", p, Modes["anidb"], MatchRequest{Type: 3, Title: "Correct Title [anidb-69] [anidb-5]", Manual: 1},
			nil, 0, 0, "id tags [anidb-69] [anidb-5]: only one is allowed"},
		{"tag mode off, manual episode", tvdbOn, Modes["auto"], MatchRequest{Type: 4, Title: "Correct Title [anidb-69]", Manual: 1},
			nil, 0, 0, "tag [anidb-69]: /anidb is off"},
		// a manual season or episode match whose tag or GUID matches skips the title search
		{"tag, manual season", p, Modes["anidb"], MatchRequest{Type: 3, Title: "Correct Title", Filename: "/a/One Piece [anidb-69]/01.mkv", Manual: 1},
			[]string{"anidb-69"}, 0, 0, "tag [anidb-69]"},
		{"guid, manual episode", p, Modes["anidb"], MatchRequest{Type: 4, Title: "Correct Title", GUID: guid, Manual: 1},
			[]string{"anidb-69"}, 0, 0, "guid " + guid},
	}
	for _, c := range cases {
		got, how := c.p.searchCandidates(c.mode, c.r.Title, c.r, nil)
		rs := roots(got)
		if c.first > 0 {
			if len(rs) != c.size || !reflect.DeepEqual(rs[:c.first], c.want) {
				t.Errorf("%s: got %v (%s), want %d starting %v", c.name, rs, how, c.size, c.want)
			}
		} else if !reflect.DeepEqual(rs, c.want) {
			t.Errorf("%s: got %v (%s), want %v", c.name, rs, how, c.want)
		}
		if !strings.HasPrefix(how, c.how) {
			t.Errorf("%s: how %q, want it to start %q", c.name, how, c.how)
		}
	}
}
