package report

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Recorder collects one build while it runs; every method is safe for
// concurrent use and does nothing on a nil Recorder, so code can record
// unconditionally. Downloads that finish after the build (background
// fetches) are still recorded for lateFor after Finish: one that comes
// after the page was written queues the build again (see Store.AddBuild).
type Recorder struct {
	mu        sync.Mutex
	b         Build
	head      ShowHead
	finished  time.Time                  // when Finish ran; zero while the build runs
	written   bool                       // the store wrote it since its last change
	requeue   func()                     // set by the store: write the build again
	summaries map[summaryKey]*summaryRow // past maxTraceRows: each source and kind's summary row
	failRows  int                        // failed and stale rows kept whole, up to maxFailRows
}

type summaryKey struct {
	src  string
	kind SpanKind
}

// summaryRow counts the rows a summary row stands for; its label is written
// only by snapshot, so a counted row costs no formatting.
type summaryRow struct {
	i, n       int    // its index in the trace, rows counted
	last, note string // the last one's label and note
}

// maxTraceRows caps a build's trace: past it, rows are counted into one
// summary row per source and kind (a long show reads hundreds of episode
// crew files from the cache).
const maxTraceRows = 120

// maxFailRows: failed and stale rows, what the trace is mostly read for,
// stay whole rows past maxTraceRows, up to this many in all; only past it
// are they counted too (a source down for a whole long show).
const maxFailRows = 100

// lateFor is how long after its build a download is still added to it.
const lateFor = 2 * time.Minute

// NewRecorder starts recording a build of root, caused by a request to mode.
// The id is unique: its time is later than that of every id before it, even
// when the clock hasn't moved since (two builds of a root right after each
// other). It holds only [0-9a-zT.-], safe in a URL.
func NewRecorder(root, mode string) *Recorder {
	now := time.Now()
	return &Recorder{b: Build{ID: idTime(now).UTC().Format("20060102T150405.000000") + "-" + root, Root: root, Mode: mode, Start: now}}
}

var lastID struct {
	sync.Mutex
	at time.Time
}

// idTime is now cut to the id's microseconds, or one microsecond past the
// last id's time when now isn't later than that.
func idTime(now time.Time) time.Time {
	lastID.Lock()
	defer lastID.Unlock()
	at := now.Truncate(time.Microsecond)
	if !at.After(lastID.at) {
		at = lastID.at.Add(time.Microsecond)
	}
	lastID.at = at
	return at
}

// ID names the build; requests answered by it point to it.
func (r *Recorder) ID() string {
	if r == nil {
		return ""
	}
	return r.b.ID
}

func (r *Recorder) sinceStart(t time.Time) int64 { return t.Sub(r.b.Start).Milliseconds() }

// Span adds a trace row that ran from start for took.
func (r *Recorder) Span(src, label, note string, kind SpanKind, start time.Time, took time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.finished.IsZero() && time.Since(r.finished) > lateFor {
		r.mu.Unlock()
		return
	}
	r.addSpan(Span{Src: src, Label: label, Note: note, Kind: kind, Start: r.sinceStart(start), End: r.sinceStart(start.Add(took))})
	var requeue func()
	if r.written {
		r.written, requeue = false, r.requeue
	}
	r.mu.Unlock()
	if requeue != nil {
		requeue()
	}
}

// addSpan appends a row, or past maxTraceRows (maxFailRows for a failed or
// stale one) counts it into its source and kind's summary row; caller holds
// r.mu.
func (r *Recorder) addSpan(sp Span) {
	failed := sp.Kind == SpanFail || sp.Kind == SpanStale
	if len(r.b.Trace) < maxTraceRows || sp.Kind == SpanStep || failed && r.failRows < maxFailRows {
		if failed {
			r.failRows++
		}
		r.b.Trace = append(r.b.Trace, sp)
		return
	}
	key := summaryKey{sp.Src, sp.Kind}
	m := r.summaries[key]
	if m == nil {
		if r.summaries == nil {
			r.summaries = map[summaryKey]*summaryRow{}
		}
		m = &summaryRow{i: len(r.b.Trace)}
		r.summaries[key] = m
		r.b.Trace = append(r.b.Trace, Span{Src: sp.Src, Kind: sp.Kind, Start: sp.Start, End: sp.End})
	}
	sum := &r.b.Trace[m.i]
	sum.Start, sum.End = min(sum.Start, sp.Start), max(sum.End, sp.End)
	m.n, m.last, m.note = m.n+1, sp.Label, sp.Note
}

// Step times a step of the build: call the returned func when it ends.
func (r *Recorder) Step(label string) func(note string) {
	if r == nil {
		return func(string) {}
	}
	start := time.Now()
	return func(note string) { r.Span("kizuna", label, note, SpanStep, start, time.Since(start)) }
}

