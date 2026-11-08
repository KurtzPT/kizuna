package sources

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"kizuna/internal/fetch"
)

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
	anidbBanPause        = 24 * time.Hour
)

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
