// Package mapping links AniDB ids to the ids of other sites (TVDB, TMDB,
// IMDb, MyAnimeList, AniList, Kitsu, Anime News Network). Two lists are merged, in the order of
// MAPPING_SOURCES; a later list only fills ids the earlier ones lack:
//
//	animelists  Anime-Lists anime-list-master.xml: TVDB/TMDB/IMDb ids plus
//	            TVDB and TMDB season, episode offset and per-episode exceptions
//	fribb       Fribb/anime-lists anime-list-mini.json: Anime-Lists merged with
//	            anime-offline-database (MAL, AniList, Kitsu, ANN ids) and ids looked
//	            up on TMDB (many extra IMDb/TMDB ids)
//
// An optional anime-list-custom.xml in the data folder (Anime-Lists format)
// replaces whole entries and always wins, also while no list has loaded yet.
package mapping

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"kizuna/internal/fetch"
)

// Lists holds the merged mapping in memory.
type Lists struct {
	mu    sync.RWMutex
	byAID map[int]Entry
	byID  map[string]map[string][]int // source -> id -> aids (sorted)
	sum   [32]byte                    // of everything the mapping was loaded from (zero after SetForTest)
	// customOnly: no list has loaded yet; byAID holds anime-list-custom.xml alone.
	customOnly bool
}

// listSources: how each MAPPING_SOURCES list is downloaded and parsed.
var listSources = map[string]struct {
	get   func(context.Context, *fetch.Client, func([]byte) error) ([]byte, error)
	parse func([]byte) (map[int]Entry, error)
}{
	"animelists": {getAnimeLists, parseAnimeLists},
	"fribb":      {getFribb, parseFribb},
}

// checked parses a download in full before fetch caches it, so a broken
// download (a stray "&", a cut-off file) is rejected and the good cached
// copy stays. It keeps the entries, so Load doesn't parse the same bytes
// twice. Validate can still run after Load gave up (fetch doesn't cancel
// downloads), hence the lock.
type checked struct {
	parse   func([]byte) (map[int]Entry, error)
	mu      sync.Mutex
	data    []byte
	entries map[int]Entry
}

func (c *checked) validate(b []byte) error {
	list, err := c.parse(b)
	if err == nil && len(list) == 0 {
		err = errors.New("no entries")
	}
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.data, c.entries = b, list
	c.mu.Unlock()
	return nil
}

// entriesOf returns what validate parsed when data is those very bytes (not a
// cached copy fetch answered with instead); nil otherwise.
func (c *checked) entriesOf(data []byte) map[int]Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(data) == 0 || len(c.data) != len(data) || &c.data[0] != &data[0] {
		return nil
	}
	return c.entries
}

