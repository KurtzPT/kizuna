package sources

import (
	"bytes"
	"context"
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
	"unicode"
	"unicode/utf8"

	"kizuna/internal/anidb"
	"kizuna/internal/fetch"
)

// anidbAPI is a variable so tests can point it at a fake server.
var anidbAPI = "http://api.anidb.net:9001/httpapi?request=anime&client=%s&clientver=%s&protover=1&aid=%d"

// AniDB's fair use limits. Fixed on purpose: breaking them gets the IP banned
// (faster than 1 call / 2 s is an instant ban; 4 s is asked for sustained use;
// IPs above roughly 100-250 calls a day are flagged as leechers).
const (
	anidbGap        = 4 * time.Second
	anidbDailyLimit = 200
	// anidbBackgroundLimit: background fetches (other entries of a /tvdb
	// series, for episode titles) stop at this many calls in 24 h, so Plex's
	// own requests always have the rest of the daily limit.
	anidbBackgroundLimit = 150
	anidbPicture         = "https://cdn.anidb.net/images/main/"
	anidbBanPause        = 24 * time.Hour
)

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
	Rating      string // permanent rating, e.g. "8.12"
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

// Enabled reports whether an AniDB client is configured.
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

// anidbBackoff: waits after failed calls in a row (timeouts, lost
// connections, server errors), as AniDB asks: 30 s, 2 min, 15 min... 24 h.
var anidbBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}

// anidbRetry: an anime whose call failed is not asked again for this long.
const anidbRetry = 24 * time.Hour

