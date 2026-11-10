package report

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"kizuna/internal/fetch"
)

// Store writes the report pages to a folder: <key>.html per show or
// unmatched title, index.html, and their data in data/<key>.json, which the
// next update of a page starts from. Updates are batched: Plex asks many
// things at once, so a page is written at most every flushEvery, and the
// index (which lists every page) at most every indexEvery.
//
// All methods do nothing on a nil Store.
type Store struct {
	dir     string
	version string

	flushMu    sync.Mutex // one flush at a time: they write the same files
	mu         sync.Mutex
	pending    map[string]*pending
	writing    map[string]*pending // the updates flush is writing now
	index      map[string]IndexRow
	indexDirty bool
	indexAt    time.Time // last index write
	wake       chan struct{}
}

const (
	// flushEvery is how long updates wait to be written together.
	flushEvery = 2 * time.Second
	// unmatchedMaxAge: an unmatched title's page goes once Plex hasn't asked
	// about the title for this long (renamed, matched another way, or gone
	// from the library). page.html says it in words ("after 30 days");
	// TestPageRepeatsLimits keeps them in step.
	unmatchedMaxAge = 30 * 24 * time.Hour
)

// indexEvery is how often the index is rewritten while pages change: a scan
// touches many pages, and the index lists them all. A var for the tests.
var indexEvery = 30 * time.Second

// maxFails: a page whose updates failed to write this many times in a row
// drops them, so a folder that stays unwritable doesn't grow the queue.
const maxFails = 5

type pending struct {
	builds    []*Recorder
	requests  []Request
	unmatched *Match // a new no-match answer for the title
	drop      bool   // the title matched after all: remove its page
	fails     int    // failed writes of these updates
}

// merge adds the updates queued after p, which failed to write.
func (p *pending) merge(later *pending) {
	if later.drop {
		*p = pending{drop: true, fails: p.fails}
		return
	}
	p.builds = append(p.builds, later.builds...)
	p.requests = append(p.requests, later.requests...)
	if later.unmatched != nil {
		p.unmatched, p.drop = later.unmatched, false
	}
}

// Open uses dir (created if needed) and reads the existing index. Unmatched
// pages older than unmatchedMaxAge are removed.
func Open(dir, version string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, version: version, pending: map[string]*pending{}, index: map[string]IndexRow{}, wake: make(chan struct{}, 1)}
	var rows []IndexRow
	if data, err := os.ReadFile(s.dataPath("index")); err == nil {
		if err := json.Unmarshal(data, &rows); err != nil {
			slog.Warn("reports: index unreadable; starting a new one", "err", err)
		}
	}
	known := map[string]bool{"index": true}
	for _, r := range rows {
		known[r.Key] = true
		s.openRow(r, false)
	}
	// Pages the index doesn't list: written less than indexEvery before a
	// crash, or the index was unreadable. Unlisted, they would never expire
	// or go once their title matches.
	files, err := os.ReadDir(filepath.Join(dir, "data"))
	if err != nil {
		slog.Warn("reports: can't list the saved pages", "err", err)
	}
	for _, f := range files {
		key, ok := strings.CutSuffix(f.Name(), ".json")
		if !ok || known[key] || f.IsDir() {
			continue
		}
		if r, ok := s.rowFromData(key); ok {
			s.openRow(r, true)
		}
	}
	return s, nil
}

// openRow takes an index row into the index at Open: an expired one is
// removed, one whose page is gone is dropped. added: the saved index
// didn't list it.
func (s *Store) openRow(r IndexRow, added bool) {
	if expired(r) {
		s.indexDirty = true
		if err := s.remove(r.Key); err != nil {
			slog.Warn("reports: can't remove an old unmatched page", "page", r.Key, "err", err)
		}
		return
	}
	if _, err := os.Stat(s.pagePath(r.Key)); err == nil {
		s.index[r.Key] = r
		s.indexDirty = s.indexDirty || added
	} else {
		s.indexDirty = s.indexDirty || !added // its page is gone: the index page drops the link
	}
}

