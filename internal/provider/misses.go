package provider

import (
	"fmt"
	"slices"

	"kizuna/internal/fetch"
)

// miss is one value a show build reads that may not come. A fixed set: the
// ledger is a bitset, and recording a miss allocates nothing. The order of the
// show-level ones is the order names gives (TestShowPrefetchGolden pins it).
type miss uint8

const (
	// Show-level sources, read by fill and fillSeasons: a request failed.
	missTVDBText miss = iota
	missTMDBText
	missTVDBArt
	missTMDBArt
	missTVDBDetails
	missTMDBDetails
	missTVDBSeason
	missTMDBSeason
	missMAL
	missANN
	missPlexShow
	missPlexSeasons
	missPlexImages

	missTVDBEpisodes // TheTVDB's episode list failed (/anidb) or came back partial (/tvdb)
	missTMDBEpisodes // TheMovieDb's episode list failed
	missPlexEpisodes // Plex's episode list failed
	missTMDBLookup   // /tvdb: finding TheMovieDb's show by TheTVDB id failed
	missAniDBDetails // AniDB's details didn't come, and a rebuild soon may bring them (anidbDetails.final)
	missCrewRecords  // some of TheTVDB's crew records are still on their way, or their request failed
	missLaterEntries // /tvdb: later AniDB entries of the series still being fetched in the background
	missOutOfTime    // the build's context ran out: a source may have been skipped without saying so
	missCount
)

// missNames: each miss's name in the log and the match report. Static: no
// allocation.
var missNames = [missCount]string{
	missTVDBText: "tvdb text", missTMDBText: "tmdb text", missTVDBArt: "tvdb art", missTMDBArt: "tmdb art",
	missTVDBDetails: "tvdb details", missTMDBDetails: "tmdb details",
	missTVDBSeason: "tvdb season", missTMDBSeason: "tmdb season",
	missMAL: "mal", missANN: "ann", missPlexShow: "plex", missPlexSeasons: "plex seasons", missPlexImages: "plex images",
	missTVDBEpisodes: "tvdb episodes", missTMDBEpisodes: "tmdb episodes", missPlexEpisodes: "plex episodes",
	missTMDBLookup: "tmdb lookup", missAniDBDetails: "anidb details", missCrewRecords: "crew records",
	missLaterEntries: "later AniDB entries", missOutOfTime: "out of time",
}

// seasonMiss: a TheTVDB or TheMovieDb season's art that failed ("tmdb season
// 2 art").
type seasonMiss struct {
	src string
	n   int
}

// misses is one actor's ledger in one build: what it read that did not come.
// The zero value is an empty, open ledger. One goroutine writes it: each
// concurrent actor of a build has its own, merged into the build's once the
// actor is joined. A nil ledger panics on the first read, miss or not.
type misses struct {
	set     uint32       // one bit per miss
	seasons []seasonMiss // nil until a season art fetch fails
	state   ledgerState
}

type ledgerState uint8

const (
	ledgerOpen   ledgerState = iota
	ledgerAhead              // warm's: records nothing (a value fetched ahead counts only once fill reads it)
	ledgerMerged             // merged into the build's: a later write is a bug, and panics
)

// ahead is the ledger of warm's reads. Shared by every warm goroutine, and
// never written.
var ahead = &misses{state: ledgerAhead}

// open is the gate of every write: false for ahead, a panic for a nil
// ledger or one already merged, so a read that took the wrong ledger fails
// loudly instead of leaving a show complete without its value.
func (m *misses) open() bool {
	switch m.state { // a nil m panics here
	case ledgerAhead:
		return false
	case ledgerMerged:
		panic("provider: a miss recorded after its ledger was merged into the build's")
	}
	return true
}

// add records x: something pending, partial, out of time, or AniDB's details.
func (m *misses) add(x miss) {
	if m.open() {
		m.set |= 1 << x
	}
}

// failed records x when err is a failure: a source that doesn't have the
// value (404) hasn't failed.
func (m *misses) failed(x miss, err error) {
	if m.open() && failure(err) {
		m.set |= 1 << x
	}
}

// seasonFailed is failed for a season art fetch.
func (m *misses) seasonFailed(src string, n int, err error) {
	if s := (seasonMiss{src, n}); m.open() && failure(err) && !slices.Contains(m.seasons, s) {
		m.seasons = append(m.seasons, s)
	}
}

func failure(err error) bool { return err != nil && !fetch.IsNotFound(err) }

func (m *misses) isAhead() bool { return m.state == ledgerAhead }

// has: x is recorded.
func (m *misses) has(x miss) bool { return m.set&(1<<x) != 0 }

// merge adds o's misses to m, and marks o merged: o's actor is done. ahead
// is shared by every build and never done: merging it panics instead of
// marking it merged for all of them.
func (m *misses) merge(o *misses) {
	if o.isAhead() {
		panic("provider: the read-ahead ledger merged into a build's")
	}
	if m.open() && o.open() {
		m.set |= o.set
		m.seasons = append(m.seasons, o.seasons...)
	}
	o.state = ledgerMerged
}

func (m *misses) empty() bool { return m.set == 0 && len(m.seasons) == 0 }

// names lists the misses in table order, then the season art, sorted.
// Nothing to list allocates nothing.
func (m *misses) names() []string {
	var out []string
	for x := range missCount {
		if m.has(x) {
			out = append(out, missNames[x])
		}
	}
	if len(m.seasons) == 0 {
		return out
	}
	seasons := make([]string, len(m.seasons))
	for i, s := range m.seasons {
		seasons[i] = fmt.Sprintf("%s season %d art", s.src, s.n)
	}
	slices.Sort(seasons)
	return append(out, seasons...)
}
