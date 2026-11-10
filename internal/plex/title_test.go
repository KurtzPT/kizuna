package plex

import "testing"

func TestCreditOf(t *testing.T) {
	for _, c := range []struct {
		stem, title string
		creditless  bool
	}{
		{"OP01", "Opening 1", false},
		{"OP01a", "Opening 1a", false},
		{"OP02", "Opening 2", false},
		{"[Group] Show - OP1 (640x480 h264) [A7E9E20B]", "Opening 1", false},
		{"[Group] Show - ED1 (640x480 h264) [3B54404B]", "Ending 1", false},
		{"Show - NCOP", "Opening", true},
		{"Show - NCED2", "Ending 2", true},
		{"Show - NCOP1b [1080p]", "Opening 1b", true},
		{"Show.OP.2.B.mkv", "Opening 2b", false},
		{"Show_ED_03", "Ending 3", false},
		{"Show - OP 1 b", "Opening 1b", false},
		{"Show - OP1v2", "Opening 1 v2", false},
		{"Show - OP1av3", "Opening 1a v3", false},
		{"Show - OP1v1", "Opening 1", false},
		{"Show - ED a", "Ending a", false},
		{"Show - OPED", "Opening & Ending", false},
		{"Show - Creditless Opening", "Opening", true},
		{"Show - Clean Ending 2", "Ending 2", true},
		{"Show - Creditless.Opening.1a", "Opening 1a", true},
		{"Show - ED 12 - Song", "Ending 12", false},
		{"Show - 05 [ED fix]", "Ending", false}, // Plex makes this episode an extra too
	} {
		got, ok := CreditOf(c.stem)
		if !ok || got.Title != c.title || got.Creditless != c.creditless {
			t.Errorf("CreditOf(%q) = %+v, %v; want %q, creditless %v", c.stem, got, ok, c.title, c.creditless)
		}
	}
	// Not an opening or ending for Plex: no pattern, a lowercase word, a word
	// inside another, and a spelled-out word without Creditless or Clean.
	for _, stem := range []string{"Show - 01", "Show - op1", "Show - TOP1", "Show - OP123", "Show - Opening 1",
		"Show - Ending", "Show - NCOpening 1", "Show - S00E101", "Show - EDv", ""} {
		if got, ok := CreditOf(stem); ok {
			t.Errorf("CreditOf(%q) = %+v, want none", stem, got)
		}
	}
}

func TestStem(t *testing.T) {
	for path, want := range map[string]string{
		"/media/Anime/Show [anidb-1]/Show - OP1.mkv": "Show - OP1",
		`D:\Anime\Show\NCED2.v2.mkv`:                 "NCED2.v2",
		"OP1":                                        "OP1",
		".hidden":                                    ".hidden",
	} {
		if got := stem(path); got != want {
			t.Errorf("stem(%q) = %q, want %q", path, got, want)
		}
	}
}
