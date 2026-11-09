// Package report writes the match reports: one HTML page per show in
// <DATA_DIR>/reports, readable without the logs. A page shows what Plex
// asked, how the show was matched, what every source offered for every
// field and which one was used, the episode list sent to Plex, a trace of
// every download, and the Plex requests that followed. Titles Plex asked
// about that matched nothing get a page of their own, and index.html lists
// everything.
//
// Each page is self-contained (data inlined as JSON, rendered by the page's
// own script), so it opens straight from the folder in any browser.
package report

import (
	"encoding/json"
	"time"
)

// Limits per page: the newest builds and requests are kept.
const (
	MaxBuilds   = 5
	MaxRequests = 50
)

// Page is what one HTML file renders: Kind names the page and the field
// its data goes in (show, unmatched or index), Data is that data as JSON.
type Page struct {
	Kind      string          `json:"kind"`
	Generated time.Time       `json:"generated"`
	Version   string          `json:"version"`
	Data      json.RawMessage `json:"-"`
}

// ShowDoc is one show's report: every build of it (by AniDB id, so /anidb
// and /tvdb builds of the same anime share it), newest first.
type ShowDoc struct {
	Key string `json:"key"` // file name without .html: anidb-1, or tvdb-2 without an AniDB entry
	ShowHead
	Builds   []Build   `json:"builds"`
	Requests []Request `json:"requests"`
	// LastMatch is the newest match request for the show, kept apart from
	// the builds: on a long scan they are often started by later metadata
	// requests, and the match would scroll out with its build.
	LastMatch *Match `json:"lastMatch,omitempty"`
}

// ShowHead names the show: each build sets it, the newest one wins.
type ShowHead struct {
	Title string `json:"title"`
	Orig  string `json:"orig,omitempty"` // original title
	IDs   []ID   `json:"ids,omitempty"`
}

// ID is the show's id on one site (anidb, tvdb, tmdb, tmdbmovie, mal, anilist, imdb, ann, kitsu).
type ID struct {
	Site  string `json:"site"`
	Value string `json:"value"`
}

// Build is one build of a show model.
type Build struct {
	ID       string    `json:"id"`
	Root     string    `json:"root"` // anidb-1 / tvdb-2
	Mode     string    `json:"mode"` // provider URL that caused it: anidb, tvdb, auto
	Start    time.Time `json:"start"`
	TookMS   int64     `json:"tookMs"`
	Complete bool      `json:"complete"`
	Error    string    `json:"error,omitempty"`

	Match    *Match      `json:"match,omitempty"` // the match request that started the build, if one did
	Trace    []Span      `json:"trace"`
	Mapping  []MapRow    `json:"mapping,omitempty"`
	Fields   []Field     `json:"fields,omitempty"`
	Seasons  []Season    `json:"seasons,omitempty"`
	Episodes []Episode   `json:"episodes,omitempty"` // kept for the newest build only
	EpsTotal int         `json:"epsTotal"`
	Output   [][2]string `json:"output,omitempty"`
	Checks   []Check     `json:"checks,omitempty"`
}

