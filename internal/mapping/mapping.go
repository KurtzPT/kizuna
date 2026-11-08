// Package mapping links AniDB ids to the ids of other sites (TVDB, TMDB,
// IMDb, MyAnimeList, AniList, Kitsu, Anime News Network). Two lists are merged, in the order of
// MAPPING_SOURCES; a later list only fills ids the earlier ones lack:
//
//	animelists  Anime-Lists anime-list-master.xml: TVDB/TMDB/IMDb ids plus
//	            TVDB season, episode offset and per-episode exceptions
//	fribb       Fribb/anime-lists anime-list-mini.json: Anime-Lists merged with
//	            anime-offline-database (MAL, AniList, Kitsu, ANN ids) and ids looked
//	            up on TMDB (many extra IMDb/TMDB ids)
//
// An optional anime-list-custom.xml in the data folder (Anime-Lists format)
// replaces whole entries and always wins.
package mapping

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kizuna/internal/fetch"
)

const (
	animeListsURL = "https://raw.githubusercontent.com/Anime-Lists/anime-lists/master/anime-list-master.xml"
	fribbURL      = "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-mini.json"
)

// Entry is everything known about one AniDB id.
type Entry struct {
	AID        int
	TVDB       int // 0 when unknown or not a series ("movie", "OVA"...)
	TVDBSeason int // defaulttvdbseason; -1 = "a" (absolute)
	TVDBOffset int // episodeoffset
	TMDBTV     int
	TMDBMovie  int
	IMDB       string
	MAL        int
	AniList    int
	Kitsu      int
	ANN        int // Anime News Network
	// Episodes: Anime-Lists' per-episode exceptions (<mapping-list>), which
	// win over TVDBSeason/TVDBOffset. Only Anime-Lists has them.
	Episodes []EpisodeMap

	// noTVDB: Anime-Lists says this entry has no TVDB series on purpose
	// (tvdbid="movie", "OVA"...), so a fallback list must not add one.
	noTVDB bool
}

// EpisodeMap places one AniDB episode on TheTVDB: AniDB season 1 (regular)
// or 0 (S specials) episode AniDBEp is TVDB TVDBSeason (-1 = absolute order)
// episode TVDBEp. TVDBEp 0 = the episode has no TVDB counterpart.
type EpisodeMap struct {
	AniDBSeason, AniDBEp, TVDBSeason, TVDBEp int
}

// TVDBEpisode returns where an AniDB episode of this entry sits on its TVDB
// series: season (-1 = TheTVDB's absolute order) and episode number. The
// entry's own exceptions win; otherwise a regular episode N is episode
// N + TVDBOffset of TVDBSeason, and special SN is TVDB special N: Anime-Lists
// only lists episodes whose numbers don't match ("not necessary if episode
// numbers match up exactly within the season(s)"). Openings, endings,
// trailers and the like (index 100 and up) have no TVDB numbers of their own,
// so they're only placed by an exception. ok = false when it has no TVDB
// position (no series, an unlisted credit or trailer, or an exception saying
// "none").
func (e Entry) TVDBEpisode(anidbSeason, ep int) (season, number int, ok bool) {
	for _, m := range e.Episodes {
		if m.AniDBSeason == anidbSeason && m.AniDBEp == ep {
			return m.TVDBSeason, m.TVDBEp, m.TVDBEp > 0
		}
	}
	switch {
	case e.TVDB == 0:
		return 0, 0, false
	case anidbSeason == 0:
		return 0, ep, ep >= 1 && ep < 100
	case anidbSeason != 1 || ep+e.TVDBOffset <= 0:
		return 0, 0, false
	}
	return e.TVDBSeason, ep + e.TVDBOffset, true
}

// HasException reports whether the entry lists this AniDB episode itself.
func (e Entry) HasException(anidbSeason, ep int) bool {
	for _, m := range e.Episodes {
		if m.AniDBSeason == anidbSeason && m.AniDBEp == ep {
			return true
		}
	}
	return false
}

// Lists holds the merged mapping in memory.
type Lists struct {
	mu    sync.RWMutex
	byAID map[int]Entry
	byID  map[string]map[string][]int // source -> id -> aids (sorted)
	sum   [32]byte                    // of everything the mapping was loaded from (zero after SetForTest)
}

// listSources: how each MAPPING_SOURCES list is downloaded and parsed.
var listSources = map[string]struct {
	get   func(context.Context, *fetch.Client) ([]byte, error)
	parse func([]byte) (map[int]Entry, error)
}{
	"animelists": {getAnimeLists, parseAnimeLists},
	"fribb":      {getFribb, parseFribb},
}

