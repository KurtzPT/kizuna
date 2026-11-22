package fetch

import (
	"context"
	"net/http"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

// age moves k's keep time back by d, as if it was kept d earlier.
func (m *Memo[K, V]) age(k K, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.kept[k]
	e.kept = e.kept.Add(-d)
	m.kept[k] = e
}

// A value comes back with the file time it was kept with; a new one for
// the same key replaces it.
func TestMemoGetPut(t *testing.T) {
	m := NewMemo[string, *int](time.Minute, 0)
	if _, _, ok := m.Get("a"); ok {
		t.Fatal("empty memo: found a value")
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	one, two := new(int), new(int)
	m.Put("a", at, one)
	if v, got, ok := m.Get("a"); !ok || v != one || !got.Equal(at) {
		t.Fatalf("got %v %v %v, want the value and its file time", v, got, ok)
	}
	m.Put("a", at.Add(-time.Minute), two)
	if v, got, _ := m.Get("a"); v != two || !got.Equal(at.Add(-time.Minute)) || m.Len() != 1 {
		t.Errorf("after a second Put: %v %v, %d kept; want the new one only", v, got, m.Len())
	}
}

// Values kept life or longer ago go when the next one is kept, not when
// read; with every set, at most once per every.
func TestMemoSweep(t *testing.T) {
	m := NewMemo[int, int](time.Minute, 0)
	m.Put(1, time.Time{}, 1)
	m.Put(2, time.Time{}, 2)
	m.age(1, time.Minute) // 2 stays young
	if _, _, ok := m.Get(1); !ok {
		t.Fatal("an old value went when read: want it kept until the next Put")
	}
	m.Put(3, time.Time{}, 3)
	if _, _, ok := m.Get(1); ok || m.Len() != 2 {
		t.Errorf("after Put: %d kept, want the old one gone and 2 left", m.Len())
	}

	m = NewMemo[int, int](time.Minute, 5*time.Minute)
	m.Put(1, time.Time{}, 1) // the first Put sweeps: the sweep's clock starts here
	m.age(1, time.Hour)
	m.Put(2, time.Time{}, 2)
	if m.Len() != 2 {
		t.Errorf("swept again within every: %d kept, want 2", m.Len())
	}
	m.mu.Lock()
	m.sweptAt = m.sweptAt.Add(-5 * time.Minute)
	m.mu.Unlock()
	m.Put(3, time.Time{}, 3)
	if _, _, ok := m.Get(1); ok || m.Len() != 2 {
		t.Errorf("every past: %d kept, want the old one gone", m.Len())
	}
}

// Readers and writers side by side (run with -count=20; -race in CI).
func TestMemoConcurrent(t *testing.T) {
	m := NewMemo[int, *[2]int](0, 0) // every Put sweeps everything before it
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Go(func() {
			for i := range 200 {
				k := i % 7
				v := &[2]int{k, w}
				m.Put(k, time.Unix(int64(i), 0), v)
				if got, _, ok := m.Get(k); ok && got[0] != k {
					t.Errorf("key %d: value of key %d", k, got[0])
				}
				_ = m.Len()
			}
		})
	}
	wg.Wait()
}

// A kept value never answers for a copy the cache would not answer with:
// once the file is past its TTL it is downloaded as GetJSON would, and when
// that fails the stale copy answers, decoded and reported as stale. Neither
// is kept.
func TestGetSharedExpired(t *testing.T) {
	c := New(t.TempDir())
	net := &bodyNet{body: `{"A": 1}`}
	c.SetTransport(net)
	m := NewMemo[string, *counted](time.Minute, 0)
	r := Request{URL: "https://api.example/e", Key: "e.json", TTL: time.Hour}
	for range 2 { // downloaded, then read from the cache and kept
		if _, err := GetShared(context.Background(), c, m, r, nil); err != nil {
			t.Fatal(err)
		}
	}
	kept, _, _ := m.Get(r.Key)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(c.path(r.Key), old, old); err != nil {
		t.Fatal(err)
	}
	m.Put(r.Key, old, kept) // kept with the expired file's time: still no answer

	net.body = `{"A": 2}`
	if v, err := GetShared(context.Background(), c, m, r, nil); err != nil || v.A != 2 || net.sent.Load() != 2 {
		t.Fatalf("expired: %+v %v, %d requests; want the download", v, err, net.sent.Load())
	}
	if _, at, _ := m.Get(r.Key); !at.Equal(old) {
		t.Error("the download was kept")
	}

	expire(t, c, r.Key)
	net.status = http.StatusInternalServerError
	ctx, events := traced()
	decodes.Store(0)
	v, err := GetShared(ctx, c, m, r, nil)
	if err != nil || v.A != 2 || decodes.Load() != 1 {
		t.Fatalf("source down: %+v %v, decoded %d times; want the stale copy, decoded", v, err, decodes.Load())
	}
	if got := kinds(events()); !slices.Equal(got, []Kind{KindStale}) {
		t.Errorf("trace %v, want stale", got)
	}
	if _, at, _ := m.Get(r.Key); !at.Equal(old) {
		t.Error("the stale copy was kept")
	}
}

// Callers side by side get the file's value, kept or decoded (run with
// -count=20; -race in CI).
func TestGetSharedConcurrent(t *testing.T) {
	c := New(t.TempDir())
	c.SetTransport(&bodyNet{body: `{"A": 5}`})
	m := NewMemo[string, *counted](time.Minute, 0)
	r := Request{URL: "https://api.example/c", Key: "c.json", TTL: time.Hour}
	if _, err := GetShared(context.Background(), c, m, r, nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 50 {
				if v, err := GetShared(context.Background(), c, m, r, nil); err != nil || v.A != 5 {
					t.Errorf("%+v %v, want A 5", v, err)
				}
			}
		})
	}
	wg.Wait()
	if m.Len() != 1 {
		t.Errorf("%d kept, want 1", m.Len())
	}
}

func TestMemoLife(t *testing.T) {
	if life, every := NewMemo[int, int](time.Hour, time.Minute).Life(); life != time.Hour || every != time.Minute {
		t.Errorf("life %v, every %v; want 1h, 1m", life, every)
	}
}
