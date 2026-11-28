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
// things at once, so a page is written flushEvery after a build or a
// no-match answer for it, requestsEvery after a request when it has only
// those, and the index (which lists every page) at most every indexEvery.
//
// All methods do nothing on a nil Store.
type Store struct {
	dir     string
	version string
	now     func() time.Time // when updates are due; the tests set it
	timer   storeTimerFunc   // Run's timer; nil = time.NewTimer. Tests fire it themselves

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
	// requestsEvery is how long a page waits when only Plex requests were
	// added to it. A long show's page is about a megabyte, and Plex pages
	// through the show with a request every few hundred milliseconds: at
	// flushEvery that page would be written again and again.
	requestsEvery = 10 * time.Second
	// unmatchedMaxAge: an unmatched title's page goes once Plex hasn't asked
	// about the title for this long (renamed, matched another way, or gone
	// from the library). page.html says it in words ("after 30 days");
	// TestPageRepeatsLimits keeps them in step.
	unmatchedMaxAge = 30 * 24 * time.Hour
	// showMaxAge: a show's page goes this long after the show's last build
	// or request, and comes back at the show's next refresh. Longer than an
	// unmatched title's: Plex may leave a finished show alone for more than
	// a month, and its page and builds should outlast that. page.html says
	// it too ("after 90 days").
	showMaxAge = 90 * 24 * time.Hour
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
	unmatched *Match    // a new no-match answer for the title
	drop      bool      // the title matched after all: remove its page
	fails     int       // failed writes of these updates
	due       time.Time // when they are to be written
}

// merge adds the updates queued after p, which failed to write.
func (p *pending) merge(later *pending) {
	if later.drop {
		*p = pending{drop: true, fails: p.fails, due: p.due}
		return
	}
	p.builds = append(p.builds, later.builds...)
	p.requests = append(p.requests, later.requests...)
	if later.due.Before(p.due) {
		p.due = later.due
	}
	if later.unmatched != nil {
		p.unmatched, p.drop = later.unmatched, false
	}
}

