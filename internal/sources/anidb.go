package sources

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"kizuna/internal/anidb"
	"kizuna/internal/fetch"
)

// anidbAPI is a variable so tests can point it at a fake server.
var anidbAPI = "http://api.anidb.net:9001/httpapi?request=anime&client=%s&clientver=%s&protover=1&aid=%d"

// ErrAniDBUnavailable: AniDB can't be asked right now (banned, daily limit, no
// client configured). Callers fall back to the title dump.
var ErrAniDBUnavailable = errors.New("anidb api unavailable")

// AniDBAnime is the parsed AniDB entry, reduced to what the provider uses.
type AniDBAnime struct {
	AID         int
	Type        string // "TV Series", "Movie", "OVA"...
	Restricted  bool
	StartDate   string
	EndDate     string
	Titles      []anidb.Title
	Description string
	Picture     string // full URL
	Genres      []string
	Creators    []AniDBCreator
	Characters  []AniDBCharacter
	MALIDs      []int
	ANNIDs      []int          // Anime News Network
	Episodes    []AniDBEpisode // sorted by Season, Index
}

type AniDBCreator struct{ Role, Name string }

type AniDBCharacter struct {
	Name, Seiyuu, Photo string
	Main                bool
}

// AniDBEpisode numbering:
//
//	regular episodes   season 1, index = episode number
//	specials  S1..     season 0, index 1..
//	openings  C1..     season 0, index 101..
//	endings            season 0, index 151..
//	trailers  T1..     season 0, index 201..
//	parodies  P1..     season 0, index 301..
//	other     O1..     season 0, index 401..
type AniDBEpisode struct {
	Season, Index int
	EpNo          string // AniDB's own number, e.g. "C3"
	Titles        []anidb.Title
	AirDate       string
	Summary       string
	Minutes       int
}

// ListIndex is the episode's number in Anime-Lists' mapping: the same as
// Index, except that credits are 100 + AniDB's C number there (C1 = 101,
// C2 = 102…), endings included, where Index starts endings at 151.
func (e AniDBEpisode) ListIndex() int {
	if e.Season == 0 && strings.HasPrefix(e.EpNo, "C") {
		if n, err := strconv.Atoi(e.EpNo[1:]); err == nil && n > 0 {
			return 100 + n
		}
	}
	return e.Index
}

// AniDB fetches anime details from the AniDB HTTP API, following its fair
// use rules: one request every 4 s, at most 200 a day,
// every answer cached for days, the same anime never asked twice in 24 h
// (failures included), exponential backoff after failures and a 24 h pause
// after a ban. All of it survives restarts (data/anidb-state.json).
type AniDB struct {
	f         *fetch.Client
	client    string
	clientVer string
	statePath string

	mu    sync.Mutex
	state struct {
		BannedUntil time.Time         `json:"banned_until"`
		Sent        []time.Time       `json:"sent"`     // API calls in the last 24 h
		Failures    int               `json:"failures"` // failed calls in a row (backoff step)
		RetryAt     time.Time         `json:"retry_at"` // no calls before this (backoff)
		Failed      map[int]time.Time `json:"failed"`   // aid -> last failed call (no retry for 24 h)
		// Fetched: aid -> last call AniDB answered with its entry. No call
		// for it for 24 h either, even when its cache file is missing (a
		// failed write, or a caller that looked before it was stored).
		Fetched map[int]time.Time `json:"fetched,omitempty"`
		// The client AniDB rejected (clientID) and when: no calls with it
		// for anidbClientPause.
		ClientRejected   string    `json:"client_rejected,omitempty"`
		ClientRejectedAt time.Time `json:"client_rejected_at,omitzero"`
	}
	queued  map[int]bool       // background fetches waiting or running
	pending []int              // background fetches waiting for a worker, in order
	workers int                // prefetch workers running
	working sync.WaitGroup     // the same workers (WaitIdle waits on it)
	calling map[int]*anidbCall // API calls queued or running (a second caller joins, uncounted)
	// failWhy: why an anime's last call failed (memory only: unknown after
	// a restart). told: the failure whose 24 h block was already logged at
	// Info, so a show rebuilt every 2 minutes doesn't repeat it.
	failWhy map[int]string
	told    map[int]time.Time
	// backedOffAt: when the last failure stepped the backoff up. A call
	// that went out before it and fails too is the same outage.
	backedOffAt time.Time

	// memoMu guards memo and prunedAt, apart from mu: mu is held over the
	// state file's writes, which reading an entry from memory must not
	// wait for. Never held together with mu.
	memoMu   sync.Mutex
	memo     map[int]anidbMemo
	prunedAt time.Time // last sweep of expired memo entries
}

// anidbMemo is a parsed entry kept in memory. A stale one is kept too, so
// that while AniDB can't be asked a show rebuilt every 2 minutes doesn't
// parse its (often 1 MB) cache file each time: it is used until that file
// changes (mod).
type anidbMemo struct {
	anime *AniDBAnime
	at    time.Time // when it was kept
	fresh bool      // within its TTL then
	mod   time.Time // a stale one: its cache file's last write
}

