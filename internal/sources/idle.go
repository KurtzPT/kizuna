package sources

import "time"

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
// The prefetch workers keep their state under c.mu and signal nothing when
// they finish, so this checks that state every few milliseconds: a test
// hook, never called while serving.
func (c *AniDB) WaitIdle() {
	if c == nil {
		return
	}
	for !c.idle() {
		time.Sleep(5 * time.Millisecond)
	}
}

func (c *AniDB) idle() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queued) == 0 && len(c.pending) == 0 && c.workers == 0
}
