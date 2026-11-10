package provider

import (
	"context"
	"strings"
	"testing"
)

// What Plex sends (a title, a file path, a GUID) is logged as it came, and
// may hold a line break. The log is slog's text format, as in main: a value
// with a line break is quoted and the break written as \n, so the value
// can't end its line and start one that reads like another entry.
func TestLoggedValuesStayOnOneLine(t *testing.T) {
	l, titles := onePiece()
	p := &Provider{lists: l, titles: titles, tagsLogged: everyTenMinutes{max: 1000}}
	const forged = "time=2026-01-01T00:00:00Z level=ERROR msg=forged"
	r := MatchRequest{Type: 2,
		Title:    "One Piece [anidb-69] {mal-21}\n" + forged,
		Filename: "/anime/One Piece\r\n" + forged + "/01.mkv",
		GUID:     "plex://show/1\n" + forged,
	}
	var logs strings.Builder
	restore := captureLogs(&logs)
	items, err := p.Match(context.Background(), Modes["anidb"], r)
	restore()
	if err != nil || len(items) != 0 {
		t.Fatalf("two id tags: got %d items, err %v, want no match", len(items), err)
	}
	var match []string // this request's lines: the logger is global, other tests' background work may log too
	for _, line := range strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n") {
		if strings.HasPrefix(line, forged) || strings.HasPrefix(line, "/01.mkv") {
			t.Errorf("a logged value started a line of its own: %q", line)
		}
		if strings.Contains(line, `msg="match: `) {
			match = append(match, line)
		}
	}
	if len(match) != 2 { // the warning about two id tags, then "nothing found"
		t.Fatalf("got %d match lines, want 2:\n%s", len(match), logs.String())
	}
	if want := `title="One Piece [anidb-69] {mal-21}\n` + forged + `"`; !strings.Contains(match[0], "level=WARN") || !strings.Contains(match[0], want) {
		t.Errorf("warning line %q, want level=WARN with %s", match[0], want)
	}
	for _, want := range []string{
		`title="One Piece [anidb-69] {mal-21}\n` + forged + `"`,
		`file="/anime/One Piece\r\n` + forged + `/01.mkv"`,
		`guid="plex://show/1\n` + forged + `"`,
	} {
		if !strings.Contains(match[1], want) {
			t.Errorf("line %q, want it to hold %s", match[1], want)
		}
	}
}
