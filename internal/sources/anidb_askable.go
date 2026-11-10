package sources

import "time"

// Askable reports whether Anime could call AniDB for aid now: AniDB is set
// up, no ban, backoff or rejected client pauses it, the daily limit isn't
// reached, and aid has no call AniDB answered in the last 24 h (its block
// after an error, or its entry fetched lately). A call already queued for
// aid is joined, so that counts too. It counts nothing and changes nothing:
// a caller only uses it to tell whether Anime would fail at once (it still
// returns a cached copy, Peek's, then). The answer may change right after.
// nil = AniDB not set up: false.
func (c *AniDB) Askable(aid int) bool {
	if !c.Enabled() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.paused(now) != "" {
		return false
	}
	if c.calling[aid] != nil {
		return true
	}
	if at, ok := c.state.Failed[aid]; ok && now.Sub(at) < anidbRetry {
		return false
	}
	if at, ok := c.state.Fetched[aid]; ok && now.Sub(at) < anidbRetry {
		return false
	}
	n := 0 // admit's count, without pruning state.Sent: that is admit's to do (and save)
	for _, t := range c.state.Sent {
		if now.Sub(t) < 24*time.Hour {
			n++
		}
	}
	return n < anidbDailyLimit
}
