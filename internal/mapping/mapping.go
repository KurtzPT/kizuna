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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"kizuna/internal/fetch"
)

// Lists holds the merged mapping in memory. A load swaps in a whole new
// snapshot; each lookup reads one, so it never mixes two loads.
type Lists struct {
	cur atomic.Pointer[snapshot] // nil until something loaded
}

// snapshot is one loaded mapping. It never changes once stored.
type snapshot struct {
	byAID  map[int]Entry
	byNum  map[string]map[int][]int // source -> numeric id -> aids (sorted)
	byIMDB map[string][]int         // IMDb id -> aids (sorted)
	sum    [32]byte                 // of everything it was loaded from (zero = load in full next time)
	// customOnly: no list has loaded yet; byAID holds anime-list-custom.xml alone.
	customOnly bool
	from       []string // the lists it was built from
}

// empty answers lookups before anything loaded.
var empty = &snapshot{}

// get returns the current snapshot (empty before the first load).
func (l *Lists) get() *snapshot {
	if s := l.cur.Load(); s != nil {
		return s
	}
	return empty
}

// listSources: how each MAPPING_SOURCES list is downloaded and parsed.
var listSources = map[string]struct {
	get   func(context.Context, *fetch.Client, func([]byte) error) ([]byte, error)
	parse func([]byte) (map[int]Entry, error)
}{
	"animelists": {getAnimeLists, parseAnimeLists},
	"fribb":      {getFribb, parseFribb},
}

// nonEmpty parses a list and rejects one without entries.
func nonEmpty(parse func([]byte) (map[int]Entry, error)) func([]byte) (map[int]Entry, error) {
	return func(b []byte) (map[int]Entry, error) {
		list, err := parse(b)
		if err == nil && len(list) == 0 {
			err = errors.New("no entries")
		}
		return list, err
	}
}

