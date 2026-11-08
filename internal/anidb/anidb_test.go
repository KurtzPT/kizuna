package anidb

import "testing"

func TestPickTitle(t *testing.T) {
	titles := []Title{{"main", "x-jat", "One Piece"}, {"official", "en", "One Piece EN"}, {"official", "ja", "ワンピース"}, {"short", "en", "OP"}}
	cases := map[string][]string{"One Piece": {"main", "en"}, "One Piece EN": {"en", "main"}, "ワンピース": {"ja"}}
	for want, langs := range cases {
		if got := PickTitle(titles, langs, false); got != want {
			t.Errorf("%v: got %q want %q", langs, got, want)
		}
	}
}

func TestSearch(t *testing.T) {
	ts := &Titles{byAID: map[int][]Title{1: {{"main", "x-jat", "Shingeki no Kyojin"}}, 2: {{"main", "x-jat", "Shingeki no Kyojin Season 2"}}}}
	ts.set(ts.byAID)
	got := ts.Search("Shingeki no Kyojin (2013)", 0, 5)
	if len(got) == 0 || got[0].AID != 1 || got[0].Score < 98 {
		t.Fatalf("search: %+v", got)
	}
}

func TestOfficialOnlyAndFind(t *testing.T) {
	titles := []Title{{"main", "x-jat", "Romaji"}, {"syn", "en", "Fan Title"}, {"official", "de", "Deutsch"}, {"official", "ja", "日本語"}}
	if got := PickTitle(titles, []string{"en", "de"}, false); got != "Fan Title" {
		t.Errorf("synonyms allowed: got %q", got)
	}
	if got := PickTitle(titles, []string{"en", "de"}, true); got != "Deutsch" {
		t.Errorf("official only: got %q, want next language", got)
	}
	if got := PickTitle(titles, []string{"en"}, true); got != "Romaji" {
		t.Errorf("official only, nothing listed: got %q, want main", got)
	}
	if got := FindTitle(titles, []string{"ko"}, true); got != "" {
		t.Errorf("find: got %q, want empty", got)
	}
	if got := FindTitle(titles, []string{"x-jat", "ja"}, true); got != "Romaji" {
		t.Errorf("original x-jat: got %q", got)
	}
}

func TestSearchYearPicksRemake(t *testing.T) {
	ts := &Titles{byAID: map[int][]Title{
		1:    {{"main", "x-jat", "Hunter x Hunter"}},
		8074: {{"main", "x-jat", "Hunter x Hunter (2011)"}, {"official", "en", "Hunter x Hunter"}},
	}}
	ts.set(ts.byAID)
	cases := []struct {
		query string
		year  int
		want  int
	}{
		{"Hunter x Hunter", 0, 1},           // no year: the original (lowest id on a tie)
		{"Hunter x Hunter", 1999, 1},        // a year no title carries changes nothing
		{"Hunter x Hunter", 2011, 8074},     // Plex's year picks the remake
		{"Hunter x Hunter (2011)", 0, 8074}, // so does a year in the title
	}
	for _, c := range cases {
		got := ts.Search(c.query, c.year, 5)
		if len(got) == 0 || got[0].AID != c.want || got[0].Score != 100 {
			t.Errorf("%q year %d: got %+v, want aid %d with 100", c.query, c.year, got, c.want)
		}
	}
}
