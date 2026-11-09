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

// FindTitle returns the best title for a language priority list (lowercase
// codes; AniDB's match ignoring case), or "" if no listed language has one:
// the first language with a title wins; "main"
// means AniDB's main title; "short" titles are never used. With
// officialOnly, only main and official titles count (no synonyms). AniDB's
// backtick apostrophes come back as plain ones ("Journey`s" -> "Journey's").
func FindTitle(titles []Title, langs []string, officialOnly bool) string {
	return apostrophes(findTitle(titles, langs, officialOnly))
}

func findTitle(titles []Title, langs []string, officialOnly bool) string {
	best := map[string]Title{}
	main := ""
	for _, t := range titles {
		if t.Text == "" || t.Type == "short" || (officialOnly && t.Type != "main" && t.Type != "official") {
			continue
		}
		if t.Type == "main" {
			main = t.Text
		}
		lang := strings.ToLower(t.Lang) // AniDB has "zh-Hans"; langs are lowercase
		if cur, ok := best[lang]; !ok || rank(t.Type) < rank(cur.Type) {
			best[lang] = t
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
			return apostrophes(t.Text)
		}
	}
	for _, t := range titles { // anything at all
		if t.Text != "" {
			return apostrophes(t.Text)
		}
	}
	return ""
}

// apostrophes turns AniDB's backtick apostrophes into plain ones.
func apostrophes(s string) string { return strings.ReplaceAll(s, "`", "'") }

func rank(typ string) int {
	if r, ok := typeRank[typ]; ok {
		return r
	}
	return 7
}