// Load downloads (daily) the lists named in order and merges them.
// On the first load, a list that fails is skipped as long as another one
// loads. Once the mapping is Ready, a failed list it was built from keeps it
// as loaded (the lists fill each other's gaps, so one alone would lose ids
// and episode exceptions); a failed list it never had doesn't hold back
// changes to the others or to the custom file. When nothing changed since
// the last load (most days: the lists answer 304 and the custom file is the
// same), the loaded mapping is kept as is. A load cancelled while Kizuna
// stops changes nothing.
func (l *Lists) Load(ctx context.Context, f *fetch.Client, dataDir string, order []string) error {
	type downloaded struct {
		src  string
		data []byte
		list map[int]Entry // already parsed by fetch.Checked; nil = parse data
	}
	var got []downloaded
	var errs []error
	var failed []string
	h := sha256.New()
	for _, src := range order {
		ls, ok := listSources[src]
		if !ok {
			continue
		}
		c := &fetch.Checked[map[int]Entry]{Parse: nonEmpty(ls.parse)}
		data, err := ls.get(ctx, f, c.Validate)
		if err != nil {
			errs, failed = append(errs, fmt.Errorf("%s: %w", src, err)), append(failed, src)
			continue
		}
		fmt.Fprintf(h, "%s:%d:", src, len(data))
		h.Write(data)
		list, ok := c.Result(data)
		if ok {
			data = nil // parsed already: don't hold on to the download
		}
		got = append(got, downloaded{src, data, list})
	}
	if err := ctx.Err(); err != nil { // stopping: fetch may still answer with cached copies
		return err
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

	var entries map[int]Entry // the first list's own map: this load made it
	var from []string
	for i := range got {
		g := &got[i]
		list := g.list
		if list == nil {
			var err error
			list, err = listSources[g.src].parse(g.data)
			g.data = nil
			if err != nil {
				errs, failed = append(errs, fmt.Errorf("%s: %w", g.src, err)), append(failed, g.src)
				continue
			}
		}
		added, filled := len(list), 0
		if entries == nil {
			entries = list
		} else {
			added, filled = merge(entries, list)
		}
		from = append(from, g.src)
		slog.Info("mapping loaded", "list", g.src, "entries", len(list), "new", added, "filled", filled)
	}
	listsLoaded := len(entries) > 0
	if !listsLoaded {
		errs = append(errs, errors.New("no mapping list loaded"))
	}
	if l.keeps(listsLoaded, failed) { // keep the full mapping of an earlier load (custom entries included)
		return errors.Join(append(errs, errors.New("keeping the mapping loaded earlier"))...)
	}
	if entries == nil {
		entries = map[int]Entry{}
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
	if len(errs) > 0 { // after a problem, load in full again: its error is logged until it's gone
		sum = [32]byte{}
	}
	l.set(&snapshot{byAID: entries, sum: sum, customOnly: !listsLoaded, from: from})
	return errors.Join(errs...) // non-nil only to log a list that failed
}

// keeps reports whether a load whose lists failed as given should keep the
// Ready mapping loaded earlier: when no list loaded, or when one that failed
// went into that mapping (without it, the mapping would lose its ids).
func (l *Lists) keeps(listsLoaded bool, failed []string) bool {
	s := l.cur.Load()
	if s == nil || s.customOnly {
		return false
	}
	return !listsLoaded || slices.ContainsFunc(failed, func(src string) bool { return slices.Contains(s.from, src) })
}

// loadedFrom reports whether the current mapping was loaded from inputs
// with this sum.
func (l *Lists) loadedFrom(sum [32]byte) bool {
	s := l.cur.Load()
	return s != nil && sum != [32]byte{} && s.sum == sum
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

// set builds s's reverse indexes from s.byAID and swaps it in. s.sum is
// what it was loaded from (zero: load in full next time).
func (l *Lists) set(s *snapshot) {
	s.byNum, s.byIMDB = map[string]map[int][]int{}, map[string][]int{}
	add := func(src string, id, aid int) {
		if id == 0 {
			return
		}
		if s.byNum[src] == nil {
			s.byNum[src] = map[int][]int{}
		}
		s.byNum[src][id] = append(s.byNum[src][id], aid)
	}
	for aid, e := range s.byAID {
		add("tvdb", e.TVDB.ID, aid)
		add("tmdb", e.TMDB.ID, aid)
		add("tmdbmovie", e.TMDBMovie, aid)
		add("mal", e.MAL, aid)
		add("anilist", e.AniList, aid)
		add("kitsu", e.Kitsu, aid)
		add("ann", e.ANN, aid)
		if e.IMDB != "" {
			s.byIMDB[e.IMDB] = append(s.byIMDB[e.IMDB], aid)
		}
	}
	for _, ids := range s.byNum {
		for _, aids := range ids {
			sort.Ints(aids)
		}
	}
	for _, aids := range s.byIMDB {
		sort.Ints(aids)
	}
	l.cur.Store(s)
}

// Ready reports whether a mapping list has been loaded (anime-list-custom.xml
// alone doesn't count).
func (l *Lists) Ready() bool {
	s := l.cur.Load()
	return s != nil && !s.customOnly
}

// SetForTest replaces the mapping (for tests in other packages).
func (l *Lists) SetForTest(entries map[int]Entry) { l.set(&snapshot{byAID: entries}) }

// --- lookups ----------------------------------------------------------------
// Each one reads a single snapshot and does all its work on it.

// ByAID returns the entry for an AniDB id (zero Entry when unknown).
func (l *Lists) ByAID(aid int) Entry { return l.get().byAID[aid] }

// AIDsForTVDB returns every AniDB entry mapped to a TVDB series (sorted).
func (l *Lists) AIDsForTVDB(tvdb int) []int { return l.get().byNum["tvdb"][tvdb] }

// FirstAID returns the AniDB entry that starts a TVDB series: season 1 (or
// absolute) with no offset, else the lowest id mapped to it. 0 if none.
func (l *Lists) FirstAID(tvdb int) int { return l.get().firstAID("tvdb", tvdb) }

// StartsShow reports whether an AniDB entry is the first one of its series
// on a site ("tvdb" or "tmdb"), picked like FirstAID by the entry's
// placement there.
func (l *Lists) StartsShow(site string, aid int) bool { return l.get().startsShow(site, aid) }

// AIDFor returns the AniDB id for another site's id (source: tvdb, tmdb,
// tmdbmovie, imdb, mal, anilist, kitsu, ann; "tmdb" means a TMDB TV show,
// "tmdbmovie" a TMDB movie), or 0. When a series-level id
// (TVDB, TMDB, IMDb) covers several AniDB entries, the series' first entry
// wins.
func (l *Lists) AIDFor(source, id string) int { return l.get().aidFor(source, id) }

// StartsSeries reports whether an AniDB entry is the first one of its TVDB
// series (or the only one on its TMDB show when there is no TVDB id), so
// series-level TVDB/TMDB data such as cast or air date describes it.
func (l *Lists) StartsSeries(aid int) bool { return l.get().startsSeries(aid) }

// aids returns the AniDB ids mapped to a source id (sorted).
func (s *snapshot) aids(source, id string) []int {
	if source == "imdb" {
		return s.byIMDB[strings.TrimSpace(id)]
	}
	return s.byNum[source][atoi(id)]
}

func (s *snapshot) startsShow(site string, aid int) bool {
	id := s.byAID[aid].On(site).ID
	return id > 0 && s.firstAID(site, id) == aid
}

// firstAID returns the first entry mapped to a site's series whose placement
// starts it, else the lowest one (0 if none).
func (s *snapshot) firstAID(site string, id int) int {
	aids := s.byNum[site][id]
	for _, aid := range aids {
		if s.byAID[aid].On(site).Starts() {
			return aid
		}
	}
	if len(aids) > 0 {
		return aids[0]
	}
	return 0
}

func (s *snapshot) aidFor(source, id string) int {
	if source == "tvdb" {
		return s.firstAID("tvdb", atoi(id))
	}
	aids := s.aids(source, id)
	for _, aid := range aids {
		if s.startsSeries(aid) {
			return aid
		}
	}
	if len(aids) > 0 {
		return aids[0]
	}
	return 0
}

func (s *snapshot) startsSeries(aid int) bool {
	e := s.byAID[aid]
	if e.TVDB.ID > 0 {
		return s.startsShow("tvdb", aid)
	}
	if e.TMDB.ID > 0 {
		return len(s.byNum["tmdb"][e.TMDB.ID]) == 1
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
