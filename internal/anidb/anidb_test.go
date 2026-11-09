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

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"Shingeki no Kyojin (2013)": "shingeki no kyojin",
		"Hunter × Hunter [TV]":      "hunter hunter",
		"Pokémon":                   "pokemon",      // é as one rune
		"Pokémon":                  "pokemon",      // e + combining accent (macOS file names)
		"İstanbul":                  "istanbul",     // İ lowercases to i + combining dot
		"Ōkido's “Lab”":            "okidos lab",   // Ō decomposed; apostrophes join
		"Journey`s End":             "journeys end", // AniDB's backtick apostrophe
		"Re:Zero  kara":             "re zero kara", // punctuation and runs of spaces
		"Straße":                    "strasse",
		"́ lone mark":               "lone mark", // a mark with no letter before it
		"ワンピース":                     "ワンピース",     // other scripts stay
		"Love Live! School Idol":    "love live school idol",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearch(t *testing.T) {
	ts := &Titles{byAID: map[int][]Title{1: {{"main", "x-jat", "Shingeki no Kyojin"}}, 2: {{"main", "x-jat", "Shingeki no Kyojin Season 2"}}}}
	ts.SetForTest(ts.byAID)
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
	// AniDB writes some codes with capitals ("zh-Hans"); settings are lowercase.
	if got := FindTitle([]Title{{"official", "zh-Hans", "中文"}}, []string{"zh-hans"}, true); got != "中文" {
		t.Errorf("zh-Hans: got %q", got)
	}
}

func TestSearchYearPicksRemake(t *testing.T) {
	ts := &Titles{byAID: map[int][]Title{
		1:    {{"main", "x-jat", "Hunter x Hunter"}},
		8074: {{"main", "x-jat", "Hunter x Hunter (2011)"}, {"official", "en", "Hunter x Hunter"}},
	}}
	ts.SetForTest(ts.byAID)
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

func TestTitleApostrophes(t *testing.T) {
	titles := []Title{
		{Type: "main", Lang: "x-jat", Text: "Sousou no Frieren"},
		{Type: "official", Lang: "en", Text: "The Journey`s End"},
	}
	if got := FindTitle(titles, []string{"en"}, false); got != "The Journey's End" {
		t.Errorf("FindTitle: %q", got)
	}
	if got := PickTitle([]Title{{Type: "syn", Lang: "en", Text: "Kino`s Journey"}}, []string{"ja"}, true); got != "Kino's Journey" {
		t.Errorf("PickTitle fallback: %q", got)
	}
	if got := CleanSummary("It`s over."); got != "It's over." {
		t.Errorf("CleanSummary: %q", got)
	}
}