// Load downloads (daily) the lists named in order and merges them.
// On the first load, a list that fails is skipped as long as another one
// loads; once the mapping is Ready, any failed list keeps the loaded mapping
// (the lists fill each other's gaps, so one alone would lose ids and
// episode exceptions). When nothing changed since the last load (most days:
// the lists answer 304 and the custom file is the same), the loaded mapping
// is kept as is.
func (l *Lists) Load(ctx context.Context, f *fetch.Client, dataDir string, order []string) error {
	type downloaded struct {
		src  string
		data []byte
		list map[int]Entry // already parsed by checked; nil = parse data
	}
	var got []downloaded
	var errs []error
	h := sha256.New()
	for _, src := range order {
		ls, ok := listSources[src]
		if !ok {
			continue
		}
		c := &checked{parse: ls.parse}
		data, err := ls.get(ctx, f, c.validate)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src, err))
			continue
		}
		got = append(got, downloaded{src, data, c.entriesOf(data)})
		fmt.Fprintf(h, "%s:%d:", src, len(data))
		h.Write(data)
	}
	custom, customErr := os.ReadFile(filepath.Join(dataDir, "anime-list-custom.xml"))
	if customErr == nil {
		fmt.Fprintf(h, "custom:%d:", len(custom))
		h.Write(custom)
	}
	var sum [32]byte
	h.Sum(sum[:0])
	if len(got) > 0 && l.loadedFrom(sum) {
		slog.Debug("mapping unchanged; keeping the loaded one", "lists", len(got))
		return errors.Join(errs...)
	}

	entries := map[int]Entry{}
	for _, g := range got {
		list := g.list
		if list == nil {
			var err error
			if list, err = listSources[g.src].parse(g.data); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", g.src, err))
				continue
			}
		}
		added, filled := merge(entries, list)
		slog.Info("mapping loaded", "list", g.src, "entries", len(list), "new", added, "filled", filled)
	}
	listsLoaded := len(entries) > 0
	if !listsLoaded {
		errs = append(errs, errors.New("no mapping list loaded"))
	}
	if len(errs) > 0 && l.Ready() { // keep the full mapping of an earlier load (custom entries included)
		return errors.Join(append(errs, errors.New("keeping the mapping loaded earlier"))...)
	}
	if customErr == nil {
		// A broken custom file is reported but never costs the downloaded lists.
		if extra, err := parseAnimeLists(custom); err != nil {
			errs = append(errs, fmt.Errorf("anime-list-custom.xml ignored (fix it and restart, or wait for the daily reload): %w", err))
		} else {
			for aid, e := range extra {
				entries[aid] = e
			}
			slog.Info("mapping: custom entries applied", "entries", len(extra))
		}
	}
	if len(entries) == 0 {
		return errors.Join(errs...)
	}
	// Custom entries alone are used until a list loads, but don't make the
	// mapping Ready: /health stays degraded and the lists are retried soon.
	l.set(entries, !listsLoaded)
	if len(errs) == 0 { // after a problem, load in full again: its error is logged until it's gone
		l.mu.Lock()
		l.sum = sum
		l.mu.Unlock()
	}
	return errors.Join(errs...) // non-nil only to log a list that failed
}

// loadedFrom reports whether the current mapping was loaded from inputs
// with this sum.
func (l *Lists) loadedFrom(sum [32]byte) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.byAID != nil && l.sum == sum
}

// merge adds src's entries to dst: unknown AniDB ids are added, known ones
// only get the ids they lack. Returns how many were added / filled.
func merge(dst, src map[int]Entry) (added, filled int) {
	for aid, s := range src {
		d, ok := dst[aid]
		if !ok {
			dst[aid] = s
			added++
			continue
		}
		changed := false
		place := func(dp *Placement, sp Placement) { // season, offset and exceptions go with the id
			if dp.ID == 0 && sp.ID > 0 {
				*dp = sp
				changed = true
			}
		}
		if !d.noTVDB {
			place(&d.TVDB, s.TVDB)
		}
		place(&d.TMDB, s.TMDB)
		fill := func(dv *int, sv int) {
			if *dv == 0 && sv != 0 {
				*dv = sv
				changed = true
			}
		}
		fill(&d.TMDBMovie, s.TMDBMovie)
		fill(&d.MAL, s.MAL)
		fill(&d.AniList, s.AniList)
		fill(&d.Kitsu, s.Kitsu)
		fill(&d.ANN, s.ANN)
		if d.IMDB == "" && s.IMDB != "" {
			d.IMDB = s.IMDB
			changed = true
		}
		if changed {
			dst[aid] = d
			filled++
		}
	}
	return added, filled
}

// set swaps in a new mapping and rebuilds the reverse indexes.
func (l *Lists) set(entries map[int]Entry, customOnly bool) {
	byID := map[string]map[string][]int{}
	add := func(src string, id string, aid int) {
		if id == "" || id == "0" {
			return
		}
		if byID[src] == nil {
			byID[src] = map[string][]int{}
		}
		byID[src][id] = append(byID[src][id], aid)
	}
	for aid, e := range entries {
		add("tvdb", strconv.Itoa(e.TVDB.ID), aid)
		add("tmdb", strconv.Itoa(e.TMDB.ID), aid)
		add("tmdbmovie", strconv.Itoa(e.TMDBMovie), aid)
		add("imdb", e.IMDB, aid)
		add("mal", strconv.Itoa(e.MAL), aid)
		add("anilist", strconv.Itoa(e.AniList), aid)
		add("kitsu", strconv.Itoa(e.Kitsu), aid)
		add("ann", strconv.Itoa(e.ANN), aid)
	}
	for _, ids := range byID {
		for _, aids := range ids {
			sort.Ints(aids)
		}
	}
	l.mu.Lock()
	l.byAID, l.byID, l.sum, l.customOnly = entries, byID, [32]byte{}, customOnly
	l.mu.Unlock()
}

