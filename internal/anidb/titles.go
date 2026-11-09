// Package anidb holds the offline AniDB title data used for matching: the
// daily title dump with its in-memory search, and title language picking.
// The AniDB HTTP API (anime details) is a source in internal/sources.
package anidb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"kizuna/internal/fetch"
)

// The AniDB title dump, mirrored on GitHub by the Anime-Lists project.
// Using the mirror means matching never puts load on AniDB itself.
const titlesURL = "https://raw.githubusercontent.com/Anime-Lists/anime-lists/master/animetitles.xml"

// Titles is the in-memory AniDB title database used for matching.
type Titles struct {
	mu    sync.RWMutex
	byAID map[int][]Title
	index index    // every title, normalized, for searching
	sum   [32]byte // SHA-256 of the dump loaded last (zero after SetForTest)
}

// index holds every normalized title in one string, each followed by a NUL
// (title k is all[starts[k]:starts[k+1]-1]), plus every distinct word of
// those titles, also NUL-separated, with the titles each word is in.
// Query words have no spaces, so a title contains one exactly when one of
// its words does: searching the much shorter word list is enough.
type index struct {
	all    string
	starts []int // len = titles + 1
	aids   []int

	words  string
	wstart []int     // len = words + 1
	posts  [][]int32 // word -> titles holding it, ascending
}

func newIndex(byAID map[int][]Title) index {
	var all, words strings.Builder
	var x index
	ids := map[string]int{}
	for _, aid := range slices.Sorted(maps.Keys(byAID)) {
		for _, ti := range byAID[aid] {
			n := Normalize(ti.Text)
			if n == "" {
				continue
			}
			k := int32(len(x.aids))
			x.starts = append(x.starts, all.Len())
			x.aids = append(x.aids, aid)
			all.WriteString(n)
			all.WriteByte(0)
			for _, w := range strings.Split(n, " ") {
				id, ok := ids[w]
				if !ok {
					id = len(x.posts)
					ids[w] = id
					x.wstart = append(x.wstart, words.Len())
					words.WriteString(w)
					words.WriteByte(0)
					x.posts = append(x.posts, nil)
				}
				if p := x.posts[id]; len(p) == 0 || p[len(p)-1] != k {
					x.posts[id] = append(p, k)
				}
			}
		}
	}
	x.all, x.words = all.String(), words.String()
	x.starts = append(x.starts, len(x.all)+1)
	x.wstart = append(x.wstart, len(x.words)+1)
	return x
}

func (x index) len() int          { return len(x.aids) }
func (x index) norm(k int) string { return x.all[x.starts[k] : x.starts[k+1]-1] }

