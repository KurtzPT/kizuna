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
	}
	memo     map[int]anidbMemo
	prunedAt time.Time    // last sweep of expired memo entries
	queued   map[int]bool // background fetches running
	calling  map[int]bool // API calls running (a second caller joins, uncounted)
	// failWhy: why an anime's last call failed (memory only: unknown after
	// a restart). told: the failure whose 24 h block was already logged at
	// Info, so a show rebuilt every 2 minutes doesn't repeat it.
	failWhy map[int]string
	told    map[int]time.Time
}

type anidbMemo struct {
	anime *AniDBAnime
	at    time.Time
}

func NewAniDB(f *fetch.Client, dataDir, client, clientVer string) *AniDB {
	c := &AniDB{f: f, client: client, clientVer: clientVer,
		statePath: filepath.Join(dataDir, "anidb-state.json"), memo: map[int]anidbMemo{}, queued: map[int]bool{}, calling: map[int]bool{},
		failWhy: map[int]string{}, told: map[int]time.Time{}}
	f.Throttle("api.anidb.net:9001", anidbGap)
	c.load()
	if c.state.Failed == nil {
		c.state.Failed = map[int]time.Time{}
	}
	if client == "" {
		slog.Warn("ANIDB_CLIENT not set: AniDB details (summaries, episodes) are off; register a client at anidb.net")
	}
	return c
}

func (c *AniDB) Enabled() bool { return c.client != "" }

// Anime returns the entry from memory, disk cache, or the API (in that order).
// Waits at most until ctx is done; an unfinished download keeps going in the
// background and is served from the cache on Plex's next refresh.
func (c *AniDB) Anime(ctx context.Context, aid int) (*AniDBAnime, error) {
	return c.anime(ctx, aid, false)
}

// Peek returns an entry from memory or the disk cache, of any age, without
// any network access, and whether it is still fresh. nil = never fetched.
func (c *AniDB) Peek(aid int) (a *AniDBAnime, fresh bool) {
	c.mu.Lock()
	if m, ok := c.memo[aid]; ok && time.Since(m.at) < time.Hour {
		c.mu.Unlock()
		return m.anime, true
	}
	c.mu.Unlock()
	data, age, ok := c.f.Cached(fmt.Sprintf("anidb/anime/%d.xml", aid))
	if !ok {
		return nil, false
	}
	if a, _ = parseAniDB(data); a == nil {
		return nil, false
	}
	if fresh = age < anidbTTL(a.EndDate); fresh {
		c.remember(a) // the next build of the series needn't parse it again
	}
	return a, fresh
}

// Prefetch fetches an entry in the background, within the fair use rules
// and only while fewer than anidbBackgroundLimit calls were made in 24 h.
// It returns at once; the entry is in the cache for the next show build.
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
	c.mu.Unlock()
	go func() {
		_, err := c.anime(context.Background(), aid, true)
		if err != nil {
			slog.Debug("anidb: background fetch skipped", "aid", aid, "why", err)
		}
		c.mu.Lock()
		delete(c.queued, aid)
		c.mu.Unlock()
	}()
}

func (c *AniDB) anime(ctx context.Context, aid int, background bool) (*AniDBAnime, error) {
	c.mu.Lock()
	if m, ok := c.memo[aid]; ok && time.Since(m.at) < time.Hour {
		c.mu.Unlock()
		slog.Debug("anidb: from memory", "aid", aid)
		return m.anime, nil
	}
	c.mu.Unlock()

	key := fmt.Sprintf("anidb/anime/%d.xml", aid)
	cached, age, ok := c.f.Cached(key)
	var a *AniDBAnime
	if ok {
		a, _ = parseAniDB(cached)
	}
	if a != nil && age < anidbTTL(a.EndDate) {
		slog.Debug("anidb: from cache", "aid", aid, "age", age.Round(time.Minute), "ttl", anidbTTL(a.EndDate))
		c.remember(a)
		return a, nil
	}
	if ok, why := c.allowed(aid, background); !ok {
		slog.Debug("anidb: not calling the API", "aid", aid, "why", why, "stale_cache", a != nil)
		if a != nil {
			return a, nil // stale beats nothing
		}
		return nil, fmt.Errorf("%w: %s", ErrAniDBUnavailable, why)
	}
	// parsed: the entry validate already parsed, so it isn't parsed twice.
	// Only a download this call started runs its Validate, and Get returns
	// that download's data only after it ended (NoStale: no stale copy
	// either), so after a successful Get, parsed is either set by it or nil
	// (a cache hit, or another caller's download joined): then parse here.
	var parsed *AniDBAnime
	data, err := c.f.Get(ctx, fetch.Request{URL: fmt.Sprintf(anidbAPI, c.client, c.clientVer, aid), Key: key,
		Validate: func(b []byte) (err error) { parsed, err = c.validate(b); return err },
		NoStale:  true, Quiet: true, Done: func(err error) { c.result(aid, err) }})
	if err != nil {
		if a != nil {
			return a, nil
		}
		return nil, err
	}
	if a = parsed; a == nil {
		if a, err = parseAniDB(data); err != nil {
			return nil, err
		}
	}
	c.remember(a)
	return a, nil
}

// remember keeps a parsed entry in memory for an hour. No size limit:
// expired entries are swept out every few minutes.
func (c *AniDB) remember(a *AniDBAnime) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.memo[a.AID] = anidbMemo{a, now}
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

// anidbTTL: finished long ago -> rarely changes; airing or ended lately -> refresh every 6 days.
func anidbTTL(endDate string) time.Duration {
	end, err := time.Parse("2006-01-02", endDate)
	if err != nil || time.Since(end) < 60*24*time.Hour {
		return 6 * 24 * time.Hour
	}
	return 180 * 24 * time.Hour
}