var (
	reBrackets = regexp.MustCompile(`\([^()]*\)|\[[^\[\]]*\]|\{[^{}]*\}`)
	// accents folds a letter with accents into the letter without them, for
	// every precomposed letter of Latin-1 Supplement, Latin Extended-A and -B,
	// Latin Extended Additional, Greek and Cyrillic (lowercase: Normalize
	// lowercases first). Their decomposed form (NFD: the letter, then
	// combining accents, as macOS writes file names) loses the accents in
	// Normalize, so both forms end up the same. A few letters without a
	// decomposition fold too (ø, ß, ł, đ, ħ, ı), as do apostrophes.
	accents = strings.NewReplacer(
		"ß", "ss", "à", "a", "á", "a", "â", "a", "ã", "a", "ä", "a", "å", "a", "ç", "c", "è", "e", "é", "e",
		"ê", "e", "ë", "e", "ì", "i", "í", "i", "î", "i", "ï", "i", "ñ", "n", "ò", "o", "ó", "o", "ô", "o",
		"õ", "o", "ö", "o", "ø", "o", "ù", "u", "ú", "u", "û", "u", "ü", "u", "ý", "y", "ÿ", "y", "ā", "a",
		"ă", "a", "ą", "a", "ć", "c", "ĉ", "c", "ċ", "c", "č", "c", "ď", "d", "đ", "d", "ē", "e", "ĕ", "e",
		"ė", "e", "ę", "e", "ě", "e", "ĝ", "g", "ğ", "g", "ġ", "g", "ģ", "g", "ĥ", "h", "ħ", "h", "ĩ", "i",
		"ī", "i", "ĭ", "i", "į", "i", "ı", "i", "ĵ", "j", "ķ", "k", "ĺ", "l", "ļ", "l", "ľ", "l", "ł", "l",
		"ń", "n", "ņ", "n", "ň", "n", "ō", "o", "ŏ", "o", "ő", "o", "ŕ", "r", "ŗ", "r", "ř", "r", "ś", "s",
		"ŝ", "s", "ş", "s", "š", "s", "ţ", "t", "ť", "t", "ũ", "u", "ū", "u", "ŭ", "u", "ů", "u", "ű", "u",
		"ų", "u", "ŵ", "w", "ŷ", "y", "ź", "z", "ż", "z", "ž", "z", "ơ", "o", "ư", "u", "ǎ", "a", "ǐ", "i",
		"ǒ", "o", "ǔ", "u", "ǖ", "u", "ǘ", "u", "ǚ", "u", "ǜ", "u", "ǟ", "a", "ǡ", "a", "ǣ", "æ", "ǧ", "g",
		"ǩ", "k", "ǫ", "o", "ǭ", "o", "ǯ", "ʒ", "ǰ", "j", "ǵ", "g", "ǹ", "n", "ǻ", "a", "ǽ", "æ", "ǿ", "o",
		"ȁ", "a", "ȃ", "a", "ȅ", "e", "ȇ", "e", "ȉ", "i", "ȋ", "i", "ȍ", "o", "ȏ", "o", "ȑ", "r", "ȓ", "r",
		"ȕ", "u", "ȗ", "u", "ș", "s", "ț", "t", "ȟ", "h", "ȧ", "a", "ȩ", "e", "ȫ", "o", "ȭ", "o", "ȯ", "o",
		"ȱ", "o", "ȳ", "y", "ʹ", "ʹ", "ΐ", "ι", "ά", "α", "έ", "ε", "ή", "η", "ί", "ι", "ΰ", "υ", "ϊ", "ι",
		"ϋ", "υ", "ό", "ο", "ύ", "υ", "ώ", "ω", "ϓ", "ϒ", "ϔ", "ϒ", "й", "и", "ѐ", "е", "ё", "е", "ѓ", "г",
		"ї", "і", "ќ", "к", "ѝ", "и", "ў", "у", "ѷ", "ѵ", "ӂ", "ж", "ӑ", "а", "ӓ", "а", "ӗ", "е", "ӛ", "ә",
		"ӝ", "ж", "ӟ", "з", "ӣ", "и", "ӥ", "и", "ӧ", "о", "ӫ", "ө", "ӭ", "э", "ӯ", "у", "ӱ", "у", "ӳ", "у",
		"ӵ", "ч", "ӹ", "ы", "ḁ", "a", "ḃ", "b", "ḅ", "b", "ḇ", "b", "ḉ", "c", "ḋ", "d", "ḍ", "d", "ḏ", "d",
		"ḑ", "d", "ḓ", "d", "ḕ", "e", "ḗ", "e", "ḙ", "e", "ḛ", "e", "ḝ", "e", "ḟ", "f", "ḡ", "g", "ḣ", "h",
		"ḥ", "h", "ḧ", "h", "ḩ", "h", "ḫ", "h", "ḭ", "i", "ḯ", "i", "ḱ", "k", "ḳ", "k", "ḵ", "k", "ḷ", "l",
		"ḹ", "l", "ḻ", "l", "ḽ", "l", "ḿ", "m", "ṁ", "m", "ṃ", "m", "ṅ", "n", "ṇ", "n", "ṉ", "n", "ṋ", "n",
		"ṍ", "o", "ṏ", "o", "ṑ", "o", "ṓ", "o", "ṕ", "p", "ṗ", "p", "ṙ", "r", "ṛ", "r", "ṝ", "r", "ṟ", "r",
		"ṡ", "s", "ṣ", "s", "ṥ", "s", "ṧ", "s", "ṩ", "s", "ṫ", "t", "ṭ", "t", "ṯ", "t", "ṱ", "t", "ṳ", "u",
		"ṵ", "u", "ṷ", "u", "ṹ", "u", "ṻ", "u", "ṽ", "v", "ṿ", "v", "ẁ", "w", "ẃ", "w", "ẅ", "w", "ẇ", "w",
		"ẉ", "w", "ẋ", "x", "ẍ", "x", "ẏ", "y", "ẑ", "z", "ẓ", "z", "ẕ", "z", "ẖ", "h", "ẗ", "t", "ẘ", "w",
		"ẙ", "y", "ẛ", "ſ", "ạ", "a", "ả", "a", "ấ", "a", "ầ", "a", "ẩ", "a", "ẫ", "a", "ậ", "a", "ắ", "a",
		"ằ", "a", "ẳ", "a", "ẵ", "a", "ặ", "a", "ẹ", "e", "ẻ", "e", "ẽ", "e", "ế", "e", "ề", "e", "ể", "e",
		"ễ", "e", "ệ", "e", "ỉ", "i", "ị", "i", "ọ", "o", "ỏ", "o", "ố", "o", "ồ", "o", "ổ", "o", "ỗ", "o",
		"ộ", "o", "ớ", "o", "ờ", "o", "ở", "o", "ỡ", "o", "ợ", "o", "ụ", "u", "ủ", "u", "ứ", "u", "ừ", "u",
		"ử", "u", "ữ", "u", "ự", "u", "ỳ", "y", "ỵ", "y", "ỷ", "y", "ỹ", "y",
		"'", "", "’", "", "`", "")
)