// allowed checks ban, backoff, the anime's last failure and the daily limit,
// and counts the call when it is allowed.
func (c *AniDB) allowed(aid int, background bool) (bool, string) {
	if c.client == "" {
		return false, "ANIDB_CLIENT not set"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Before(c.state.BannedUntil) {
		return false, "banned until " + c.state.BannedUntil.Format(time.RFC3339)
	}
	if now.Before(c.state.RetryAt) {
		return false, fmt.Sprintf("backing off until %s (%d failures in a row)", c.state.RetryAt.Format(time.RFC3339), c.state.Failures)
	}
	for id, at := range c.state.Failed {
		if now.Sub(at) >= anidbRetry {
			delete(c.state.Failed, id)
			delete(c.failWhy, id)
			delete(c.told, id)
		}
	}
	if c.calling[aid] { // the fetch layer runs one request per anime: this caller just waits for it
		return true, ""
	}
	if at, failed := c.state.Failed[aid]; failed {
		c.tellBlocked(aid, at)
		return false, "this anime failed at " + at.Format(time.RFC3339) + "; retried 24 h later"
	}
	recent := c.state.Sent[:0]
	for _, t := range c.state.Sent {
		if now.Sub(t) < 24*time.Hour {
			recent = append(recent, t)
		}
	}
	c.state.Sent = recent
	if background && len(recent) >= anidbBackgroundLimit {
		return false, fmt.Sprintf("background share of the daily limit used (%d calls in 24 h)", len(recent))
	}
	if len(recent) >= anidbDailyLimit {
		return false, fmt.Sprintf("daily limit reached (%d calls in 24 h)", len(recent))
	}
	c.state.Sent = append(c.state.Sent, now)
	c.calling[aid] = true
	c.save()
	slog.Debug("anidb: calling the API", "aid", aid, "background", background, "calls_24h", len(c.state.Sent), "limit", anidbDailyLimit)
	return true, ""
}

// tellBlocked logs, once per failure, that an anime is skipped until its
// 24 h block ends: otherwise its details just stay missing (or stale) with
// nothing above debug saying why. Caller holds c.mu.
func (c *AniDB) tellBlocked(aid int, failedAt time.Time) {
	if c.told[aid].Equal(failedAt) {
		return
	}
	c.told[aid] = failedAt
	why := c.failWhy[aid]
	if why == "" {
		why = "unknown (it failed before a restart)"
	}
	slog.Info("AniDB: not asking about this anime again until its 24 h block ends (AniDB's rule after a failed call); "+
		"its details stay cached or missing until then", "aid", aid, "blocked_until", failedAt.Add(anidbRetry).Format(time.RFC3339),
		"failed_at", failedAt.Format(time.RFC3339), "failure", why)
}

// anidbEntryError: AniDB answered, but not with a usable entry for this
// anime (an <error> answer, or XML that does not parse). Only this anime
// waits; other calls go on.
type anidbEntryError struct {
	msg    string
	banned bool
}

func (e *anidbEntryError) Error() string { return "anidb: " + e.msg }

// validate rejects answers that must not be cached, and returns the
// parsed entry of one that may.
func (c *AniDB) validate(b []byte) (*AniDBAnime, error) {
	head := b[:min(len(b), 300)]
	if bytes.Contains(head, []byte("<error")) {
		return nil, &anidbEntryError{msg: strings.TrimSpace(string(head)), banned: bytes.Contains(bytes.ToLower(head), []byte("banned"))}
	}
	if !bytes.Contains(head, []byte("<anime")) {
		return nil, errors.New("anidb: unexpected answer") // e.g. a maintenance page: back off
	}
	a, err := parseAniDB(b)
	if err != nil {
		return nil, &anidbEntryError{msg: "unreadable XML: " + err.Error()}
	}
	return a, nil
}

// result records how a call ended (called once per network request, even
// when Plex stopped waiting for it).
func (c *AniDB) result(aid int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.calling, aid)
	now := time.Now()
	var entry *anidbEntryError
	var status *fetch.StatusError
	if err == nil {
		delete(c.failWhy, aid)
	} else {
		c.failWhy[aid] = err.Error()
	}
	retryAnime := now.Add(anidbRetry).Format(time.RFC3339)
	switch {
	case err == nil:
		delete(c.state.Failed, aid)
		c.state.Failures, c.state.RetryAt = 0, time.Time{}
	case errors.As(err, &entry) && entry.banned, errors.As(err, &status) && status.Code == 503:
		c.state.Failed[aid] = now
		c.state.BannedUntil = now.Add(anidbBanPause)
		slog.Error("AniDB banned this IP; AniDB API paused for 24 h (cache only)", "err", err)
	case errors.As(err, &entry):
		c.state.Failed[aid] = now
		slog.Warn("AniDB has no usable entry; not asking again for 24 h", "aid", aid, "retry_anime_at", retryAnime, "err", err)
	default:
		c.state.Failed[aid] = now
		c.state.Failures++
		wait := anidbBackoff[min(c.state.Failures, len(anidbBackoff))-1]
		c.state.RetryAt = now.Add(wait)
		slog.Warn("AniDB call failed; backing off, and not asking about this anime again for 24 h (AniDB's rule)",
			"aid", aid, "wait", wait, "retry_anime_at", retryAnime, "err", err)
	}
	c.save()
}

