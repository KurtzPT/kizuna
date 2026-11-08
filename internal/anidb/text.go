package anidb

import (
	"regexp"
	"strings"
	"unicode"
)

// Title is one AniDB title. Type is "main", "official", "syn", "short"...
// (empty for episode titles); Lang is AniDB's xml:lang ("x-jat", "en", "ja").
type Title struct {
	Type string `xml:"type,attr"`
	Lang string `xml:"lang,attr"`
	Text string `xml:",chardata"`
}

// typeRank: lower is better when a language has several titles.
var typeRank = map[string]int{"main": 1, "official": 2, "kana": 3, "card": 4, "syn": 5, "synonym": 5, "": 6}

// FindTitle returns the best title for a language priority list, or "" if
// no listed language has one: the first language with a title wins; "main"
// means AniDB's main title; "short" titles are never used. With
// officialOnly, only main and official titles count (no synonyms).
func FindTitle(titles []Title, langs []string, officialOnly bool) string {
	best := map[string]Title{}
	main := ""
	for _, t := range titles {
		if t.Text == "" || t.Type == "short" || (officialOnly && t.Type != "main" && t.Type != "official") {
			continue
		}
		if t.Type == "main" {
			main = t.Text
		}
		if cur, ok := best[t.Lang]; !ok || rank(t.Type) < rank(cur.Type) {
			best[t.Lang] = t
		}
	}
	for _, l := range langs {
		if l == "main" && main != "" {
			return main
		}
		if t, ok := best[l]; ok {
			return t.Text
		}
	}
	return ""
}

// PickTitle is FindTitle that never comes back empty: it falls back to the
// main title, then to any title at all.
func PickTitle(titles []Title, langs []string, officialOnly bool) string {
	if t := FindTitle(titles, langs, officialOnly); t != "" {
		return t
	}
	for _, t := range titles {
		if t.Type == "main" && t.Text != "" {
			return t.Text
		}
	}
	for _, t := range titles { // anything at all
		if t.Text != "" {
			return t.Text
		}
	}
	return ""
}

func rank(typ string) int {
	if r, ok := typeRank[typ]; ok {
		return r
	}
	return 7
}

var (
	reBrackets = regexp.MustCompile(`\([^()]*\)|\[[^\[\]]*\]|\{[^{}]*\}`)
	accents    = strings.NewReplacer(
		"à", "a", "á", "a", "â", "a", "ä", "a", "ã", "a", "å", "a", "ā", "a",
		"è", "e", "é", "e", "ê", "e", "ë", "e", "ē", "e",
		"ì", "i", "í", "i", "î", "i", "ï", "i", "ī", "i",
		"ò", "o", "ó", "o", "ô", "o", "ö", "o", "õ", "o", "ō", "o", "ø", "o",
		"ù", "u", "ú", "u", "û", "u", "ü", "u", "ū", "u",
		"ç", "c", "ñ", "n", "ß", "ss", "'", "", "’", "", "`", "")
)

// Normalize makes titles comparable: lowercase, no bracketed parts, no
// accents or punctuation, single spaces. "Shingeki no Kyojin (2013)" ->
// "shingeki no kyojin".
func Normalize(s string) string {
	s = accents.Replace(strings.ToLower(s))
	s = reBrackets.ReplaceAllString(s, " ")
	var b strings.Builder
	space := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

var (
	reAniDBLink = regexp.MustCompile(`https?://anidb\.net/\S+ \[([^\]]+)\]`)
	reNoteLines = regexp.MustCompile(`(?m)^(\*|--|~) .*$`)
	reSource    = regexp.MustCompile(`(?s)\n(Source|Note|Summary):.*`)
	reBlank     = regexp.MustCompile(`\n{3,}`)
)

// CleanSummary removes AniDB markup: "http://anidb.net/ch123 [Name]" -> "Name",
// note lines and "Source: ..." trailers.
func CleanSummary(s string) string {
	s = reAniDBLink.ReplaceAllString(s, "$1")
	s = reNoteLines.ReplaceAllString(s, "")
	s = reSource.ReplaceAllString(s, "")
	s = reBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(strings.ReplaceAll(s, "`", "'"))
}

// similarity scores two normalized titles 0..97 (exact matches score higher
// elsewhere): 100 * (1 - edit distance / longer length). Its buffers are
// reused between calls, so one value scores many titles without allocating;
// not safe for concurrent use.
type similarity struct {
	b         []rune
	prev, cur []int
}

func (s *similarity) score(a []rune, bs string) int {
	s.b = s.b[:0]
	for _, r := range bs {
		s.b = append(s.b, r)
	}
	b := s.b
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if cap(s.prev) < len(b)+1 {
		s.prev, s.cur = make([]int, len(b)+1), make([]int, len(b)+1)
	}
	prev, cur := s.prev[:len(b)+1], s.cur[:len(b)+1]
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	longer := max(len(a), len(b))
	return min(97, 100*(longer-prev[len(b)])/longer)
}
