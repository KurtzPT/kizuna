package fetch

import (
	"sync"
	"time"
)

// Memo keeps values parsed from cache files, one per key, each with the
// time its file had when it was parsed: the value stands for that file only
// while the file keeps that time (every write gives it a new one). The
// caller compares the times; Memo only keeps them.
//
// Values are shared by every caller, builds running side by side included:
// read them, never change them (no append, no sort in place, no field set).
//
// Nothing is dropped when it is read. Values kept life or longer ago are
// dropped when a new one is kept, at most once per every (0: at each one),
// so a library scan doesn't keep them all. Safe for concurrent use.
type Memo[K comparable, V any] struct {
	life, every time.Duration

	mu      sync.Mutex // never held over anything but the map
	kept    map[K]memoEntry[V]
	sweptAt time.Time // the last sweep of old values
}

type memoEntry[V any] struct {
	v        V
	at, kept time.Time // the file's time; when it was kept
}

func NewMemo[K comparable, V any](life, every time.Duration) *Memo[K, V] {
	return &Memo[K, V]{life: life, every: every, kept: map[K]memoEntry[V]{}}
}

// Get returns k's value and the file time it was kept with.
func (m *Memo[K, V]) Get(k K) (v V, at time.Time, ok bool) {
	m.mu.Lock()
	e, ok := m.kept[k]
	m.mu.Unlock()
	return e.v, e.at, ok
}

// Put keeps v as k's value, parsed from the file whose time was at, in
// place of the one before. v must not be changed from now on.
func (m *Memo[K, V]) Put(k K, at time.Time, v V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if now.Sub(m.sweptAt) >= m.every {
		m.sweptAt = now
		for k, e := range m.kept {
			if now.Sub(e.kept) >= m.life {
				delete(m.kept, k)
			}
		}
	}
	m.kept[k] = memoEntry[V]{v: v, at: at, kept: now}
}

// Life returns the life and sweep interval the memo was made with.
func (m *Memo[K, V]) Life() (life, every time.Duration) { return m.life, m.every }

// Len says how many values are kept now.
func (m *Memo[K, V]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.kept)
}
