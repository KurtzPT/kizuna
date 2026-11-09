package sources

// Hooks for tests that must not overlap a source's background work: an
// allocation count or a check of what a background fetch cached.

// WaitIdle returns once no background crew fetch is under way. Safe on a
// nil TVDB (not set up).
func (t *TVDB) WaitIdle() {
	if t != nil {
		t.crewQueue.Wait()
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
