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
	AID int
	// TVDB: the entry's place on its TVDB series (ID 0 when unknown or not a
	// series: "movie", "OVA"...). Always Mapped.
	TVDB Placement
	// TMDB: the entry's place on its TheMovieDb TV show, whose seasons can
	// differ from TheTVDB's. Mapped only when the list gives a TMDB season.
	TMDB      Placement
	TMDBMovie int
	IMDB      string
	MAL       int
	AniList   int
	Kitsu     int
	ANN       int // Anime News Network

	// noTVDB: Anime-Lists says this entry has no TVDB series on purpose
	// (tvdbid="movie", "OVA"...), so a fallback list must not add one.
	noTVDB bool
}

// Placement is where an entry's episodes sit on one site's series: TheTVDB
// (defaulttvdbseason, episodeoffset, <mapping tvdbseason="…">) or TheMovieDb
// (tmdbseason, tmdboffset, <mapping tmdbseason="…">).
type Placement struct {
	ID     int // the series or show; 0 = none
	Season int // the default season; -1 = "a" (absolute). Only when Mapped
	Offset int // episode offset
	// Mapped: the list gives the season, so Season and Offset place the
	// episodes no exception lists. Without it only the exceptions are known
	// (the provider pairs TheMovieDb like TheTVDB then). TheTVDB placements
	// are always mapped.
	Mapped bool
	// Episodes: Anime-Lists' per-episode exceptions (<mapping-list>), which
	// win over Season/Offset. Only Anime-Lists has them.
	Episodes []EpisodeMap
	// Ranges: <mapping>s with no end ("start=1156 offset=-1155": that
	// episode and every later one) or no start and end at all (the offset
	// applies to the whole AniDB season). Checked after Episodes.
	Ranges []EpisodeRange
}

// On returns the entry's placement on a site: "tvdb" or "tmdb" (TheMovieDb's
// TV show). Zero for any other site.
func (e Entry) On(site string) Placement {
	switch site {
	case "tvdb":
		return e.TVDB
	case "tmdb":
		return e.TMDB
	}
	return Placement{}
}

// EpisodeMap places one AniDB episode on TheTVDB or TheMovieDb: AniDB season
// 1 (regular) or 0 (S specials) episode AniDBEp is Season (-1 = absolute
// order) episode Ep there. Ep 0 = the episode has no counterpart.
type EpisodeMap struct {
	AniDBSeason, AniDBEp, Season, Ep int
}

// EpisodeRange: AniDB episodes From..To (inclusive) of AniDBSeason are
// episode n + Offset of Season.
type EpisodeRange struct {
	AniDBSeason, From, To, Season, Offset int
}

func (r EpisodeRange) covers(anidbSeason, ep int) bool {
	return r.AniDBSeason == anidbSeason && ep >= r.From && ep <= r.To
}

// Episode returns where an AniDB episode of the entry sits on this series:
// season (-1 = the site's absolute order) and episode number. ep is in
// Anime-Lists' numbering (credits C1 = 101, trailers T1 = 201, other O1 =
// 401: see sources.AniDBEpisode.ListIndex). The entry's own exceptions win
// (single episodes, then open ranges); otherwise, when Mapped, a regular
// episode N is episode N + Offset of Season, and special SN is special N:
// Anime-Lists only lists episodes whose numbers don't match ("not necessary
// if episode numbers match up exactly within the season(s)"). Openings,
// endings, trailers and the like (index 100 and up) have no numbers of their
// own on TheTVDB or TheMovieDb, so they're only placed by an exception. ok =
// false when it has no position (no series, an unlisted credit or trailer, or
// an exception saying "none").
func (p Placement) Episode(anidbSeason, ep int) (season, number int, ok bool) {
	if s, n, listed := p.exception(anidbSeason, ep); listed {
		return s, n, n > 0
	}
	return p.defaultEpisode(anidbSeason, ep)
}

// HasException reports whether the placement lists this AniDB episode itself.
func (p Placement) HasException(anidbSeason, ep int) bool {
	_, _, listed := p.exception(anidbSeason, ep)
	return listed
}

// Starts reports whether the placement starts its series: season 1 (or
// absolute) with no offset.
func (p Placement) Starts() bool {
	return p.Mapped && (p.Season == 1 || p.Season == -1) && p.Offset == 0
}

// exception finds an AniDB episode in the exceptions: single episodes first,
// then open ranges.
func (p Placement) exception(anidbSeason, ep int) (season, number int, listed bool) {
	for _, m := range p.Episodes {
		if m.AniDBSeason == anidbSeason && m.AniDBEp == ep {
			return m.Season, m.Ep, true
		}
	}
	for _, r := range p.Ranges {
		if r.covers(anidbSeason, ep) {
			return r.Season, ep + r.Offset, true
		}
	}
	return 0, 0, false
}