// containing returns the titles (ascending) that contain any of words
// (as a substring: "kyojin" is in "shingeki no kyojin" and in "kyojinka").
func (x index) containing(words []string) []int {
	var out []int
	for _, w := range words {
		for off := 0; off < len(x.words); {
			i := strings.Index(x.words[off:], w)
			if i < 0 {
				break
			}
			id := sort.SearchInts(x.wstart, off+i+1) - 1 // the word holding the hit
			for _, k := range x.posts[id] {
				out = append(out, int(k))
			}
			off = x.wstart[id+1]
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Match is one search result.
type Match struct {
	AID   int
	Score int // 0-100
}

// Load downloads (at most daily) and parses the title dump. Most days it
// hasn't changed (the server answers 304): then the loaded one is kept.
// A download is parsed in full before it's cached, so a broken one never
// replaces the good cached copy; that parse is used, not repeated.
func (t *Titles) Load(ctx context.Context, f *fetch.Client) error {
	checked := &fetch.Checked[map[int][]Title]{Parse: parseTitles}
	data, err := f.Get(ctx, fetch.Request{URL: titlesURL, Key: "anidb/animetitles.xml", TTL: 24 * time.Hour, Revalidate: true,
		Validate: func(b []byte) error {
			if !bytes.Contains(b[:min(len(b), 500)], []byte("<animetitles")) {
				return errors.New("not an AniDB title dump")
			}
			return checked.Validate(b)
		}})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil { // stopping: Get may still answer with the cached copy
		return err
	}
	sum := sha256.Sum256(data)
	t.mu.RLock()
	same := t.byAID != nil && sum == t.sum
	t.mu.RUnlock()
	if same {
		slog.Debug("AniDB title list unchanged; keeping the loaded one")
		return nil
	}
	byAID, ok := checked.Result(data)
	if !ok { // fetch answered with the cached copy, not the download Validate saw
		if byAID, err = parseTitles(data); err != nil {
			return err
		}
	}
	n := t.set(byAID, sum)
	slog.Info("AniDB title list loaded", "anime", len(byAID), "titles", n)
	return nil
}

// parseTitles reads the title dump; one without any anime is an error.
func parseTitles(data []byte) (map[int][]Title, error) {
	var dump struct {
		Anime []struct {
			AID    int     `xml:"aid,attr"`
			Titles []Title `xml:"title"`
		} `xml:"anime"`
	}
	if err := xml.Unmarshal(data, &dump); err != nil {
		return nil, err
	}
	if len(dump.Anime) == 0 {
		return nil, errors.New("no anime in the title dump")
	}
	byAID := make(map[int][]Title, len(dump.Anime))
	for _, a := range dump.Anime {
		byAID[a.AID] = a.Titles
	}
	return byAID, nil
}

// SetForTest replaces the title list (for tests in other packages).
func (t *Titles) SetForTest(byAID map[int][]Title) { t.set(byAID, [32]byte{}) }

// set swaps in a new title list, its search index and the sum of the dump
// it came from (zero: none). Returns how many titles the index holds.
func (t *Titles) set(byAID map[int][]Title, sum [32]byte) int {
	x := newIndex(byAID)
	t.mu.Lock()
	t.byAID, t.index, t.sum = byAID, x, sum
	t.mu.Unlock()
	return x.len()
}

func (t *Titles) Ready() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byAID != nil
}

// Of returns the titles of one anime (from the dump, no API call).
func (t *Titles) Of(aid int) []Title {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byAID[aid]
}

// Search returns the best matches for a folder/show name, best first.
// Scores: 100 exact, 98 equal after normalizing, otherwise edit similarity
// (only for titles sharing at least one meaningful word, to stay fast).
//
// year (0 = unknown; a "(2011)" in the query counts too) separates remakes
// that share a name: AniDB writes the year into the remake's title
// ("Hunter x Hunter (2011)"), so an entry whose title is the query plus that
// year scores 100 and a plain exact match of another entry drops to 99.
func (t *Titles) Search(query string, year, limit int) []Match {
	q := Normalize(query)
	if q == "" {
		return nil
	}
	var words []string
	for _, w := range strings.Fields(q) {
		if len([]rune(w)) > 3 {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		words = strings.Fields(q)
	}

	t.mu.RLock()
	best := map[int]int{}
	var sim similarity
	qr := []rune(q)
	for _, k := range t.index.containing(words) { // every title equal to q contains its words too
		norm, aid := t.index.norm(k), t.index.aids[k]
		score := 98
		if norm != q {
			// The edit distance is at least the length difference: skip titles
			// that can't beat this entry's best score anyway.
			n := utf8.RuneCountInString(norm)
			if 100*min(n, len(qr))/max(n, len(qr)) <= best[aid] {
				continue
			}
			score = sim.score(qr, norm)
		}
		if score > best[aid] {
			best[aid] = score
		}
	}
	withYear := map[int]bool{} // entries titled "<query> <year>"
	if y := yearOf(query, year); y != "" {
		for aid, s := range best {
			if s < 98 {
				continue
			}
			for _, ti := range t.byAID[aid] {
				if strings.Contains(ti.Text, y) && Normalize(strings.ReplaceAll(ti.Text, y, "")) == q {
					withYear[aid] = true
				}
			}
		}
	}
	for aid := range best { // exact (unnormalized) title match wins outright
		for _, ti := range t.byAID[aid] {
			if strings.EqualFold(ti.Text, strings.TrimSpace(query)) {
				best[aid] = 100
				if len(withYear) > 0 && !withYear[aid] {
					best[aid] = 99 // same name, but another entry carries the year asked for
				}
			}
		}
	}
	for aid := range withYear {
		best[aid] = 100
	}
	t.mu.RUnlock()

	out := make([]Match, 0, len(best))
	for aid, s := range best {
		if s > 0 {
			out = append(out, Match{aid, s})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].AID < out[j].AID // older entry first on ties (the original series)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Score scores a name against one anime's titles the way Search scores a
// title (100 exact, 98 equal after normalizing, else edit similarity up to
// 97), but without the index or the year: for comparing a few known entries.
// 0 when either side is empty.
func Score(query string, titles []Title) int {
	q := Normalize(query)
	if q == "" {
		return 0
	}
	var sim similarity
	qr := []rune(q)
	best := 0
	for _, ti := range titles {
		norm := Normalize(ti.Text)
		switch {
		case norm == "":
		case strings.EqualFold(ti.Text, strings.TrimSpace(query)):
			return 100
		case norm == q:
			best = max(best, 98)
		default:
			best = max(best, sim.score(qr, norm))
		}
	}
	return best
}

var queryYearRe = regexp.MustCompile(`\(((?:19|20)\d\d)\)`)

// yearOf returns the year to look for: the one given, else a "(2011)" in
// the query, else "".
func yearOf(query string, year int) string {
	if year >= 1900 && year < 2100 {
		return strconv.Itoa(year)
	}
	if m := queryYearRe.FindStringSubmatch(query); m != nil {
		return m[1]
	}
	return ""
}