// rowFromData rebuilds the index row of a page from its saved data; false
// if the data is unreadable or not a page.
func (s *Store) rowFromData(key string) (IndexRow, bool) {
	data, err := os.ReadFile(s.dataPath(key))
	if err != nil {
		slog.Warn("reports: can't read a page's saved data", "page", key, "err", err)
		return IndexRow{}, false
	}
	if strings.HasPrefix(key, "unmatched-") {
		var doc UnmatchedDoc
		if json.Unmarshal(data, &doc) != nil || doc.Match.Title == "" {
			return IndexRow{}, false
		}
		doc.Key = key
		return unmatchedRow(doc), true
	}
	var doc ShowDoc
	if json.Unmarshal(data, &doc) != nil {
		return IndexRow{}, false
	}
	doc.Key = key
	return showRow(doc), true
}

// Run writes pending updates every flushEvery until ctx ends, then
// everything once more.
func (s *Store) Run(ctx context.Context) {
	if s == nil {
		return
	}
	for {
		var indexDue <-chan time.Time // an index change left for later
		s.mu.Lock()
		if s.indexDirty {
			indexDue = time.After(indexEvery - time.Since(s.indexAt))
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			s.Flush()
			return
		case <-indexDue:
		case <-s.wake:
			select {
			case <-ctx.Done():
			case <-time.After(flushEvery):
			}
		}
		s.flush(false)
	}
}

func (s *Store) touch(key string) *pending {
	p := s.pending[key]
	if p == nil {
		p = &pending{}
		s.pending[key] = p
	}
	s.signal()
	return p
}

func (s *Store) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// AddBuild queues a finished build for the show page key. The build is read
// when the page is written, so downloads still finishing are included; one
// that finishes after that queues the build again, and the page's copy of
// it is replaced.
func (s *Store) AddBuild(key string, r *Recorder) {
	if s == nil || r == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.touch(key)
	p.builds = append(p.builds, r)
}

// AddRequest queues a Plex request for the page rq.Key.
func (s *Store) AddRequest(rq Request) {
	if s == nil || rq.Key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.touch(rq.Key)
	p.requests = append(p.requests, rq)
}

// Unmatched records a match request that found nothing and returns the key
// of its page.
func (s *Store) Unmatched(m Match) string {
	key := UnmatchedKey(m.Title)
	if s == nil {
		return key
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.touch(key)
	p.unmatched, p.drop = &m, false
	return key
}

// Matched removes the page of a title that now matches.
func (s *Store) Matched(title string) {
	if s == nil {
		return
	}
	key := UnmatchedKey(title)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, listed := s.index[key]; !listed && s.pending[key] == nil && s.writing[key] == nil {
		return // no page: one being written isn't in the index yet
	}
	p := s.touch(key)
	*p = pending{drop: true}
}

// idTagRe: an id tag like [anidb-1], {tvdb2-5} or [imdb-tt0388629].
var idTagRe = regexp.MustCompile(`(?i)[\[{][a-z]+\d?-[a-z]*\d+[\]}]`)

// normTitle is how titles compare for unmatched pages: without id tags,
// lower case, and every run of characters other than letters and digits
// one space. "Show [anidb-1]" and "show" are the same title.
func normTitle(title string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(idTagRe.ReplaceAllString(title, " ")) {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			gap = true
			continue
		}
		if gap && b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		gap = false
	}
	return b.String()
}

// UnmatchedKey names an unmatched title's page: readable, safe as a file
// name, and one per title whatever the year (Plex's year is often missing
// or off, and the page must go once the title matches).
func UnmatchedKey(title string) string {
	norm := normTitle(title)
	var b strings.Builder
	dash := false
	for _, r := range norm {
		if r < unicode.MaxASCII && r != ' ' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 48 {
			break
		}
	}
	h := fnv.New32a()
	h.Write([]byte(norm))
	return fmt.Sprintf("unmatched-%s-%08x", cmp.Or(strings.Trim(b.String(), "-"), "title"), h.Sum32())
}

// Flush writes every pending update and the index now.
func (s *Store) Flush() { s.flush(true) }