// Normalize makes titles comparable: lowercase, no bracketed parts, no
// accents or punctuation, single spaces. "Shingeki no Kyojin (2013)" ->
// "shingeki no kyojin". A title written precomposed (NFC, as AniDB does)
// and decomposed (NFD) comes out the same.
func Normalize(s string) string {
	s = accents.Replace(strings.ToLower(compose(s, voiceLead, voice)))
	s = reBrackets.ReplaceAllString(s, " ")
	var b strings.Builder
	space := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
		} else if unicode.Is(unicode.Mn, r) {
			// A combining accent (decomposed "é" from macOS, the dot of
			// lowercased "İ") belongs to the letter before it: drop it.
			continue
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	// Jamo join last: a dropped accent or apostrophe may have stood between
	// two (and normalizing again must not join more).
	return compose(strings.TrimSpace(b.String()), hangulLead, hangul)
}

// Kana voicing marks, as NFD writes them after the kana: か + U+3099 is
// NFC's が, は + U+309A is ぱ. Each string pairs a kana with its voiced form.
var (
	voiced     = pairs("かがきぎくぐけげこごさざしじすずせぜそぞただちぢつづてでとどはばひびふぶへべほぼうゔゝゞカガキギクグケゲコゴサザシジスズセゼソゾタダチヂツヅテデトドハバヒビフブヘベホボウヴワヷヰヸヱヹヲヺヽヾ")
	semiVoiced = pairs("はぱひぴふぷへぺほぽハパヒピフプヘペホポ")
)

func pairs(s string) map[rune]rune {
	r := []rune(s)
	m := make(map[rune]rune, len(r)/2)
	for i := 0; i+1 < len(r); i += 2 {
		m[r[i]] = r[i+1]
	}
	return m
}

// Hangul syllables and their jamo (the Unicode composition algorithm).
const (
	hangulS, hangulL, hangulV, hangulT = 0xAC00, 0x1100, 0x1161, 0x11A7
	countL, countV, countT             = 19, 21, 28
)

// voice joins a kana and the voicing mark NFD writes after it (か + U+3099
// -> が). Dropped like an accent, the mark would make が match か: another
// word, not another spelling.
func voice(prev, r rune) (rune, bool) {
	switch r {
	case 0x3099:
		c, ok := voiced[prev]
		return c, ok
	case 0x309A:
		c, ok := semiVoiced[prev]
		return c, ok
	}
	return 0, false
}

// hangul joins Hangul jamo into their syllable, as NFC writes it: a leading
// consonant and a vowel, then a trailing consonant.
func hangul(prev, r rune) (rune, bool) {
	switch {
	case r >= hangulV && r < hangulV+countV && prev >= hangulL && prev < hangulL+countL:
		return hangulS + ((prev-hangulL)*countV+r-hangulV)*countT, true
	case r > hangulT && r < hangulT+countT && prev >= hangulS && prev < hangulS+countL*countV*countT &&
		(prev-hangulS)%countT == 0:
		return prev + r - hangulT, true
	}
	return 0, false
}

// The UTF-8 bytes every rune voice or hangul joins to the one before it
// starts with (U+3099, U+309A; U+1161-U+11C2): a string without them has
// nothing to join.
const voiceLead, hangulLead = "\xe3\x82", "\xe1"

// compose replaces each rune and the one before it with what join makes of
// them; lead is the bytes join's second runes start with. With nothing to
// join, s comes back as is (no copy).
func compose(s, lead string, join func(prev, r rune) (rune, bool)) string {
	if !strings.Contains(s, lead) {
		return s
	}
	var out []rune // nil until something joins
	prev := rune(-1)
	for i, r := range s {
		if c, ok := join(prev, r); ok {
			if out == nil {
				out = []rune(s[:i]) // ends with prev
			}
			out[len(out)-1], prev = c, c
			continue
		}
		if out != nil {
			out = append(out, r)
		}
		prev = r
	}
	if out == nil {
		return s
	}
	return string(out)
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
	return strings.TrimSpace(apostrophes(s))
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