// Open uses dir (created if needed) and reads the existing index. Pages
// older than their limit (expired) are removed.
func Open(dir, version string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, version: version, now: time.Now, pending: map[string]*pending{}, index: map[string]IndexRow{}, wake: make(chan struct{}, 1)}
	var rows []IndexRow
	if data, err := os.ReadFile(s.dataPath("index")); err == nil {
		if err := json.Unmarshal(data, &rows); err != nil {
			slog.Warn("reports: index unreadable; starting a new one", "err", err)
		}
	}
	known := map[string]bool{"index": true}
	for _, r := range rows {
		if r.Schema != schemaVersion { // its page's data says the same below
			s.indexDirty = true
			continue
		}
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
	ignored := 0
	for _, f := range files {
		key, ok := strings.CutSuffix(f.Name(), ".json")
		if !ok || known[key] || f.IsDir() {
			continue
		}
		r, ok, old := s.rowFromData(key)
		if ok {
			s.openRow(r, true)
		} else if old {
			ignored++
		}
	}
	if ignored > 0 {
		slog.Info("reports: pages saved by another version are not listed; each starts over when it is next written", "pages", ignored)
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
			slog.Warn("reports: can't remove an old page", "page", r.Key, "err", err)
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

// rowFromData rebuilds the index row of a page from its saved data; ok
// false if the data is unreadable, not a page, or (old) of another schema
// version.
func (s *Store) rowFromData(key string) (row IndexRow, ok, old bool) {
	data, err := os.ReadFile(s.dataPath(key))
	if err != nil {
		slog.Warn("reports: can't read a page's saved data", "page", key, "err", err)
		return IndexRow{}, false, false
	}
	if strings.HasPrefix(key, "unmatched-") {
		var doc UnmatchedDoc
		if old, err := decode(data, &doc); old || err != nil || doc.Match.Title == "" {
			return IndexRow{}, false, old
		}
		doc.Key = key
		return unmatchedRow(doc), true, false
	}
	var doc showFile
	if old, err := decode(data, &doc); old || err != nil {
		return IndexRow{}, false, old
	}
	doc.Key = key
	return showRow(doc), true, false
}

// saved is a page's saved data: current says whether this version saved
// it, clear empties it.
type saved interface {
	current() bool
	clear()
}

func (d *showFile) current() bool     { return d.Schema == schemaVersion }
func (d *showFile) clear()            { *d = showFile{} }
func (d *UnmatchedDoc) current() bool { return d.Schema == schemaVersion }
func (d *UnmatchedDoc) clear()        { *d = UnmatchedDoc{} }

// decode reads a page's saved data into doc. old: another version saved
// it, which is no error, whatever else is in it (pages from before
// schemaVersion 2 hold values of another type). Data that isn't JSON at all
// is an error, not old.
func decode(data []byte, doc saved) (old bool, err error) {
	err = json.Unmarshal(data, doc)
	var syntax *json.SyntaxError
	if !doc.current() && !errors.As(err, &syntax) {
		return true, nil
	}
	return false, err
}

// Run writes pending updates as they fall due until ctx ends, then
// everything once more.
func (s *Store) Run(ctx context.Context) {
	if s == nil {
		return
	}
	for s.wait(ctx) {
		s.flush(false, s.now())
	}
	s.Flush()
}

// wait returns true when a page or the index is due to be written, false
// when ctx ends. A new update may be due sooner than what it waits for, so
// each one makes it look again.
func (s *Store) wait(ctx context.Context) bool {
	for {
		s.mu.Lock()
		next, ok := s.nextDue()
		s.mu.Unlock()
		var due <-chan time.Time // nil: nothing to wait for but a new update
		var timer storeTimer
		if ok {
			due, timer = s.newTimer(next.Sub(s.now()))
		}
		select {
		case <-ctx.Done():
			return false
		case <-due:
			return true
		case <-s.wake:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// storeTimer is Run's timer: a *time.Timer, or a test's.
type storeTimer interface{ Stop() bool }

// storeTimerFunc starts a timer that fires on the channel after d, as
// time.NewTimer.
type storeTimerFunc func(d time.Duration) (<-chan time.Time, storeTimer)

func (s *Store) newTimer(d time.Duration) (<-chan time.Time, storeTimer) {
	if s.timer != nil {
		return s.timer(d)
	}
	t := time.NewTimer(d)
	return t.C, t
}

// nextDue is when the next page, or the index, is due to be written; ok
// false when nothing is. Caller holds s.mu.
func (s *Store) nextDue() (next time.Time, ok bool) {
	if s.indexDirty {
		next, ok = s.indexAt.Add(indexEvery), true
	}
	for _, p := range s.pending {
		if !ok || p.due.Before(next) {
			next, ok = p.due, true
		}
	}
	return next, ok
}

// touch returns key's pending updates, due wait from now at the latest.
func (s *Store) touch(key string, wait time.Duration) *pending {
	due := s.now().Add(wait)
	p := s.pending[key]
	if p == nil {
		p = &pending{due: due}
		s.pending[key] = p
	} else if due.Before(p.due) {
		p.due = due
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
	p := s.touch(key, flushEvery)
	p.builds = append(p.builds, r)
}

// AddRequest queues a Plex request for the page rq.Key. It waits up to
// requestsEvery, or goes with the page's next build if that comes first.
func (s *Store) AddRequest(rq Request) {
	if s == nil || rq.Key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.touch(rq.Key, requestsEvery)
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
	p := s.touch(key, flushEvery)
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
	if !s.hasPage(key) {
		return
	}
	p := s.touch(key, flushEvery)
	*p = pending{drop: true, due: p.due}
}

// HasPage reports whether key has a page, written or about to be.
func (s *Store) HasPage(key string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hasPage(key)
}

// hasPage: a page being written isn't in the index yet; caller holds s.mu.
func (s *Store) hasPage(key string) bool {
	_, listed := s.index[key]
	return listed || s.pending[key] != nil || s.writing[key] != nil
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
func (s *Store) Flush() {
	if s == nil {
		return
	}
	s.flush(true, s.now())
}

// flush writes the pending updates due by now, or all of them if all is
// set, and the index if it changed and all is set or it was last written
// indexEvery before now.
func (s *Store) flush(all bool, now time.Time) {
	if s == nil {
		return
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	work := s.pending
	if !all {
		work = map[string]*pending{}
		for key, p := range s.pending {
			if !p.due.After(now) {
				work[key] = p
				delete(s.pending, key)
			}
		}
	} else {
		s.pending = map[string]*pending{}
	}
	s.writing = work
	s.mu.Unlock()

	keys := make([]string, 0, len(work))
	for k := range work {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if err := s.write(key, work[key]); err != nil {
			s.retry(key, work[key], err)
		}
	}

	s.mu.Lock()
	s.writing = nil
	due := s.indexDirty && (all || now.Sub(s.indexAt) >= indexEvery)
	rows := []IndexRow{}
	var old []string
	if due {
		s.indexDirty, s.indexAt = false, now
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
			slog.Warn("reports: can't remove an old page", "page", key, "err", err)
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

// write writes one page's updates. A panic there is logged and drops them:
// Run writes every show's page, so one show's panic must not end Kizuna,
// and written again they would panic again.
func (s *Store) write(key string, p *pending) (err error) {
	defer func() {
		if r := recover(); r != nil {
			fetch.LogPanic("reports: writing a report page panicked; dropping its updates", r, "page", key)
			err = nil
		}
	}()
	if strings.HasPrefix(key, "unmatched-") {
		return s.writeUnmatched(key, p)
	}
	return s.writeShow(key, p)
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
	p.due = s.now().Add(flushEvery)
	if later := s.pending[key]; later != nil {
		p.merge(later)
	}
	s.pending[key] = p
	s.signal()
}

// expired: a title Plex hasn't asked about for unmatchedMaxAge, or a show
// for showMaxAge. A show's row counts from its newest build or request
// (showRow).
func expired(r IndexRow) bool {
	if r.Kind == "unmatched" {
		return time.Since(r.Last) > unmatchedMaxAge
	}
	return time.Since(r.Last) > showMaxAge
}

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
	var doc showFile
	if err := s.readData(key, &doc); err != nil {
		return err
	}
	doc.Schema, doc.Key = schemaVersion, key
	var head ShowHead
	var headAt time.Time
	for _, r := range p.builds {
		b, h := r.snapshot(func() { s.AddBuild(key, r) })
		sb, err := newSavedBuild(b)
		if err != nil {
			return err
		}
		if i := slices.IndexFunc(doc.Builds, func(x savedBuild) bool { return x.ID == b.ID }); i >= 0 {
			doc.Builds[i] = sb // written before; a download came in after
		} else {
			doc.Builds = append(doc.Builds, sb)
		}
		if h.Title != "" && (head.Title == "" || !b.Start.Before(headAt)) {
			head, headAt = h, b.Start
		}
	}
	slices.SortStableFunc(doc.Builds, func(a, b savedBuild) int { return b.Start.Compare(a.Start) })
	// The newest build names the show: a late download on an older build must not overwrite it.
	noTitle := doc.Title == "" || (doc.Title == key && doc.Orig == "" && len(doc.IDs) == 0)
	if head.Title != "" && (noTitle || !headAt.Before(doc.Builds[0].Start)) {
		doc.ShowHead = head
	}
	doc.Builds = doc.Builds[:min(len(doc.Builds), maxBuilds)]
	for i := 1; i < len(doc.Builds); i++ { // episode and image lists are long: the newest build's only
		if err := doc.Builds[i].lean(); err != nil {
			return err
		}
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
		doc.Builds = []savedBuild{}
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
	doc.Schema, doc.Key = schemaVersion, key
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

func showRow(doc showFile) IndexRow {
	row := IndexRow{Schema: doc.Schema, Key: doc.Key, Kind: "show", Title: doc.Title, Orig: doc.Orig, Builds: len(doc.Builds), Requests: len(doc.Requests)}
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

func unmatchedRow(doc UnmatchedDoc) IndexRow {
	row := IndexRow{Schema: doc.Schema, Key: doc.Key, Kind: "unmatched", Title: doc.Match.Title, Year: doc.Match.Year, Last: doc.Last,
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

// sameRequest: the same request, answered the same way (from memory 6 s
// and 9 s old are both from memory).
func sameRequest(a, b Request) bool {
	return a.Method == b.Method && a.URL == b.URL && a.Status == b.Status && a.Build == b.Build && a.Answer.Kind == b.Answer.Kind
}

func (s *Store) pagePath(key string) string { return filepath.Join(s.dir, key+".html") }
func (s *Store) dataPath(key string) string { return filepath.Join(s.dir, "data", key+".json") }

// readData loads a page's saved data into doc, which is empty; a missing
// file leaves it so, and so does data another version saved (see decode):
// the page starts over. A broken one starts over too, rather than stopping
// the page for good.
func (s *Store) readData(key string, doc saved) error {
	data, err := os.ReadFile(s.dataPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	old, err := decode(data, doc)
	if err != nil {
		slog.Warn("reports: saved data unreadable; starting the page over", "page", key, "err", err)
	}
	if old {
		doc.clear()
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