// Match is what Plex sent to .../matches and how Kizuna decided.
type Match struct {
	Time       time.Time   `json:"time"`
	Mode       string      `json:"mode"`
	Type       string      `json:"type"` // show, season, episode
	Title      string      `json:"title"`
	Year       int         `json:"year,omitempty"`
	File       string      `json:"file,omitempty"`
	GUID       string      `json:"guid,omitempty"`
	Manual     bool        `json:"manual,omitempty"`
	How        string      `json:"how"` // what decided: "tag [anidb-1]", "guid …", "title search", or why nothing matched
	MinScore   int         `json:"min"`
	Root       string      `json:"root,omitempty"` // the show picked; "" = no match
	Score      int         `json:"score,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
}

// Candidate is one title search result.
type Candidate struct {
	AID   int    `json:"aid"`
	Title string `json:"title"`
	Score int    `json:"score"`
}

// Span is one row of a build's trace, in ms from the build's start.
type Span struct {
	Src   string   `json:"src"` // anidb, tvdb, tmdb, mal, ann, map, kizuna
	Label string   `json:"label"`
	Note  string   `json:"note,omitempty"`
	Kind  SpanKind `json:"kind"`
	Start int64    `json:"start"`
	End   int64    `json:"end"`
}

// SpanKind is how a trace row's data came. All but SpanStep are
// fetch.Kind's values, converted as they are.
type SpanKind string

const (
	SpanNet    SpanKind = "net"    // downloaded
	SpanCache  SpanKind = "cache"  // read from the cache
	SpanStale  SpanKind = "stale"  // the download failed or was not allowed: an old copy was used
	SpanFail   SpanKind = "fail"   // failed
	SpanMemory SpanKind = "memory" // the source's copy in memory
	SpanStep   SpanKind = "step"   // a step of the build itself
)

// MapRow is one AniDB entry of the show in the mapping lists.
type MapRow struct {
	AID        int    `json:"aid"`
	Name       string `json:"name"`
	TVDB       int    `json:"tvdb,omitempty"`
	TVDBSeason string `json:"tvdbSeason,omitempty"`
	TVDBOffset int    `json:"tvdbOffset,omitempty"`
	TMDB       int    `json:"tmdb,omitempty"`
	TMDBSeason string `json:"tmdbSeason,omitempty"`
	Exceptions int    `json:"exceptions,omitempty"` // per-episode mappings
}

// Field is one show field: the sources in the configured order, what each
// one asked offered, and the one(s) used.
type Field struct {
	Name    string            `json:"name"`
	Order   []string          `json:"order"`
	Asked   []string          `json:"asked"`
	Offered map[string]string `json:"offered"`
	Used    []string          `json:"used"`
}

// Season is one season of the built show and its episode count.
type Season struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Episode is one episode as sent to Plex.
type Episode struct {
	Season int               `json:"s"`
	Index  int               `json:"e"`
	Title  string            `json:"title"`
	From   string            `json:"from,omitempty"`   // source of the title
	Titles map[string]string `json:"titles,omitempty"` // every source's title
	Pairs  map[string]string `json:"pairs,omitempty"`  // source -> where the episode is there ("S01E05", "15207 · 7")
	Date   string            `json:"date,omitempty"`
	Issue  string            `json:"issue,omitempty"`
}

// Check is something in a build worth a look.
type Check struct {
	Level   Level   `json:"level"`
	Title   string  `json:"title"`
	Text    string  `json:"text,omitempty"`
	Section Section `json:"section,omitempty"` // page section to jump to
	// Cause: it says why the build is incomplete, so the generic
	// "built with something missing" check isn't added for the same thing.
	Cause bool `json:"-"`
}

// Level is how much a check matters: a warning is counted on the index.
type Level string

const (
	LevelWarn Level = "warn"
	LevelInfo Level = "info"
)

// Section is a part of the show page a check links to.
type Section string

const (
	SectionTrace    Section = "trace"
	SectionMatch    Section = "match"
	SectionFields   Section = "fields"
	SectionEpisodes Section = "episodes"
)

// Request is one Plex request about the show (or the unmatched title).
type Request struct {
	Key    string    `json:"-"` // report it belongs to; "" = none
	Time   time.Time `json:"time"`
	Mode   string    `json:"mode"`
	Method string    `json:"method"`
	URL    string    `json:"url"`
	Build  string    `json:"build,omitempty"`  // build that answered
	Answer string    `json:"answer,omitempty"` // new build, joined build, memory 6s, …
	Note   string    `json:"note,omitempty"`
	Status int       `json:"status"`
	Bytes  int       `json:"bytes"`
	TookMS float64   `json:"tookMs"`
	Count  int       `json:"count,omitempty"` // the same request this many times in a row (0 = once); Time is the last
	Match  *Match    `json:"-"`               // the match it answered, for ShowDoc.LastMatch
}

// UnmatchedDoc is a title Plex asked about that matched nothing.
type UnmatchedDoc struct {
	Key      string    `json:"key"`
	Match    Match     `json:"match"` // the newest no-match answer
	Last     time.Time `json:"last"`  // last asked; the page goes after unmatchedMaxAge without a request
	Requests []Request `json:"requests"`
}

// IndexRow is one line of index.html.
type IndexRow struct {
	Key      string    `json:"key"`
	Kind     string    `json:"kind"` // show | unmatched
	Title    string    `json:"title"`
	Orig     string    `json:"orig,omitempty"`
	Roots    []string  `json:"roots,omitempty"`
	Modes    []string  `json:"modes,omitempty"`
	Last     time.Time `json:"last"`
	Complete bool      `json:"complete"`
	Checks   int       `json:"checks"`
	Builds   int       `json:"builds"`
	Requests int       `json:"requests"`
	// unmatched only
	Year     int    `json:"year,omitempty"`
	Best     string `json:"best,omitempty"`
	Score    int    `json:"score,omitempty"`
	MinScore int    `json:"min,omitempty"`
}