// flush writes the pending updates, and the index if it changed and index
// is set or it was last written indexEvery ago.
func (s *Store) flush(index bool) {
	if s == nil {
		return
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	work := s.pending
	s.pending, s.writing = map[string]*pending{}, work
	s.mu.Unlock()

	keys := make([]string, 0, len(work))
	for k := range work {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, key := range keys {
		var err error
		if strings.HasPrefix(key, "unmatched-") {
			err = s.writeUnmatched(key, work[key])
		} else {
			err = s.writeShow(key, work[key])
		}
		if err != nil {
			s.retry(key, work[key], err)
		}
	}

	s.mu.Lock()
	s.writing = nil
	due := s.indexDirty && (index || time.Since(s.indexAt) >= indexEvery)
	rows := []IndexRow{}
	var old []string
	if due {
		s.indexDirty, s.indexAt = false, time.Now()
		for key, r := range s.index {
			if expired(r) {
				old = append(old, key)
				delete(s.index, key)
			} else {
				rows = append(rows, r)
			}
		}
	}
	s.mu.Unlock()
	if !due {
		return
	}
	for _, key := range old {
		if err := s.remove(key); err != nil {
			slog.Warn("reports: can't remove an old unmatched page", "page", key, "err", err)
		}
	}
	slices.SortFunc(rows, func(a, b IndexRow) int { return b.Last.Compare(a.Last) })
	if err := s.writePage("index", "index", rows); err != nil {
		slog.Warn("reports: can't write index.html", "err", err)
		s.mu.Lock()
		s.indexDirty = true
		s.mu.Unlock()
	}
}

// retry queues updates that failed to write again, ahead of any queued
// since, until they failed maxFails times.
func (s *Store) retry(key string, p *pending, err error) {
	p.fails++
	if p.fails >= maxFails {
		slog.Warn("reports: can't write a report page; dropping its updates", "page", key, "tries", p.fails, "err", err)
		return
	}
	slog.Warn("reports: can't write a report page; trying again", "page", key, "err", err)
	s.mu.Lock()
	defer s.mu.Unlock()
	if later := s.pending[key]; later != nil {
		p.merge(later)
	}
	s.pending[key] = p
	s.signal()
}

// expired: an unmatched title Plex hasn't asked about for unmatchedMaxAge.
func expired(r IndexRow) bool {
	return r.Kind == "unmatched" && time.Since(r.Last) > unmatchedMaxAge
}

// remove deletes a page and its data.
func (s *Store) remove(key string) error {
	for _, path := range []string{s.pagePath(key), s.dataPath(key)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) setIndex(r IndexRow, remove bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if remove {
		delete(s.index, r.Key)
	} else {
		s.index[r.Key] = r
	}
	s.indexDirty = true
}

func (s *Store) writeShow(key string, p *pending) error {
	var doc ShowDoc
	if err := s.readData(key, &doc); err != nil {
		return err
	}
	doc.Key = key
	for _, r := range p.builds {
		b, head := r.snapshot(func() { s.AddBuild(key, r) })
		if i := slices.IndexFunc(doc.Builds, func(x Build) bool { return x.ID == b.ID }); i >= 0 {
			doc.Builds[i] = b // written before; a download came in after
		} else {
			doc.Builds = append(doc.Builds, b)
		}
		if head.Title != "" {
			doc.ShowHead = head
		}
	}
	slices.SortStableFunc(doc.Builds, func(a, b Build) int { return b.Start.Compare(a.Start) })
	doc.Builds = doc.Builds[:min(len(doc.Builds), maxBuilds)]
	for i := 1; i < len(doc.Builds); i++ { // episode lists are long: the newest build's only
		doc.Builds[i].Episodes = nil
	}
	for _, rq := range p.requests {
		if m := rq.Match; m != nil && (doc.LastMatch == nil || !m.Time.Before(doc.LastMatch.Time)) {
			doc.LastMatch = m
		}
	}
	doc.Requests = newestRequests(doc.Requests, p.requests)
	if doc.Title == "" { // no build recorded yet, or none that learned the title
		doc.Title = key
	}
	// Never null in the page's data: the page lists both.
	if doc.Builds == nil {
		doc.Builds = []Build{}
	}
	if doc.Requests == nil {
		doc.Requests = []Request{}
	}
	if err := s.writePage(key, "show", doc); err != nil {
		return err
	}
	s.setIndex(showRow(doc), false)
	return nil
}

func (s *Store) writeUnmatched(key string, p *pending) error {
	if p.drop {
		if err := s.remove(key); err != nil {
			return err
		}
		s.setIndex(IndexRow{Key: key}, true)
		return nil
	}
	var doc UnmatchedDoc
	if err := s.readData(key, &doc); err != nil {
		return err
	}
	doc.Key = key
	if p.unmatched != nil {
		doc.Match = *p.unmatched
		doc.Last = time.Now()
	}
	doc.Requests = newestRequests(doc.Requests, p.requests)
	if len(doc.Requests) > 0 && doc.Requests[0].Time.After(doc.Last) {
		doc.Last = doc.Requests[0].Time
	}
	if doc.Match.Title == "" { // only requests, and the no-match itself was never recorded
		return nil
	}
	if doc.Requests == nil {
		doc.Requests = []Request{} // never null in the page's data
	}
	if err := s.writePage(key, "unmatched", doc); err != nil {
		return err
	}
	s.setIndex(unmatchedRow(doc), false)
	return nil
}

// showRow is a show page's index row.
func showRow(doc ShowDoc) IndexRow {
	row := IndexRow{Key: doc.Key, Kind: "show", Title: doc.Title, Orig: doc.Orig, Builds: len(doc.Builds), Requests: len(doc.Requests)}
	for _, b := range doc.Builds {
		if !slices.Contains(row.Roots, b.Root) {
			row.Roots = append(row.Roots, b.Root)
		}
		if !slices.Contains(row.Modes, b.Mode) {
			row.Modes = append(row.Modes, b.Mode)
		}
	}
	if len(doc.Builds) > 0 {
		b := doc.Builds[0]
		row.Last, row.Complete, row.Failed = b.Start, b.Complete && b.Error == "", b.Error != ""
		for _, c := range b.Checks {
			if c.Level == LevelWarn {
				row.Checks++
			}
		}
	}
	if len(doc.Requests) > 0 && doc.Requests[0].Time.After(row.Last) {
		row.Last = doc.Requests[0].Time
	}
	return row
}

// unmatchedRow is an unmatched title's index row.
func unmatchedRow(doc UnmatchedDoc) IndexRow {
	row := IndexRow{Key: doc.Key, Kind: "unmatched", Title: doc.Match.Title, Year: doc.Match.Year, Last: doc.Last,
		Requests: len(doc.Requests), MinScore: doc.Match.MinScore, Modes: []string{doc.Match.Mode}}
	if len(doc.Match.Candidates) > 0 {
		c := doc.Match.Candidates[0]
		row.Best, row.Score = fmt.Sprintf("anidb-%d %s", c.AID, c.Title), c.Score
	}
	return row
}

// newestRequests puts added (oldest first) before old (newest first) and
// keeps maxRequests. A request like the one before it (Plex asks the same
// thing again and again) only counts up that one, so it doesn't push
// different requests out.
func newestRequests(old, added []Request) []Request {
	out := slices.Clone(old)
	for _, r := range added {
		if len(out) > 0 && sameRequest(out[0], r) {
			n := max(out[0].Count, 1) + 1
			out[0] = r
			out[0].Count = n
			continue
		}
		out = slices.Insert(out, 0, r)
		out = out[:min(len(out), maxRequests)]
	}
	return out
}

// sameRequest: the same request, answered the same way ("memory, 6s" and
// "memory, 9s" are both from memory).
func sameRequest(a, b Request) bool {
	kind := func(answer string) string { k, _, _ := strings.Cut(answer, ","); return k }
	return a.Method == b.Method && a.URL == b.URL && a.Status == b.Status && a.Build == b.Build && kind(a.Answer) == kind(b.Answer)
}

func (s *Store) pagePath(key string) string { return filepath.Join(s.dir, key+".html") }
func (s *Store) dataPath(key string) string { return filepath.Join(s.dir, "data", key+".json") }

// readData loads a page's saved data; a missing file is an empty page. A
// broken one starts over rather than stopping the page for good.
func (s *Store) readData(key string, v any) error {
	data, err := os.ReadFile(s.dataPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		slog.Warn("reports: saved data unreadable; starting the page over", "page", key, "err", err)
	}
	return nil
}

// writePage writes the HTML page, then the data the next update starts
// from: if the page fails, the data is left as it was and the retry
// doesn't add the same updates twice.
func (s *Store) writePage(key, kind string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	html, err := render(page{Kind: kind, Generated: time.Now().UTC(), Version: s.version, Data: raw})
	if err != nil {
		return err
	}
	if err := fetch.WriteFile(s.pagePath(key), html); err != nil { // a browser never opens half a page
		return err
	}
	return fetch.WriteFile(s.dataPath(key), raw)
}