func NewAniDB(f *fetch.Client, dataDir, client, clientVer string) *AniDB {
	c := &AniDB{f: f, client: client, clientVer: clientVer,
		statePath: filepath.Join(dataDir, "anidb-state.json"), memo: map[int]anidbMemo{}, queued: map[int]bool{}, calling: map[int]*anidbCall{},
		failWhy: map[int]string{}, told: map[int]time.Time{}}
	f.Throttle("api.anidb.net:9001", anidbGap)
	c.load()
	if c.state.Failed == nil {
		c.state.Failed = map[int]time.Time{}
	}
	if c.state.Fetched == nil {
		c.state.Fetched = map[int]time.Time{}
	}
	if client == "" {
		slog.Warn("ANIDB_CLIENT not set: AniDB details (summaries, episodes) are off; register a client at anidb.net")
	}
	return c
}

func (c *AniDB) Enabled() bool { return c != nil && c.client != "" } // nil = not set up

// Anime returns the entry from memory, disk cache, or the API (in that order).
// Waits at most until ctx is done; an unfinished download keeps going in the
// background and is served from the cache on Plex's next refresh.
func (c *AniDB) Anime(ctx context.Context, aid int) (*AniDBAnime, error) {
	return c.anime(ctx, aid, false)
}

// Peek returns an entry from memory or the disk cache, of any age, without
// any network access, and whether it is still fresh. nil = never fetched.
func (c *AniDB) Peek(aid int) (a *AniDBAnime, fresh bool) {
	a, fresh, _, _ = c.local(aid)
	return a, fresh
}

// local returns the entry from memory or the disk cache, of any age,
// without any network access: whether it is fresh, whether it came from
// memory, and (read from disk) the cache file's age. What it parses is kept
// in memory, fresh or not, so the next build needn't parse it again.
func (c *AniDB) local(aid int) (a *AniDBAnime, fresh, memo bool, age time.Duration) {
	c.memoMu.Lock()
	m, kept := c.memo[aid]
	c.memoMu.Unlock()
	kept = kept && time.Since(m.at) < time.Hour
	if kept && m.fresh {
		return m.anime, true, true, 0
	}
	key := anidbKey(aid)
	if kept { // stale: still the same copy as last time?
		if mod, ok := c.f.CachedAt(key); ok && mod.Equal(m.mod) {
			return m.anime, false, true, 0
		}
	}
	data, mod, ok := c.f.CachedSince(key)
	if !ok {
		return nil, false, false, 0
	}
	if a, _ = parseAniDB(data); a == nil {
		return nil, false, false, 0
	}
	age = time.Since(mod)
	fresh = age < anidbTTL(a.EndDate)
	c.remember(a, fresh, mod)
	return a, fresh, false, age
}

// Prefetch fetches an entry in the background, within the fair use rules
// and only while fewer than anidbBackgroundLimit calls were made in 24 h.
// Its API call waits behind every call Plex is waiting for (a foreground
// one), keeping the same 4 s gap. It returns at once; the entry is in the
// cache for the next show build. A few workers fetch the waiting entries in
// order, so a big series doesn't start a goroutine for each.
func (c *AniDB) Prefetch(aid int) {
	if c.client == "" {
		return
	}
	c.mu.Lock()
	if c.queued[aid] {
		c.mu.Unlock()
		return
	}
	c.queued[aid] = true
	c.pending = append(c.pending, aid)
	if c.workers < anidbPrefetchWorkers {
		// Counted in working under c.mu, with workers: once working is
		// done, nothing is waiting or running (WaitIdle).
		c.workers++
		c.working.Go(c.prefetchWorker)
	}
	c.mu.Unlock()
}

// anidbPrefetchWorkers: background fetches run at most this many at a
// time, however many entries a /tvdb series asks for (only one call goes
// out every 4 s anyway).
const anidbPrefetchWorkers = 2

// prefetchWorker fetches the waiting background entries, in order, until
// none is left.
func (c *AniDB) prefetchWorker() {
	for {
		c.mu.Lock()
		if len(c.pending) == 0 {
			c.workers--
			c.mu.Unlock()
			return
		}
		aid := c.pending[0]
		c.pending = c.pending[1:]
		c.mu.Unlock()
		if _, err := c.anime(context.Background(), aid, true); err != nil {
			slog.Debug("anidb: background fetch skipped", "aid", aid, "why", err)
		}
		c.mu.Lock()
		delete(c.queued, aid)
		c.mu.Unlock()
	}
}