// Load downloads (daily) the lists named in order and merges them.
// A list that fails to download is skipped as long as another one loads.
// When nothing changed since the last load (most days: the lists answer
// 304 and the custom file is the same), the loaded mapping is kept as is.
func (l *Lists) Load(ctx context.Context, f *fetch.Client, dataDir string, order []string) error {
	type downloaded struct {
		src  string
		data []byte
	}
	var got []downloaded
	var errs []error
	h := sha256.New()
	for _, src := range order {
		ls, ok := listSources[src]
		if !ok {
			continue
		}
		data, err := ls.get(ctx, f)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src, err))
			continue
		}
		got = append(got, downloaded{src, data})
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
		list, err := listSources[g.src].parse(g.data)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", g.src, err))
			continue
		}
		added, filled := merge(entries, list)
		slog.Info("mapping loaded", "list", g.src, "entries", len(list), "new", added, "filled", filled)
	}
	if len(entries) == 0 {
		return errors.Join(append(errs, errors.New("no mapping list loaded"))...)
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
	l.set(entries)
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
		if d.TVDB == 0 && !d.noTVDB && s.TVDB > 0 { // season, offset and exceptions belong to the TVDB id
			d.TVDB, d.TVDBSeason, d.TVDBOffset, d.Episodes = s.TVDB, s.TVDBSeason, s.TVDBOffset, s.Episodes
			changed = true
		}
		fill := func(dv *int, sv int) {
			if *dv == 0 && sv != 0 {
				*dv = sv
				changed = true
			}
		}
		fill(&d.TMDBTV, s.TMDBTV)
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
func (l *Lists) set(entries map[int]Entry) {
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
		add("tvdb", strconv.Itoa(e.TVDB), aid)
		add("tmdb", strconv.Itoa(e.TMDBTV), aid)
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
	l.byAID, l.byID, l.sum = entries, byID, [32]byte{}
	l.mu.Unlock()
}

// Ready reports whether a mapping has been loaded.
func (l *Lists) Ready() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.byAID != nil
}

// SetForTest replaces the mapping (for tests in other packages).
func (l *Lists) SetForTest(entries map[int]Entry) { l.set(entries) }

// --- Anime-Lists (XML) -----------------------------------------------------

func getAnimeLists(ctx context.Context, f *fetch.Client) ([]byte, error) {
	return f.Get(ctx, fetch.Request{URL: animeListsURL, Key: "mapping/anime-list-master.xml", TTL: 24 * time.Hour, Revalidate: true,
		Validate: func(b []byte) error {
			if !bytes.Contains(b[:min(len(b), 500)], []byte("<anime-list")) {
				return errors.New("not an anime-list")
			}
			return nil
		}})
}