// load reads the ban/limit state saved by an earlier run. A state file that
// exists but can't be read may have held a ban or a full day of calls, and
// whatever it held ended at most 24 h after it was last written (a ban, the
// calls counted, a failed anime, the longest backoff). So the API waits
// until then, instead of starting with a clean slate that could get the IP
// banned.
func (c *AniDB) load() {
	b, err := os.ReadFile(c.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err == nil {
		if err = json.Unmarshal(b, &c.state); err == nil {
			return
		}
	}
	written := time.Now()
	if fi, serr := os.Stat(c.statePath); serr == nil && fi.ModTime().Before(written) {
		written = fi.ModTime()
	}
	c.state.BannedUntil, c.state.Sent, c.state.Failed = time.Time{}, nil, nil
	c.state.Failures, c.state.RetryAt = 0, written.Add(anidbBanPause)
	slog.Warn("AniDB state file unreadable: the ban and the calls of the last 24 h are unknown, so AniDB isn't asked until "+
		"24 h after the file was last written (cache only until then)", "file", c.statePath,
		"retry_at", c.state.RetryAt.Format(time.RFC3339), "err", err)
}

// save writes the ban/limit state; caller holds c.mu. The write is atomic,
// so a crash or a full disk leaves the last state, not a cut-off file.
func (c *AniDB) save() {
	b, _ := json.Marshal(c.state)
	if err := fetch.WriteFile(c.statePath, b); err != nil {
		slog.Warn("AniDB state not saved; a restart now would forget the latest calls", "file", c.statePath, "err", err)
	}
}

// anidbTTL: finished long ago -> rarely changes; airing -> refresh weekly.
func anidbTTL(endDate string) time.Duration {
	end, err := time.Parse("2006-01-02", endDate)
	if err != nil || time.Since(end) < 60*24*time.Hour {
		return 6 * 24 * time.Hour
	}
	return 180 * 24 * time.Hour
}

// --- XML parsing -----------------------------------------------------------

type anidbXML struct {
	ID          int           `xml:"id,attr"`
	Restricted  bool          `xml:"restricted,attr"`
	Type        string        `xml:"type"`
	StartDate   string        `xml:"startdate"`
	EndDate     string        `xml:"enddate"`
	Titles      []anidb.Title `xml:"titles>title"`
	Description string        `xml:"description"`
	Picture     string        `xml:"picture"`
	Permanent   string        `xml:"ratings>permanent"`
	Creators    []struct {
		Type string `xml:"type,attr"`
		Name string `xml:",chardata"`
	} `xml:"creators>name"`
	Tags []struct {
		Infobox bool   `xml:"infobox,attr"`
		Name    string `xml:"name"`
	} `xml:"tags>tag"`
	Characters []struct {
		Type   string `xml:"type,attr"`
		Name   string `xml:"name"`
		Seiyuu struct {
			Picture string `xml:"picture,attr"`
			Name    string `xml:",chardata"`
		} `xml:"seiyuu"`
	} `xml:"characters>character"`
	Resources []struct {
		Type string   `xml:"type,attr"`
		IDs  []string `xml:"externalentity>identifier"`
	} `xml:"resources>resource"`
	Episodes []struct {
		EpNo struct {
			Type int    `xml:"type,attr"`
			Text string `xml:",chardata"`
		} `xml:"epno"`
		Length  int           `xml:"length"`
		AirDate string        `xml:"airdate"`
		Titles  []anidb.Title `xml:"title"`
		Summary string        `xml:"summary"`
	} `xml:"episodes>episode"`
}

func parseAniDB(data []byte) (*AniDBAnime, error) {
	var x anidbXML
	if err := xml.Unmarshal(data, &x); err != nil {
		return nil, err
	}
	a := &AniDBAnime{AID: x.ID, Type: x.Type, Restricted: x.Restricted, StartDate: x.StartDate, EndDate: x.EndDate,
		Titles: x.Titles, Description: anidb.CleanSummary(x.Description), Rating: x.Permanent}
	if x.Picture != "" {
		a.Picture = anidbPicture + x.Picture
	}
	for _, c := range x.Creators {
		a.Creators = append(a.Creators, AniDBCreator{c.Type, strings.TrimSpace(c.Name)})
	}
	for _, t := range x.Tags {
		if t.Infobox { // only AniDB's "infobox" tags become genres
			a.Genres = append(a.Genres, capitalize(t.Name))
		}
	}
	sort.Strings(a.Genres)
	for _, ch := range x.Characters {
		if ch.Type != "main character in" && ch.Type != "secondary cast in" || strings.TrimSpace(ch.Seiyuu.Name) == "" {
			continue
		}
		c := AniDBCharacter{Name: ch.Name, Seiyuu: strings.TrimSpace(ch.Seiyuu.Name), Main: ch.Type == "main character in"}
		if ch.Seiyuu.Picture != "" {
			c.Photo = anidbPicture + ch.Seiyuu.Picture
		}
		a.Characters = append(a.Characters, c)
	}
	sort.SliceStable(a.Characters, func(i, j int) bool { return a.Characters[i].Main && !a.Characters[j].Main })
	for _, r := range x.Resources {
		for _, id := range r.IDs {
			n, err := strconv.Atoi(id)
			if err != nil {
				continue
			}
			switch r.Type { // AniDB resource types
			case "1":
				a.ANNIDs = append(a.ANNIDs, n)
			case "2":
				a.MALIDs = append(a.MALIDs, n)
			}
		}
	}

	// Episodes: see the AniDBEpisode doc comment for the numbering.
	type raw struct {
		typ, n int
		i      int
	}
	order := make([]raw, 0, len(x.Episodes))
	for i, e := range x.Episodes {
		no := strings.TrimSpace(e.EpNo.Text)
		digits := strings.TrimLeft(no, "SCTPO")
		n, err := strconv.Atoi(digits)
		if err != nil {
			continue
		}
		order = append(order, raw{e.EpNo.Type, n, i})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].typ != order[j].typ {
			return order[i].typ < order[j].typ
		}
		return order[i].n < order[j].n
	})
	endingStart := 1 << 30 // first "C" number that is an ending (its title starts with "Ending")
	for _, r := range order {
		e := x.Episodes[r.i]
		if r.typ == 3 && r.n < endingStart && len(e.Titles) > 0 && strings.HasPrefix(e.Titles[0].Text, "Ending") {
			endingStart = r.n
		}
	}
	for _, r := range order {
		e := x.Episodes[r.i]
		ep := AniDBEpisode{EpNo: strings.TrimSpace(e.EpNo.Text), Titles: e.Titles, AirDate: e.AirDate,
			Summary: anidb.CleanSummary(e.Summary), Minutes: e.Length}
		switch r.typ {
		case 1:
			ep.Season, ep.Index = 1, r.n
		case 2:
			ep.Index = r.n
		case 3:
			if r.n >= endingStart {
				ep.Index = 151 + r.n - endingStart
			} else {
				ep.Index = 100 + r.n
			}
		case 4:
			ep.Index = 200 + r.n
		case 5:
			ep.Index = 300 + r.n
		case 6:
			ep.Index = 400 + r.n
		default:
			continue
		}
		a.Episodes = append(a.Episodes, ep)
	}
	sort.SliceStable(a.Episodes, func(i, j int) bool {
		if a.Episodes[i].Season != a.Episodes[j].Season {
			return a.Episodes[i].Season < a.Episodes[j].Season
		}
		return a.Episodes[i].Index < a.Episodes[j].Index
	})
	return a, nil
}

// Details converts AniDB's show fields to the common shape.
func (a *AniDBAnime) Details() Details {
	d := Details{Genres: a.Genres, Date: a.StartDate} // may be "2004" or "2004-05"; the provider completes it
	if a.Restricted {
		d.ContentRating = "X"
	}
	for _, c := range a.Characters {
		d.Cast = append(d.Cast, Person{Name: c.Seiyuu, Role: c.Name, Photo: c.Photo})
	}
	for _, c := range a.Creators {
		switch c.Role {
		case "Animation Work":
			if d.Studio == "" {
				d.Studio = c.Name
			}
		case "Direction":
			d.Directors = append(d.Directors, c.Name)
		case "Original Work", "Series Composition":
			d.Writers = append(d.Writers, c.Name)
		}
	}
	return d
}

// IsMovie: an AniDB movie becomes S01E01 in a TV library.
func (a *AniDBAnime) IsMovie() bool { return a.Type == "Movie" }

// capitalize: "action" -> "Action", "science fiction" -> "Science Fiction".
func capitalize(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		r, n := utf8.DecodeRuneInString(w)
		words[i] = string(unicode.ToUpper(r)) + w[n:]
	}
	return strings.Join(words, " ")
}