// defaultEpisode places an episode no exception lists: regular N is N +
// offset of the default season, special SN is special N.
func (p Placement) defaultEpisode(anidbSeason, ep int) (season, number int, ok bool) {
	switch {
	case p.ID <= 0 || !p.Mapped:
		return 0, 0, false
	case anidbSeason == 0:
		return 0, ep, ep >= 1 && ep < 100
	case anidbSeason != 1 || ep+p.Offset <= 0:
		return 0, 0, false
	}
	return p.Season, ep + p.Offset, true
}

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
	listsLoaded := len(entries) > 0
	if !listsLoaded {
		errs = append(errs, errors.New("no mapping list loaded"))
		if l.Ready() { // keep the full mapping of an earlier load (custom entries included)
			return errors.Join(errs...)
		}
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
			TMDBSn string `xml:"tmdbseason,attr"`
			TMDBOf string `xml:"tmdboffset,attr"`
			TMDB   string `xml:"tmdbid,attr"`
			IMDB   string `xml:"imdbid,attr"`
			Maps   []struct {
				AniDBSeason string `xml:"anidbseason,attr"`
				TVDBSeason  string `xml:"tvdbseason,attr"`
				TMDBSeason  string `xml:"tmdbseason,attr"`
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
		tvdb := strings.TrimSpace(a.TVDB)
		e := Entry{AID: aid,
			TVDB: Placement{ID: atoi(tvdb), Season: listSeason(a.Season), Offset: atoi(a.Offset), Mapped: true},
			TMDB: Placement{ID: atoi(a.TMDBTV), Season: listSeason(a.TMDBSn), Offset: atoi(a.TMDBOf),
				Mapped: strings.TrimSpace(a.TMDBSn) != ""},
			TMDBMovie: atoi(first(a.TMDB)), IMDB: first(a.IMDB)}
		e.noTVDB = e.TVDB.ID == 0 && tvdb != "" && !strings.EqualFold(tvdb, "unknown")
		if !strings.HasPrefix(e.IMDB, "tt") {
			e.IMDB = ""
		}
		// A <mapping> is for TheTVDB (tvdbseason), TheMovieDb (tmdbseason) or both.
		for _, m := range a.Maps {
			if strings.TrimSpace(m.TVDBSeason) != "" {
				e.TVDB.addMapping(atoi(m.AniDBSeason), listSeason(m.TVDBSeason), m.Pairs, m.Start, m.End, m.Offset)
			}
			if strings.TrimSpace(m.TMDBSeason) != "" {
				e.TMDB.addMapping(atoi(m.AniDBSeason), listSeason(m.TMDBSeason), m.Pairs, m.Start, m.End, m.Offset)
			}
		}
		out[aid] = e
	}
	return out, nil
}

// listSeason reads a TVDB or TMDB season attribute ("a" = absolute: -1).
func listSeason(s string) int {
	if strings.TrimSpace(s) == "a" {
		return -1
	}
	return atoi(s)
}

// addMapping reads one <mapping> into a TheTVDB or TheMovieDb placement
// (season is that site's): explicit ";1-3;2-4+5;3-0;" pairs (AniDB episode -
// episode there; "+" = several episodes there, the first is used; 0 = none),
// and/or a start..end range shifted by offset. Explicit pairs come first, so
// they win over a range that covers the same episode.
// A range without end runs on (every later episode); an offset without start
// and end covers the whole AniDB season. Such open ranges go to Ranges.
// An open range of specials stays inside its hundred: specials 1–99, credits
// 101–199, trailers 201–299… are separate kinds of episodes.
func (pl *Placement) addMapping(anidbSeason, season int, pairs, start, end, offset string) {
	listed := map[int]bool{}
	var explicit []EpisodeMap
	for _, p := range strings.Split(pairs, ";") {
		a, t, ok := strings.Cut(strings.TrimSpace(p), "-")
		if !ok {
			continue
		}
		t, _, _ = strings.Cut(t, "+")
		if n := atoi(a); n > 0 {
			explicit = append(explicit, EpisodeMap{anidbSeason, n, season, atoi(t)})
			listed[n] = true
		}
	}
	// Kept in order: an AniDB episode listed twice (";1-1;1-2;", two
	// episodes on the other site) gets the first, as with "+".
	pl.Episodes = append(explicit, pl.Episodes...)
	from, to, shift := atoi(start), atoi(end), atoi(offset)
	switch {
	case strings.TrimSpace(start) == "" && strings.TrimSpace(end) == "":
		if strings.TrimSpace(offset) == "" {
			return // only pairs
		}
		from = 1
		fallthrough
	case strings.TrimSpace(end) == "":
		if from <= 0 {
			return
		}
		to = 1 << 30
		if anidbSeason == 0 {
			to = from/100*100 + 99
		}
		// Explicit pairs win: they come first in Episodes, which is checked first.
		pl.Ranges = append(pl.Ranges, EpisodeRange{anidbSeason, from, to, season, shift})
	case from > 0 && to >= from && to-from < 5000:
		for n := from; n <= to; n++ {
			if !listed[n] {
				pl.Episodes = append(pl.Episodes, EpisodeMap{anidbSeason, n, season, n + shift})
			}
		}
	}
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
			TMDB *int `json:"tmdb"`
		} `json:"season"`
		Offset struct {
			TVDB int `json:"tvdb"`
			TMDB int `json:"tmdb"`
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
		e := Entry{AID: a.AID,
			TVDB: Placement{ID: a.TVDB, Offset: a.Offset.TVDB, Mapped: true},
			TMDB: Placement{ID: a.TMDB.TV, Offset: a.Offset.TMDB},
			MAL:  a.MAL, AniList: a.AniList, Kitsu: a.Kitsu, ANN: a.ANN}
		// Fribb drops Anime-Lists' absolute marker ("a"): a TVDB id without a
		// season is an absolute-order entry.
		e.TVDB.Season = -1
		if a.Season != nil && a.Season.TVDB != nil {
			e.TVDB.Season = *a.Season.TVDB
		}
		// A TMDB id without a season stays unmapped rather than absolute: it
		// is Anime-Lists' "a", but also every TMDB id Fribb looked up itself.
		if a.Season != nil && a.Season.TMDB != nil {
			e.TMDB.Mapped, e.TMDB.Season = true, *a.Season.TMDB
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