func (c *AniDB) anime(ctx context.Context, aid int, background bool) (*AniDBAnime, error) {
	start := time.Now()
	// Read here, not by Get: the TTL depends on the entry's end date, and a
	// stale copy is still used when the API may not be asked.
	a, fresh, memo, age := c.local(aid)
	if fresh && memo {
		slog.Debug("anidb: from memory", "aid", aid)
		if fetch.Observing(ctx) { // the key is only built for a match report
			fetch.Observe(ctx, anidbKey(aid), fetch.KindMemory, start, nil)
		}
		return a, nil
	}
	if fresh {
		slog.Debug("anidb: from cache", "aid", aid, "age", age.Round(time.Minute), "ttl", anidbTTL(a.EndDate))
		fetch.Observe(ctx, anidbKey(aid), fetch.KindCache, start, nil)
		return a, nil
	}
	return c.ask(ctx, aid, background, a, start)
}

// ask gets the entry from the API, when the fair use rules allow it, or
// else returns stale (the old copy local found; nil = none).
func (c *AniDB) ask(ctx context.Context, aid int, background bool, stale *AniDBAnime, start time.Time) (*AniDBAnime, error) {
	key := anidbKey(aid)
	if ok, why := c.allowed(aid, background); !ok {
		if a := c.again(ctx, aid, start); a != nil {
			return a, nil
		}
		slog.Debug("anidb: not calling the API", "aid", aid, "why", why, "stale_cache", stale != nil)
		if stale != nil {
			if fetch.Observing(ctx) { // the error is only built for a match report
				fetch.Observe(ctx, key, fetch.KindStale, start, fmt.Errorf("%w: %s", ErrAniDBUnavailable, why))
			}
			return stale, nil // stale beats nothing
		}
		err := fmt.Errorf("%w: %s", ErrAniDBUnavailable, why)
		fetch.Observe(ctx, key, fetch.KindFail, start, err)
		return nil, err
	}
	// parsed: the entry validate already parsed, so it isn't parsed twice.
	// Only a download this call started runs its Validate, and Get returns
	// that download's data only after it ended (NoStale: no stale copy
	// either), so after a successful Get, parsed is either set by it or nil
	// (a cache hit, or another caller's download joined): then parse here.
	// call: what sending made of this download's turn, for result (both run
	// on the download's goroutine, one after the other).
	var parsed *AniDBAnime
	var call *anidbCall
	data, err := c.f.Get(ctx, fetch.Request{URL: fmt.Sprintf(anidbAPI, c.client, c.clientVer, aid), Key: key,
		Validate: func(b []byte) (err error) {
			if parsed, err = c.validate(b); err == nil {
				c.answered(aid) // before it is cached: a caller that misses the file can't call again
			}
			return err
		},
		Before:  func() (err error) { call, err = c.sending(aid, background); return err },
		Done:    func(err error) { c.result(aid, call, err) },
		NoStale: true, Quiet: true, Background: background})
	if err != nil {
		if a := c.again(ctx, aid, start); a != nil {
			return a, nil
		}
		if stale != nil { // Get reported a failure (NoStale): the old copy answers after all
			fetch.Observe(ctx, key, fetch.KindStale, start, err)
			return stale, nil
		}
		return nil, err
	}
	a := parsed
	if a == nil {
		if a, err = parseAniDB(data); err != nil {
			return nil, err
		}
	}
	c.remember(a, true, time.Time{})
	return a, nil
}

// again reads the cache once more when the API was not asked because aid
// was fetched in the last 24 h: another caller's call may have stored the
// entry since local looked. nil: no fresh copy there (that call's cache
// write failed; AniDB is still not asked again).
func (c *AniDB) again(ctx context.Context, aid int, start time.Time) *AniDBAnime {
	if !c.fetchedLately(aid) {
		return nil
	}
	a, fresh, _, _ := c.local(aid)
	if a == nil || !fresh {
		return nil
	}
	slog.Debug("anidb: from cache, fetched by another call", "aid", aid)
	fetch.Observe(ctx, anidbKey(aid), fetch.KindCache, start, nil)
	return a
}

// remember keeps a parsed entry in memory for an hour; mod is a stale
// one's cache file write time. No size limit: expired entries are swept
// out every few minutes.
func (c *AniDB) remember(a *AniDBAnime, fresh bool, mod time.Time) {
	c.memoMu.Lock()
	defer c.memoMu.Unlock()
	now := time.Now()
	c.memo[a.AID] = anidbMemo{a, now, fresh, mod}
	if now.Sub(c.prunedAt) < 5*time.Minute {
		return
	}
	c.prunedAt = now
	for aid, m := range c.memo {
		if now.Sub(m.at) >= time.Hour {
			delete(c.memo, aid)
		}
	}
}

// anidbKey is the cache file of an anime's entry.
func anidbKey(aid int) string { return fmt.Sprintf("anidb/anime/%d.xml", aid) }

// anidbTTL: finished long ago -> rarely changes; airing or ended lately -> refresh every 6 days.
func anidbTTL(endDate string) time.Duration {
	end, err := time.Parse("2006-01-02", endDate)
	if err != nil || time.Since(end) < 60*24*time.Hour {
		return 6 * 24 * time.Hour
	}
	return 180 * 24 * time.Hour
}