// Ready reports whether a mapping list has been loaded (anime-list-custom.xml
// alone doesn't count).
func (l *Lists) Ready() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.byAID != nil && !l.customOnly
}

// SetForTest replaces the mapping (for tests in other packages).
func (l *Lists) SetForTest(entries map[int]Entry) { l.set(entries, false) }

// --- lookups ----------------------------------------------------------------

// ByAID returns the entry for an AniDB id (zero Entry when unknown).
func (l *Lists) ByAID(aid int) Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.byAID[aid]
}

// aids returns the AniDB ids mapped to a source id (sorted).
func (l *Lists) aids(source, id string) []int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.byID[source][strings.TrimSpace(id)]
}

// AIDsForTVDB returns every AniDB entry mapped to a TVDB series (sorted).
func (l *Lists) AIDsForTVDB(tvdb int) []int {
	return l.aids("tvdb", strconv.Itoa(tvdb))
}

// FirstAID returns the AniDB entry that starts a TVDB series: season 1 (or
// absolute) with no offset, else the lowest id mapped to it. 0 if none.
func (l *Lists) FirstAID(tvdb int) int {
	return l.firstAID("tvdb", tvdb)
}

// StartsShow reports whether an AniDB entry is the first one of its series
// on a site ("tvdb" or "tmdb"), picked like FirstAID by the entry's
// placement there.
func (l *Lists) StartsShow(site string, aid int) bool {
	id := l.ByAID(aid).On(site).ID
	return id > 0 && l.firstAID(site, id) == aid
}

// firstAID returns the first entry mapped to a site's series whose placement
// starts it, else the lowest one (0 if none).
func (l *Lists) firstAID(site string, id int) int {
	aids := l.aids(site, strconv.Itoa(id))
	for _, aid := range aids {
		if l.ByAID(aid).On(site).Starts() {
			return aid
		}
	}
	if len(aids) > 0 {
		return aids[0]
	}
	return 0
}

// AIDFor returns the AniDB id for another site's id (source: tvdb, tmdb,
// tmdbmovie, imdb, mal, anilist, kitsu, ann; "tmdb" means a TMDB TV show,
// "tmdbmovie" a TMDB movie), or 0. When a series-level id
// (TVDB, TMDB, IMDb) covers several AniDB entries, the series' first entry
// wins.
func (l *Lists) AIDFor(source, id string) int {
	if source == "tvdb" {
		return l.FirstAID(atoi(id))
	}
	aids := l.aids(source, id)
	for _, aid := range aids {
		if l.StartsSeries(aid) {
			return aid
		}
	}
	if len(aids) > 0 {
		return aids[0]
	}
	return 0
}

// StartsSeries reports whether an AniDB entry is the first one of its TVDB
// series (or the only one on its TMDB show when there is no TVDB id), so
// series-level TVDB/TMDB data such as cast or air date describes it.
func (l *Lists) StartsSeries(aid int) bool {
	e := l.ByAID(aid)
	if e.TVDB.ID > 0 {
		return l.StartsShow("tvdb", aid)
	}
	if e.TMDB.ID > 0 {
		return len(l.aids("tmdb", strconv.Itoa(e.TMDB.ID))) == 1
	}
	return false
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// first value of a comma separated list ("123,456" -> "123").
func first(s string) string {
	s, _, _ = strings.Cut(s, ",")
	return strings.TrimSpace(s)
}