// Field records one show field.
func (r *Recorder) Field(f Field) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.b.Fields = append(r.b.Fields, f)
}

// Check adds something worth a look.
func (r *Recorder) Check(c Check) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.b.Checks = append(r.b.Checks, c)
}

// SetMatch records the match request that started the build.
func (r *Recorder) SetMatch(m *Match) {
	if r == nil || m == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c := *m
	r.b.Match = &c
}

// Mode is the provider URL the build is reported under.
func (r *Recorder) Mode() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.b.Mode
}

// Facts is what the build recorded so far that its checks are derived
// from, as a copy.
type Facts struct {
	Fields []Field
	Match  *Match // the match request that started the build; nil = none did
	Cause  bool   // a recorded check already says why the build is incomplete
}

// Facts returns what was recorded so far. Call it at the end of the build,
// before Finish.
func (r *Recorder) Facts() Facts {
	if r == nil {
		return Facts{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f := Facts{Fields: slices.Clone(r.b.Fields), Cause: slices.ContainsFunc(r.b.Checks, func(c Check) bool { return c.Cause })}
	if r.b.Match != nil {
		m := *r.b.Match
		f.Match = &m
	}
	return f
}

// Result is what a finished build adds to its report. A failed build has
// only Head and Mapping.
type Result struct {
	Head     ShowHead
	Mapping  []MapRow
	Complete bool
	Seasons  []Season
	Episodes []Episode
	EpsTotal int
	Art      []ArtGroup
	Item     json.RawMessage
	Checks   []Check // go after the ones recorded while the build ran
}

// Finish ends the build: took is how long it ran, err its failure.
func (r *Recorder) Finish(took time.Duration, err error, res Result) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.b.TookMS = took.Milliseconds()
	if err != nil {
		r.b.Error = err.Error()
	}
	r.head = res.Head
	r.b.Mapping, r.b.Complete, r.b.Seasons = res.Mapping, res.Complete, res.Seasons
	r.b.Episodes, r.b.EpsTotal, r.b.Art, r.b.Item = res.Episodes, res.EpsTotal, res.Art, res.Item
	r.b.Checks = append(r.b.Checks, res.Checks...)
	r.finished = time.Now()
}

// snapshot copies the build as it stands, rows in time order, and marks it
// written: a later span calls requeue.
func (r *Recorder) snapshot(requeue func()) (Build, ShowHead) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.written, r.requeue = true, requeue
	b := r.b
	b.Trace = slices.Clone(b.Trace)
	for _, m := range r.summaries {
		b.Trace[m.i].Label = fmt.Sprintf("%d more like this", m.n)
		b.Trace[m.i].Note = "last: " + m.last
		if m.note != "" { // a failure's error
			b.Trace[m.i].Note += ": " + m.note
		}
	}
	slices.SortStableFunc(b.Trace, func(x, y Span) int {
		return cmp.Or(cmp.Compare(x.Start, y.Start), strings.Compare(x.Label, y.Label))
	})
	b.Fields = slices.Clone(b.Fields)
	b.Checks = slices.Clone(b.Checks)
	return b, r.head
}

// --- context ---------------------------------------------------------------

type recorderKey struct{}
type matchKey struct{}
type requestKey struct{}

// WithRecorder passes a build's recorder to the code that builds it.
func WithRecorder(ctx context.Context, r *Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

// RecorderFrom returns the build's recorder, nil outside a build.
func RecorderFrom(ctx context.Context) *Recorder {
	r, _ := ctx.Value(recorderKey{}).(*Recorder)
	return r
}

// WithMatch passes a match request to the build it may start.
func WithMatch(ctx context.Context, m *Match) context.Context {
	return context.WithValue(ctx, matchKey{}, m)
}

// MatchFrom returns the match request passed by WithMatch, nil if none.
func MatchFrom(ctx context.Context) *Match {
	m, _ := ctx.Value(matchKey{}).(*Match)
	return m
}

// WithRequest carries the Plex request being answered, for handlers to
// fill in (Key, Build, Answer, Note, Match). Only the request's own
// goroutine reads or writes it: a build it starts (whose ctx still carries
// it) takes its mode from it beforehand, on that goroutine.
func WithRequest(ctx context.Context, rq *Request) context.Context {
	return context.WithValue(ctx, requestKey{}, rq)
}

// RequestFrom returns the request being answered; nil outside a request or
// with the reports off, so callers check before writing.
func RequestFrom(ctx context.Context) *Request {
	rq, _ := ctx.Value(requestKey{}).(*Request)
	return rq
}
