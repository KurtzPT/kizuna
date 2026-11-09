package sources

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
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
	// anidbClientPause: AniDB said the client name or version is wrong.
	// Every call would get the same answer (and AniDB may count them as
	// abuse), so none is made for this long, or until ANIDB_CLIENT or
	// ANIDB_CLIENTVER change (a restart with new values asks again at once).
	anidbClientPause = 24 * time.Hour
)

// anidbBackoff: waits after failed calls in a row (timeouts, lost
// connections, server errors), as AniDB asks: 30 s, 2 min, 15 min... 24 h.
var anidbBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}

// anidbRetry: an anime whose call failed is not asked again for this long.
const anidbRetry = 24 * time.Hour

// anidbCall is one API call, from the moment it is allowed (and counted)
// until it ends.
type anidbCall struct {
	at   time.Time // its entry in state.Sent: when it was queued, then when it went out
	sent bool      // it left the 4 s queue and went out
}

// allowed checks ban, backoff, the anime's last failure and the daily limit,
// and counts the call when it is allowed. The count is taken while the call
// waits in the 4 s queue, so queued calls never add up past the limit;
// sending checks again when it leaves the queue.
func (c *AniDB) allowed(aid int, background bool) (bool, string) {
	if c.client == "" {
		return false, "ANIDB_CLIENT not set"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if why := c.paused(now); why != "" {
		return false, why
	}
	if c.calling[aid] != nil { // the fetch layer runs one request per anime: this caller just waits for it
		return true, ""
	}
	if why := c.admit(aid, background, now); why != "" {
		return false, why
	}
	c.save()
	return true, ""
}

// paused says why no call may go out now ("" = none): a ban or a backoff.
// Caller holds c.mu.
func (c *AniDB) paused(now time.Time) string {
	if now.Before(c.state.BannedUntil) {
		return "banned until " + c.state.BannedUntil.Format(time.RFC3339)
	}
	if until := c.state.ClientRejectedAt.Add(anidbClientPause); c.state.ClientRejected == c.clientID() && now.Before(until) {
		return "AniDB rejected ANIDB_CLIENT / ANIDB_CLIENTVER; paused until " + until.Format(time.RFC3339) + " or until they change"
	}
	if now.Before(c.state.RetryAt) {
		return fmt.Sprintf("backing off until %s (%d failures in a row)", c.state.RetryAt.Format(time.RFC3339), c.state.Failures)
	}
	return ""
}

// admit checks the anime's last failure and the daily limit, and counts a
// new call for aid ("" = allowed). Caller holds c.mu and saves.
func (c *AniDB) admit(aid int, background bool, now time.Time) string {
	for id, at := range c.state.Failed {
		if now.Sub(at) >= anidbRetry {
			delete(c.state.Failed, id)
			delete(c.failWhy, id)
			delete(c.told, id)
		}
	}
	if at, failed := c.state.Failed[aid]; failed {
		c.tellBlocked(aid, at)
		return "this anime failed at " + at.Format(time.RFC3339) + "; retried 24 h later"
	}
	recent := c.state.Sent[:0]
	for _, t := range c.state.Sent {
		if now.Sub(t) < 24*time.Hour {
			recent = append(recent, t)
		}
	}
	c.state.Sent = recent
	if background && len(recent) >= anidbBackgroundLimit {
		return fmt.Sprintf("background share of the daily limit used (%d calls in 24 h)", len(recent))
	}
	if len(recent) >= anidbDailyLimit {
		return fmt.Sprintf("daily limit reached (%d calls in 24 h)", len(recent))
	}
	c.state.Sent = append(c.state.Sent, now)
	c.calling[aid] = &anidbCall{at: now}
	slog.Debug("anidb: calling the API", "aid", aid, "background", background, "calls_24h", len(c.state.Sent), "limit", anidbDailyLimit)
	return ""
}

// anidbNotSent: a call stopped when its turn in the 4 s queue came (see
// sending). Nothing went out, so it counts against neither AniDB's limits
// nor the anime.
type anidbNotSent struct{ why string }

func (e *anidbNotSent) Error() string { return ErrAniDBUnavailable.Error() + ": " + e.why }
func (e *anidbNotSent) Unwrap() error { return ErrAniDBUnavailable }

// sending runs when a call's turn in the 4 s queue comes, right before it
// goes out (fetch's Before). The answer just ahead of it may have brought a
// ban or a backoff: then it is stopped, and its count given back. Otherwise
// its count moves to now, so the 24 h window starts when it really went out.
func (c *AniDB) sending(aid int, background bool) (*anidbCall, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	call := c.calling[aid]
	if why := c.paused(now); why != "" {
		if call != nil && !call.sent {
			c.uncount(call)
			delete(c.calling, aid)
			c.save()
		}
		slog.Debug("anidb: call stopped before it went out", "aid", aid, "why", why)
		return nil, &anidbNotSent{why}
	}
	if call == nil || call.sent {
		// A caller that joined a call which ended before its own download
		// started: this is a new call, under the same rules.
		if why := c.admit(aid, background, now); why != "" {
			c.save()
			return nil, &anidbNotSent{why}
		}
		call = c.calling[aid]
	}
	if i := slices.IndexFunc(c.state.Sent, call.at.Equal); i >= 0 {
		c.state.Sent[i] = now
	}
	call.at, call.sent = now, true
	c.save()
	return call, nil
}

// uncount takes a call that never went out off state.Sent. Caller holds c.mu.
func (c *AniDB) uncount(call *anidbCall) {
	if i := slices.IndexFunc(c.state.Sent, call.at.Equal); i >= 0 {
		c.state.Sent = slices.Delete(c.state.Sent, i, i+1)
	}
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
// waits; other calls go on. Except for a ban, and for an error about the
// client (its name or version): those stop every call.
type anidbEntryError struct {
	msg    string
	banned bool
	client bool
}

func (e *anidbEntryError) Error() string { return "anidb: " + e.msg }

// clientID stands for this ANIDB_CLIENT and ANIDB_CLIENTVER in the state
// file (a hash: the client name is kept out of it, as out of the logs).
func (c *AniDB) clientID() string {
	sum := sha256.Sum256([]byte(c.client + "\x00" + c.clientVer))
	return hex.EncodeToString(sum[:8])
}

// result records how a call ended (called once per network request, even
// when Plex stopped waiting for it). call is what sending returned (nil:
// the call was stopped there).
func (c *AniDB) result(aid int, call *anidbCall, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if call != nil && c.calling[aid] == call {
		delete(c.calling, aid)
	}
	var notSent *anidbNotSent
	if errors.As(err, &notSent) {
		return // nothing went out: sending already gave its count back
	}
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
		if !now.Before(c.state.BannedUntil) { // calls already out when the ban came get the same answer
			slog.Error("AniDB banned this IP; AniDB API paused for 24 h (cache only)", "err", err)
		}
		c.state.BannedUntil = now.Add(anidbBanPause)
	case errors.As(err, &entry) && entry.client:
		// Not this anime's fault: every call would get the same answer.
		if c.paused(now) == "" {
			slog.Error("AniDB rejected the client: check ANIDB_CLIENT and ANIDB_CLIENTVER (the client name and version "+
				"registered at anidb.net, protocol HTTP), then restart. AniDB API paused for 24 h or until they change (cache only)",
				"err", err)
		}
		c.state.ClientRejected, c.state.ClientRejectedAt = c.clientID(), now
	case errors.As(err, &entry):
		c.state.Failed[aid] = now
		slog.Warn("AniDB has no usable entry; not asking again for 24 h", "aid", aid, "retry_anime_at", retryAnime, "err", err)
	case call != nil && call.at.Before(c.backedOffAt):
		// It went out before the failure that started this backoff (a slow
		// answer, 4 s behind it): the same outage, not one more step.
		c.state.Failed[aid] = now
		slog.Warn("AniDB call failed, like the one before it; not asking about this anime again for 24 h (AniDB's rule)",
			"aid", aid, "retry_anime_at", retryAnime, "err", err)
	default:
		c.state.Failed[aid] = now
		c.backedOffAt = now
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
