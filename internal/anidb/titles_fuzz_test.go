package anidb

import (
	"strings"
	"testing"
	"unicode"
)

// FuzzNormalize: normalizing is stable (a normalized title normalizes to
// itself) and leaves only lowercase letters, digits and single spaces.
func FuzzNormalize(f *testing.F) {
	for _, s := range []string{"Shingeki no Kyojin (2013)", "Hunter × Hunter [TV]", "Pokémon: Ōkido's “Lab”", "Straße", "  a  b  ", "İstanbul"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n := normalize(s)
		if again := normalize(n); again != n {
			t.Errorf("normalize(%q) = %q, but normalize(%q) = %q", s, n, n, again)
		}
		if n != strings.TrimSpace(n) || strings.Contains(n, "  ") {
			t.Errorf("normalize(%q) = %q: stray spaces", s, n)
		}
		for _, r := range n {
			if r != ' ' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				t.Errorf("normalize(%q) = %q: keeps %q", s, n, r)
			}
		}
	})
}

// FuzzCleanSummary: AniDB descriptions never panic and come back trimmed.
func FuzzCleanSummary(f *testing.F) {
	f.Add("Pirates. http://anidb.net/ch123 [Luffy] sails.\n* note\n\n\n\nSource: somewhere")
	f.Add("http://anidb.net/ [] [x]\n-- a\n~ b")
	f.Fuzz(func(t *testing.T, s string) {
		if c := CleanSummary(s); c != strings.TrimSpace(c) {
			t.Errorf("CleanSummary(%q) = %q: not trimmed", s, c)
		}
	})
}

// FuzzSearch: search over fuzzed titles and folder names never panics,
// respects the limit, sorts best first and only returns known entries with
// scores 1..100. A query equal to a title after normalizing finds it with
// at least 98.
func FuzzSearch(f *testing.F) {
	f.Add("Hunter x Hunter", "Hunter x Hunter (2011)", "Hunter x Hunter", 0, 5)
	f.Add("Shingeki no Kyojin", "Attack on Titan", "attack on titan!", 2013, 1)
	f.Add("Pokémon", "a", "Pokemon (1997)", 0, 0)
	f.Fuzz(func(t *testing.T, a, b, query string, year, limit int) {
		limit = max(limit, 0) % 10
		var x Titles
		x.SetForTest(map[int][]Title{1: {{Type: "main", Text: a}}, 2: {{Type: "official", Text: b}}, 3: {{Type: "syn", Text: b + " " + a}}})
		got := x.Search(query, year, limit)
		if len(got) > limit {
			t.Fatalf("%d results for limit %d", len(got), limit)
		}
		for i, m := range got {
			if m.AID < 1 || m.AID > 3 || m.Score < 1 || m.Score > 100 {
				t.Errorf("bad result %+v", m)
			}
			if i > 0 && got[i-1].Score < m.Score {
				t.Errorf("not best first: %+v", got)
			}
		}
		q := normalize(query)
		if q == "" || limit < 3 {
			return
		}
		for aid, title := range map[int]string{1: a, 2: b, 3: b + " " + a} {
			if normalize(title) != q {
				continue
			}
			found := false
			for _, m := range got {
				found = found || m.AID == aid && m.Score >= 98
			}
			if !found {
				t.Errorf("Search(%q) = %+v: entry %d (%q) normalizes to the query but scores under 98", query, got, aid, title)
			}
		}
	})
}