func parseAnimeLists(data []byte) (map[int]Entry, error) {
	var doc struct {
		Anime []struct {
			AID    string `xml:"anidbid,attr"`
			TVDB   string `xml:"tvdbid,attr"`
			Season string `xml:"defaulttvdbseason,attr"`
			Offset string `xml:"episodeoffset,attr"`
			TMDBTV string `xml:"tmdbtv,attr"`
			TMDB   string `xml:"tmdbid,attr"`
			IMDB   string `xml:"imdbid,attr"`
			Maps   []struct {
				AniDBSeason string `xml:"anidbseason,attr"`
				TVDBSeason  string `xml:"tvdbseason,attr"`
				Start       string `xml:"start,attr"`
				End         string `xml:"end,attr"`
				Offset      string `xml:"offset,attr"`
				Pairs       string `xml:",chardata"`
			} `xml:"mapping-list>mapping"`
		} `xml:"anime"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	out := make(map[int]Entry, len(doc.Anime))
	for _, a := range doc.Anime {
		aid := atoi(a.AID)
		if aid == 0 {
			continue
		}
		season := atoi(a.Season)
		if a.Season == "a" {
			season = -1
		}
		tvdb := strings.TrimSpace(a.TVDB)
		e := Entry{AID: aid, TVDB: atoi(tvdb), TVDBSeason: season, TVDBOffset: atoi(a.Offset),
			TMDBTV: atoi(a.TMDBTV), TMDBMovie: atoi(first(a.TMDB)), IMDB: first(a.IMDB)}
		e.noTVDB = e.TVDB == 0 && tvdb != "" && !strings.EqualFold(tvdb, "unknown")
		if !strings.HasPrefix(e.IMDB, "tt") {
			e.IMDB = ""
		}
		for _, m := range a.Maps {
			e.Episodes = appendMapping(e.Episodes, atoi(m.AniDBSeason), tvdbSeason(m.TVDBSeason), m.Pairs, m.Start, m.End, m.Offset)
		}
		out[aid] = e
	}
	return out, nil
}

func tvdbSeason(s string) int {
	if strings.TrimSpace(s) == "a" {
		return -1
	}
	return atoi(s)
}

// appendMapping reads one <mapping>: explicit ";1-3;2-4+5;3-0;" pairs (AniDB
// episode - TVDB episode; "+" = several TVDB episodes, the first is used;
// 0 = none), or a start/end range shifted by offset. Explicit pairs come
// first, so they win over a range that covers the same episode.
func appendMapping(out []EpisodeMap, anidbSeason, tvdbSeason int, pairs, start, end, offset string) []EpisodeMap {
	listed := map[int]bool{}
	for _, p := range strings.Split(pairs, ";") {
		a, t, ok := strings.Cut(strings.TrimSpace(p), "-")
		if !ok {
			continue
		}
		t, _, _ = strings.Cut(t, "+")
		if n := atoi(a); n > 0 {
			out = append([]EpisodeMap{{anidbSeason, n, tvdbSeason, atoi(t)}}, out...)
			listed[n] = true
		}
	}
	if from, to := atoi(start), atoi(end); from > 0 && to >= from && to-from < 5000 {
		shift := atoi(offset)
		for n := from; n <= to; n++ {
			if !listed[n] {
				out = append(out, EpisodeMap{anidbSeason, n, tvdbSeason, n + shift})
			}
		}
	}
	return out
}

// --- Fribb/anime-lists (JSON) ----------------------------------------------

func getFribb(ctx context.Context, f *fetch.Client) ([]byte, error) {
	return f.Get(ctx, fetch.Request{URL: fribbURL, Key: "mapping/fribb-anime-list-mini.json", TTL: 24 * time.Hour, Revalidate: true,
		Validate: func(b []byte) error {
			if len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '[' {
				return errors.New("not a JSON list")
			}
			return nil
		}})
}

func parseFribb(data []byte) (map[int]Entry, error) {
	var list []struct {
		AID     int      `json:"anidb_id"`
		TVDB    int      `json:"tvdb_id"`
		IMDB    []string `json:"imdb_id"`
		MAL     int      `json:"mal_id"`
		AniList int      `json:"anilist_id"`
		Kitsu   int      `json:"kitsu_id"`
		ANN     int      `json:"animenewsnetwork_id"`
		TMDB    struct {
			TV    int   `json:"tv"`
			Movie []int `json:"movie"`
		} `json:"themoviedb_id"`
		Season *struct {
			TVDB *int `json:"tvdb"`
		} `json:"season"`
		Offset struct {
			TVDB int `json:"tvdb"`
		} `json:"episode_offset"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	out := make(map[int]Entry, len(list))
	for _, a := range list {
		if a.AID == 0 { // anime without an AniDB id are of no use here
			continue
		}
		e := Entry{AID: a.AID, TVDB: a.TVDB, TVDBOffset: a.Offset.TVDB, TMDBTV: a.TMDB.TV,
			MAL: a.MAL, AniList: a.AniList, Kitsu: a.Kitsu, ANN: a.ANN}
		// Fribb drops Anime-Lists' absolute marker ("a"): a TVDB id without a
		// season is an absolute-order entry.
		e.TVDBSeason = -1
		if a.Season != nil && a.Season.TVDB != nil {
			e.TVDBSeason = *a.Season.TVDB
		}
		if len(a.TMDB.Movie) > 0 {
			e.TMDBMovie = a.TMDB.Movie[0]
		}
		if len(a.IMDB) > 0 && strings.HasPrefix(a.IMDB[0], "tt") {
			e.IMDB = a.IMDB[0]
		}
		out[a.AID] = e
	}
	return out, nil
}

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
	aids := l.aids("tvdb", strconv.Itoa(tvdb))
	for _, aid := range aids {
		e := l.ByAID(aid)
		if (e.TVDBSeason == 1 || e.TVDBSeason == -1) && e.TVDBOffset == 0 {
			return aid
		}
	}
	if len(aids) > 0 {
		return aids[0]
	}
	return 0
}

// AIDFor returns the AniDB id for another site's id (source: tvdb, tmdb,
// imdb, mal, anilist, kitsu, ann; "tmdb" means a TMDB TV show), or 0. When a series-level id
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
	if e.TVDB > 0 {
		return l.FirstAID(e.TVDB) == aid
	}
	if e.TMDBTV > 0 {
		return len(l.aids("tmdb", strconv.Itoa(e.TMDBTV))) == 1
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
