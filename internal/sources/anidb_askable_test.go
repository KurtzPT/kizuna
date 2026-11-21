package sources

import (
	"context"
	"slices"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

// Askable says whether Anime would call AniDB now, by the same rules as
// allowed, without counting a call or changing the state; Anime then does
// what it said.
func TestAniDBAskable(t *testing.T) {
	const aid = 69 // anidbSample's
	var none *AniDB
	if none.Askable(aid) {
		t.Error("nil AniDB: want not askable")
	}
	dir := t.TempDir()
	if NewAniDB(fetch.New(dir), dir, "", "1").Askable(aid) {
		t.Error("no ANIDB_CLIENT: want not askable")
	}
	now := time.Now()
	recent := make([]time.Time, anidbDailyLimit)
	old := make([]time.Time, anidbDailyLimit)
	for i := range recent {
		recent[i], old[i] = now.Add(-time.Hour), now.Add(-25*time.Hour)
	}
	for name, c := range map[string]struct {
		set  func(c *AniDB)
		want bool
	}{
		"nothing against it": {func(*AniDB) {}, true},
		"backing off":        {func(c *AniDB) { c.state.RetryAt, c.state.Failures = now.Add(time.Hour), 3 }, false},
		"backoff over":       {func(c *AniDB) { c.state.RetryAt, c.state.Failures = now.Add(-time.Second), 3 }, true},
		"banned":             {func(c *AniDB) { c.state.BannedUntil = now.Add(time.Hour) }, false},
		"client rejected": {func(c *AniDB) {
			c.state.ClientRejected, c.state.ClientRejectedAt = c.clientID, now.Add(-time.Hour)
		}, false},
		"daily limit":                {func(c *AniDB) { c.state.Sent = slices.Clone(recent) }, false},
		"daily limit a day ago":      {func(c *AniDB) { c.state.Sent = slices.Clone(old) }, true},
		"this anime blocked":         {func(c *AniDB) { c.state.Failed[aid] = now.Add(-time.Hour) }, false},
		"another anime blocked":      {func(c *AniDB) { c.state.Failed[aid+1] = now.Add(-time.Hour) }, true},
		"block over":                 {func(c *AniDB) { c.state.Failed[aid] = now.Add(-anidbRetry) }, true},
		"this anime fetched lately":  {func(c *AniDB) { c.state.Fetched[aid] = now.Add(-time.Hour) }, false},
		"a call for it already runs": {func(c *AniDB) { c.calling[aid] = &anidbCall{at: now} }, true},
	} {
		a, calls := fakeAniDB(t, [2]string{"200", anidbSample})
		a.mu.Lock()
		c.set(a)
		sent := len(a.state.Sent)
		a.mu.Unlock()
		if got := a.Askable(aid); got != c.want {
			t.Errorf("%s: askable %v, want %v", name, got, c.want)
		}
		a.mu.Lock()
		if len(a.state.Sent) != sent {
			t.Errorf("%s: Askable changed the call count %d -> %d", name, sent, len(a.state.Sent))
		}
		joined := a.calling[aid] != nil
		a.mu.Unlock()
		if joined {
			continue // the call it would join is made up: nothing would answer it
		}
		// Anime does what Askable said: calls AniDB, or fails at once.
		if _, err := a.Anime(context.Background(), aid); (err == nil) != c.want || (calls.Load() == 1) != c.want {
			t.Errorf("%s: Anime err %v after %d calls, want a call: %v", name, err, calls.Load(), c.want)
		}
	}
}
