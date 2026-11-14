package sources

import "time"

// Hooks for tests that must not overlap a source's background work: an
// allocation count or a check of what a background fetch cached.

// WaitIdle returns once no background crew fetch or token renewal is under
// way. Safe on a nil TVDB (not set up).
func (t *TVDB) WaitIdle() {
	if t != nil {
		t.crewQueue.Wait()
		t.renewing.Wait()
	}
}

// WaitIdle returns once no background fetch (Prefetch) is waiting or
// running. Safe on a nil AniDB (not set up).
//
// A prefetch worker leaves only when nothing is waiting, after its last
// fetch is done, so once every worker has left the AniDB is idle.
func (c *AniDB) WaitIdle() {
	if c != nil {
		c.working.Wait()
	}
}

// SetCrewBudgetForTest sets how long a show build fetches TheTVDB's episode
// records itself (crewBudget), for tests in other packages: 0 leaves every
// record not cached yet to the background, so the build's crew is pending.
// It returns a func that restores the old budget. Not for parallel tests.
func SetCrewBudgetForTest(d time.Duration) (restore func()) {
	old := crewBudget
	crewBudget = d
	return func() { crewBudget = old }
}
