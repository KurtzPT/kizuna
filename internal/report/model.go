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
	"fmt"
	"time"
)

// Limits per page: the newest builds and requests are kept. page.html says
// maxRequests in words ("up to 50 are kept"); TestPageRepeatsLimits keeps
// them in step.
const (
	maxBuilds   = 5
	maxRequests = 50
)

// schemaVersion is the shape of a page's saved data (data/<key>.json). A
// saved page with another one (pages written before 2 have none) counts as
// absent: it isn't listed, and the next write starts the page over. Raise
// it when a change makes old data wrong for the page's script.
const schemaVersion = 3

// page is what one HTML file renders: Kind names the page and the field
// its data goes in (show, unmatched or index), Data is that data as JSON.
type page struct {
	Kind      string          `json:"kind"`
	Generated time.Time       `json:"generated"`
	Version   string          `json:"version"`
	Data      json.RawMessage `json:"-"`
}

// ShowDoc is one show's report: every build of it (by AniDB id, so /anidb
// and /tvdb builds of the same anime share it), newest first.
type ShowDoc struct {
	Schema int    `json:"schema"`
	Key    string `json:"key"` // file name without .html: anidb-1, or tvdb-2 without an AniDB entry
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

	Match    *Match     `json:"match,omitempty"` // the match request that started the build, if one did
	Trace    []Span     `json:"trace"`
	Mapping  []MapRow   `json:"mapping,omitempty"`
	Fields   []Field    `json:"fields,omitempty"`
	Art      []ArtGroup `json:"art,omitempty"` // kept for the newest build only
	Seasons  []Season   `json:"seasons,omitempty"`
	Episodes []Episode  `json:"episodes,omitempty"` // kept for the newest build only
	EpsTotal int        `json:"epsTotal"`
	// Item is the show item sent to Plex, as the JSON Plex got (without its
	// seasons).
	Item json.RawMessage `json:"item,omitempty"`
	// ItemSrc is the source of Item's values, by item key ("title", "Genre",
	// "thumb"; an image by its place in the list: "Image.0"). A key Kizuna
	// knows no single source for (ratingKey, guid, Guid, duration) has none.
	// Builds recorded before it have no ItemSrc at all.
	ItemSrc map[string]string `json:"itemSrc,omitempty"`
	Checks  []Check           `json:"checks,omitempty"`
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
	How        string      `json:"how"` // By in words, shown as it is: "tag [anidb-1]", "guid …", "title search", or why nothing matched
	By         MatchBy     `json:"by"`
	MinScore   int         `json:"min"`
	Root       string      `json:"root,omitempty"` // the show picked; "" = no match
	Score      int         `json:"score,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
	// Query is the title without its id tags, when it has any: what the
	// title search looks for, and what a new tag goes after.
	Query string `json:"query,omitempty"`
}

// MatchBy is what decided a match, as facts.
type MatchBy struct {
	ID      MatchID      `json:"id,omitempty"`      // "" = no id decided
	Text    string       `json:"text,omitempty"`    // the tag, the GUID, or every tag
	Off     string       `json:"off,omitempty"`     // MatchTagOff: the mode the tag needs
	Need    string       `json:"need,omitempty"`    // MatchTagOff: the setting that turns it on
	ByName  *ByName      `json:"byName,omitempty"`  // picked by name inside a TheTVDB id
	Search  SearchResult `json:"search,omitempty"`  // "" = the title search didn't run
	Best    int          `json:"best,omitempty"`    // the best title result's score
	Missing string       `json:"missing,omitempty"` // the show has no such "season 3" / "episode S01E02"
}

// MatchID is what an id tag or GUID said.
type MatchID string

const (
	MatchTag      MatchID = "tag"  // an id tag named the show
	MatchGUID     MatchID = "guid" // the GUID Plex already had named the show
	MatchManyTags MatchID = "tags" // more than one id tag: no match
	MatchTagOff   MatchID = "off"  // an /auto id tag whose mode is off: no match
)

// SearchResult is what the title search said.
type SearchResult string

const (
	SearchFound SearchResult = "found" // at least one candidate
	SearchNone  SearchResult = "none"  // no similar title
	SearchLow   SearchResult = "low"   // the best result is below MIN_SCORE
)

// TagRule: an id tag rule said no match (two different tags, or a tag
// whose provider is off) and the title search never ran (in a Fix Match
// list it still does).
func (by MatchBy) TagRule() bool {
	return (by.ID == MatchManyTags || by.ID == MatchTagOff) && by.Search == ""
}

// TitleSearch: the title search alone found the show Plex got: no id, and
// the show had the season or episode asked for.
func (by MatchBy) TitleSearch() bool {
	return by.ID == "" && by.Search == SearchFound && by.Missing == ""
}

// ByName is an AniDB entry picked by name inside a TheTVDB id, and the id
// tags that name it exactly.
type ByName struct {
	AID  int    `json:"aid"`
	Sure string `json:"sure"`
}

// Note says which entry was picked by name and which tag makes it sure.
func (n ByName) Note() string {
	return fmt.Sprintf("AniDB %d, may be wrong; use %s to be sure", n.AID, n.Sure)
}

// Candidate is one title search result.
type Candidate struct {
	AID   int    `json:"aid"`
	Title string `json:"title"`
	Score int    `json:"score"`
}

// Span is one row of a build's trace, in ms from the build's start.
type Span struct {
	Src   string   `json:"src"` // anidb, tvdb, tmdb, mal, plex, ann, map, kizuna, other
	Label string   `json:"label"`
	Note  string   `json:"note,omitempty"`
	Kind  SpanKind `json:"kind"`
	Start int64    `json:"start"`
	End   int64    `json:"end"`
}

// SpanKind is how a trace row's data came. All but SpanStep are
// fetch.Kind's values, converted as they are (TestSpanKindsAreFetchKinds).
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

// ArtGroup is the images of one kind that the show's or a season's item
// sent to Plex, in the order sent. A level without images of a kind has no
// group for it.
type ArtGroup struct {
	Level  string     `json:"level"`            // "show", or the season's name ("Season 2", "Specials")
	Season *int       `json:"season,omitempty"` // the season's number; nil for the show
	Kind   string     `json:"kind"`             // poster, background, logo
	Images []ArtImage `json:"images"`
}

// ArtImage is one image sent to Plex.
type ArtImage struct {
	URL     string `json:"url"`
	Src     string `json:"src,omitempty"`     // the source that gave it
	Primary bool   `json:"primary,omitempty"` // the item's thumb (a poster) or art (a background)
}

// Season is one season of the built show: its episode count and what its
// item sent to Plex besides its images (Build.Art). A season built without
// art of its own has the show's poster as Thumb and no groups in Build.Art.
type Season struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Index int    `json:"index"`
	Thumb string `json:"thumb,omitempty"`
	Art   string `json:"art,omitempty"`
	Date  string `json:"date,omitempty"`
}

// Episode is one episode as sent to Plex.
type Episode struct {
	Season    int               `json:"s"`
	Index     int               `json:"e"`
	Title     string            `json:"title"`
	From      string            `json:"from,omitempty"`   // source of the title
	Titles    map[string]string `json:"titles,omitempty"` // every source's title
	Pairs     map[string]string `json:"pairs,omitempty"`  // source -> where the episode is there ("S01E05", "15207 ep 7")
	Summary   string            `json:"summary,omitempty"`
	Date      string            `json:"date,omitempty"`
	Thumb     string            `json:"thumb,omitempty"`
	Minutes   int               `json:"minutes,omitempty"`
	Directors []string          `json:"directors,omitempty"`
	Writers   []string          `json:"writers,omitempty"`
	Guids     []string          `json:"guids,omitempty"`
	// Src: the source of summary, date, thumb, minutes, directors and
	// writers (anidb, tvdb, tmdb, plex, or show: a movie's episode has the show's
	// summary), for those the episode has.
	Src   map[string]string `json:"src,omitempty"`
	Issue string            `json:"issue,omitempty"`
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
	Build  string    `json:"build,omitempty"` // build that answered
	Answer Answer    `json:"answer,omitzero"`
	Note   string    `json:"note,omitempty"`
	Status int       `json:"status"`
	Bytes  int       `json:"bytes"`
	TookMS float64   `json:"tookMs"`
	Count  int       `json:"count,omitempty"` // the same request this many times in a row (0 = once); Time is the last
	Match  *Match    `json:"-"`               // the match it answered, for ShowDoc.LastMatch
}

// Answer is how a request was answered. The page words it ("memory, 6s").
type Answer struct {
	Kind  AnswerKind `json:"kind"`
	Age   int64      `json:"age,omitempty"`   // seconds: how old the memory copy or the failure is
	After int64      `json:"after,omitempty"` // seconds: a failed build is tried again after this
}

// AnswerKind is what answered a request.
type AnswerKind string

const (
	AnswerNewBuild    AnswerKind = "new"         // the request started a build
	AnswerJoined      AnswerKind = "joined"      // it waited for a build already running
	AnswerMemory      AnswerKind = "memory"      // the show in memory, built Age ago
	AnswerFailed      AnswerKind = "failed"      // a build that failed Age ago; built again After that
	AnswerTimedOut    AnswerKind = "timeout"     // the request gave up before its build ended
	AnswerUnavailable AnswerKind = "unavailable" // the build it waited for failed
	AnswerNoMatch     AnswerKind = "nomatch"
	AnswerFixMatch    AnswerKind = "list" // a Fix Match list
)

// UnmatchedDoc is a title Plex asked about that matched nothing.
type UnmatchedDoc struct {
	Schema   int       `json:"schema"`
	Key      string    `json:"key"`
	Match    Match     `json:"match"` // the newest no-match answer
	Last     time.Time `json:"last"`  // last asked; the page goes after unmatchedMaxAge without a request
	Requests []Request `json:"requests"`
}

// IndexRow is one line of index.html.
type IndexRow struct {
	Schema   int       `json:"schema"` // its page's (schemaVersion)
	Key      string    `json:"key"`
	Kind     string    `json:"kind"` // show | unmatched
	Title    string    `json:"title"`
	Orig     string    `json:"orig,omitempty"`
	Roots    []string  `json:"roots,omitempty"`
	Modes    []string  `json:"modes,omitempty"`
	Last     time.Time `json:"last"`
	Complete bool      `json:"complete"`
	Failed   bool      `json:"failed,omitempty"` // the newest build failed
	Checks   int       `json:"checks"`
	Builds   int       `json:"builds"`
	Requests int       `json:"requests"`
	// unmatched only
	Year     int    `json:"year,omitempty"`
	Best     string `json:"best,omitempty"`
	Score    int    `json:"score,omitempty"`
	MinScore int    `json:"min,omitempty"`
}
