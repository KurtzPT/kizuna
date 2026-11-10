// Package plex renames the opening and ending extras Plex makes from local
// files ("OP1", "NCED2a") to readable titles ("Opening 1", "Ending 2a"),
// through Plex's own HTTP API. See MIGRATION.md ("Openings and endings").
package plex

import (
	"regexp"
	"strconv"
	"strings"
)

// creditRe is the pattern Plex's scanner (Plex Media Server 1.43) looks for
// in a file name to make the file an opening or ending extra:
//
//	(?:\b|_|\.)(?<title>(?:NC|(?:Creditless|Clean)[ ._])?(?<type>OP|ED|Opening|Ending|OPED)
//	(?:[ ._]?(?<index>\d{1,2})[A-Za-z]?)?(?:[ ._][A-Za-z])?)(?:v\d)?(?:\b|_|\.)
//
// with groups added for the letters and the version. Like Plex it is matched
// with case, and the first match in the name counts.
var creditRe = regexp.MustCompile(`(?:\b|_|\.)(NC|(?:Creditless|Clean)[ ._])?(OP|ED|Opening|Ending|OPED)` +
	`(?:[ ._]?(\d{1,2})([A-Za-z])?)?(?:[ ._]([A-Za-z]))?(?:v(\d))?(?:\b|_|\.)`)

// A Credit is what a file name says about an opening or ending.
type Credit struct {
	Title      string // "Opening 1a", "Ending 2 v2", "Opening & Ending"
	Creditless bool   // the name has NC, Creditless or Clean
}

// CreditOf reads the opening or ending a file name (without its folder and
// extension) stands for, the way Plex's scanner does: OP1 is "Opening 1",
// NCED02a "Ending 2a", "OP 1 b" "Opening 1b", OP1v2 "Opening 1 v2". ok is
// false for a name Plex doesn't make an opening or ending of: one without the
// pattern, or with a spelled-out Opening or Ending that lacks Creditless or
// Clean in front (Plex leaves that file out of the library).
func CreditOf(stem string) (c Credit, ok bool) {
	m := creditRe.FindStringSubmatch(stem)
	if m == nil {
		return Credit{}, false
	}
	prefix, word, number, letter, version := m[1], m[2], m[3], m[4], m[6]
	if letter == "" {
		letter = m[5]
	}
	if (word == "Opening" || word == "Ending") && (prefix == "" || prefix == "NC") {
		return Credit{}, false
	}
	switch {
	case word == "OPED":
		c.Title = "Opening & Ending"
	case strings.HasPrefix(word, "O"):
		c.Title = "Opening"
	default:
		c.Title = "Ending"
	}
	if n, err := strconv.Atoi(number); err == nil { // "01" is 1
		c.Title += " " + strconv.Itoa(n)
	}
	if letter != "" {
		if number == "" {
			c.Title += " "
		}
		c.Title += strings.ToLower(letter)
	}
	if v, err := strconv.Atoi(version); err == nil && v > 1 {
		c.Title += " v" + version
	}
	c.Creditless = prefix != ""
	return c, true
}

// stem is a file path's name without its folders and extension. Plex's
// paths may use either slash (a Windows server).
func stem(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		path = path[i+1:]
	}
	if i := strings.LastIndexByte(path, '.'); i > 0 {
		path = path[:i]
	}
	return path
}
