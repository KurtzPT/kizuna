package provider

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"kizuna/internal/sources"
)

// The memory's delayed save: a build that replaced a photo asks for a save,
// which runs later on a timer, or at shutdown (Flush). A build that learned
// a person's first photo saves at once (TestCrewMemoryFirstPhotoSavedInBuild).

// manualTimers stands in for time.AfterFunc: the saves it is asked for run
// only when the test fires them.
type manualTimers struct {
	mu     sync.Mutex
	timers []*manualTimer
}

type manualTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (t *manualTimer) Stop() bool {
	was := !t.stopped
	t.stopped = true
	return was
}

// manualSaves makes m's saves wait for the test (fire), not for a clock.
func manualSaves(m *crewMemory) *manualTimers {
	mt := &manualTimers{}
	m.after = func(d time.Duration, f func()) crewTimer {
		mt.mu.Lock()
		defer mt.mu.Unlock()
		t := &manualTimer{d: d, f: f}
		mt.timers = append(mt.timers, t)
		return t
	}
	return mt
}

// fire runs the saves asked for and not stopped, and returns how many timers
// were started since the last fire.
func (mt *manualTimers) fire() int {
	mt.mu.Lock()
	timers := mt.timers
	mt.timers = nil
	mt.mu.Unlock()
	for _, t := range timers {
		if !t.stopped {
			t.f()
		}
	}
	return len(timers)
}

// learnSoon learns a photo and asks for the delayed save, whether the photo
// is a first one or not: these tests are about the timer, not about which
// save a build picks.
func learnSoon(m *crewMemory, key, photo string) {
	if changed, _ := m.learn(key, photo); changed {
		m.saveSoon()
	}
}

func readPhotos(t *testing.T, dir string) map[string]string {
	t.Helper()
	var f crewMemoryJSON
	readJSON(t, filepath.Join(dir, crewMemoryFile), &f)
	return f.Photos
}

// Delayed saves asked for one after the other are one save, crewSaveDelay
// after the first, which writes every photo learned by then. The next one
// asked for is a new save.
func TestCrewMemorySavesLater(t *testing.T) {
	dir := crewDir(t)
	m := loadCrewMemory(dir)
	mt := manualSaves(m)
	learnSoon(m, "a", "a.jpg")
	learnSoon(m, "b", "b.jpg")
	learnSoon(m, "b", "b.jpg") // known already: nothing to save
	if len(mt.timers) != 1 || mt.timers[0].d != crewSaveDelay {
		t.Fatalf("timers %+v, want one of %v", mt.timers, crewSaveDelay)
	}
	if _, err := os.Stat(filepath.Join(dir, crewMemoryFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("written before the timer: %v", err)
	}
	mt.fire()
	if got := readPhotos(t, dir); len(got) != 2 || got["a"] != "a.jpg" || got["b"] != "b.jpg" {
		t.Errorf("file %v, want a and b", got)
	}
	learnSoon(m, "c", "c.jpg")
	if n := mt.fire(); n != 1 {
		t.Errorf("%d timers for a photo learned after the save, want 1", n)
	}
	if got := readPhotos(t, dir); got["c"] != "c.jpg" {
		t.Errorf("file %v, want c too", got)
	}
}

// A build saves a person's first photo before it ends, and a photo that
// replaces one a few seconds later: the file never lacks a photo Kizuna
// sent, whenever the process stops. A first photo's save takes a pending
// save's photos with it.
func TestCrewMemoryFirstPhotoSavedInBuild(t *testing.T) {
	dir := crewDir(t)
	p := &Provider{crewMemory: loadCrewMemory(dir)}
	mt := manualSaves(p.crewMemory)
	p.cfg.EpisodeCrewSources = []string{"tmdb"}
	build := func(people ...sources.Person) {
		var names []string
		for _, person := range people {
			names = append(names, person.Name)
		}
		s := &Show{Root: "tvdb-1", Episodes: map[int][]Ep{1: {{Season: 1, Index: 1, Directors: names}}}}
		var cp crewPhotos
		cp.add(srcTMDB, people)
		p.finishCrew(s, &showSources{}, &cp)
	}
	a, b := nameKey("Person A"), nameKey("Person B")

	build(sources.Person{Name: "Person A", Photo: "a1.jpg"})
	if got := readPhotos(t, dir); got[a] != "a1.jpg" {
		t.Errorf("a first photo: file %v, want it saved by the build", got)
	}
	if n := len(mt.timers); n != 0 {
		t.Errorf("a first photo: %d delayed saves, want 0", n)
	}

	build(sources.Person{Name: "Person A", Photo: "a2.jpg"})
	if got := readPhotos(t, dir); got[a] != "a1.jpg" {
		t.Errorf("a replaced photo: file %v, want a1.jpg until the timer", got)
	}
	if n := len(mt.timers); n != 1 {
		t.Fatalf("a replaced photo: %d delayed saves, want 1", n)
	}

	// The delayed save is still pending: a first photo's save writes both.
	build(sources.Person{Name: "Person A", Photo: "a2.jpg"}, sources.Person{Name: "Person B", Photo: "b1.jpg"})
	if got := readPhotos(t, dir); got[a] != "a2.jpg" || got[b] != "b1.jpg" {
		t.Errorf("a first photo beside a pending save: file %v, want a2.jpg and b1.jpg", got)
	}
	path := filepath.Join(dir, crewMemoryFile)
	const mark = `{"photos":{"left":"alone"}}`
	if err := os.WriteFile(path, []byte(mark), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := mt.fire(); n != 1 {
		t.Errorf("%d delayed saves in all, want the one", n)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != mark {
		t.Errorf("the timer wrote again after the build's save took its photos: %q %v", data, err)
	}

	// A replaced and a first photo in one build: saved by the build.
	if err := os.WriteFile(path, []byte(`{"photos":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	build(sources.Person{Name: "Person A", Photo: "a3.jpg"}, sources.Person{Name: "Person C", Photo: "c1.jpg"})
	if got := readPhotos(t, dir); got[a] != "a3.jpg" || got[nameKey("Person C")] != "c1.jpg" {
		t.Errorf("a replaced and a first photo: file %v, want both", got)
	}
	if n := len(mt.timers); n != 0 {
		t.Errorf("a replaced and a first photo: %d delayed saves, want 0", n)
	}
}

// After a save that failed, the file may lack a person the memory has: the
// next build that only replaced a photo saves before it ends, not later.
func TestCrewMemorySavesInBuildAfterFailedSave(t *testing.T) {
	dir := crewDir(t)
	p := &Provider{crewMemory: loadCrewMemory(dir)}
	mt := manualSaves(p.crewMemory)
	p.cfg.EpisodeCrewSources = []string{"tmdb"}
	build := func(photo string) {
		s := &Show{Root: "tvdb-1", Episodes: map[int][]Ep{1: {{Season: 1, Index: 1, Directors: []string{"Person A"}}}}}
		var cp crewPhotos
		cp.add(srcTMDB, []sources.Person{{Name: "Person A", Photo: photo}})
		p.finishCrew(s, &showSources{}, &cp)
	}
	// The first photo's save fails: the file's folder is a file.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	good := p.crewMemory.path
	p.crewMemory.path = filepath.Join(blocker, crewMemoryFile)
	var logs strings.Builder
	restore := captureLogs(&logs)
	build("a1.jpg")
	restore()
	if !strings.Contains(logs.String(), "crew photos file not written") {
		t.Fatalf("the save didn't fail: %q", logs.String())
	}
	p.crewMemory.path = good
	// The shutdown tries a failed save once more.
	p.Flush()
	if got := readPhotos(t, dir); got[nameKey("Person A")] != "a1.jpg" {
		t.Errorf("Flush after a failed save: file %v, want a1.jpg", got)
	}
	p.crewMemory.path = filepath.Join(blocker, crewMemoryFile)
	restore = captureLogs(&logs)
	p.crewMemory.save()
	restore()
	p.crewMemory.path = good
	build("a2.jpg")
	if got := readPhotos(t, dir); got[nameKey("Person A")] != "a2.jpg" {
		t.Errorf("after a failed save: file %v, want a2.jpg saved by the build", got)
	}
	if n := len(mt.timers); n != 0 {
		t.Errorf("after a failed save: %d delayed saves, want 0", n)
	}
	// Saved again: the next replaced photo waits for the timer.
	build("a3.jpg")
	if got := readPhotos(t, dir); got[nameKey("Person A")] != "a2.jpg" || len(mt.timers) != 1 {
		t.Errorf("after a good save: file %v and %d delayed saves, want a2.jpg and 1", got, len(mt.timers))
	}
}

// Flush writes a pending save at once and stops its timer; a timer that
// fired anyway (before Stop) then finds nothing to write. With nothing
// pending, Flush leaves the file as it is.
func TestCrewMemoryFlush(t *testing.T) {
	dir := crewDir(t)
	path := filepath.Join(dir, crewMemoryFile)
	p := &Provider{crewMemory: loadCrewMemory(dir)}
	mt := manualSaves(p.crewMemory)
	learnSoon(p.crewMemory, "a", "a.jpg")
	timer := mt.timers[0]
	p.Flush()
	if got := readPhotos(t, dir); got["a"] != "a.jpg" {
		t.Errorf("file %v after Flush, want a", got)
	}
	if !timer.stopped {
		t.Error("the pending save's timer still runs")
	}
	const mark = `{"photos":{"left":"alone"}}`
	if err := os.WriteFile(path, []byte(mark), 0o644); err != nil {
		t.Fatal(err)
	}
	timer.f() // the timer fired just before Flush stopped it
	p.Flush() // nothing pending
	if data, err := os.ReadFile(path); err != nil || string(data) != mark {
		t.Errorf("file rewritten with nothing to save: %q %v", data, err)
	}
	(&Provider{}).Flush()                               // no memory
	(&Provider{crewMemory: loadCrewMemory("")}).Flush() // kept in memory only
}

// The rules for an unreadable file (read again at the first save, kept
// aside if it still fails) hold for the delayed save: the first save is
// the timer's or Flush's, and without a photo learned the file is never
// touched.
func TestCrewMemoryDelayedUnreadable(t *testing.T) {
	t.Run("nothing learned", func(t *testing.T) {
		dir := crewDir(t)
		m := unreadableLoad(t, dir)
		manualSaves(m)
		m.flush()
		if fi, err := os.Stat(filepath.Join(dir, crewMemoryFile, "inside")); err != nil || !fi.IsDir() {
			t.Errorf("the unreadable file was touched: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, crewMemoryFile+crewMemoryAside)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("moved aside with nothing to save: %v", err)
		}
	})
	t.Run("read again", func(t *testing.T) {
		dir := crewDir(t)
		m := unreadableLoad(t, dir)
		mt := manualSaves(m)
		learnSoon(m, "new", "new.jpg")
		healFile(t, dir, map[string]string{"old": "old.jpg", "new": "file.jpg"})
		mt.fire()
		if got := readPhotos(t, dir); len(got) != 2 || got["old"] != "old.jpg" || got["new"] != "new.jpg" {
			t.Errorf("file %v, want old merged and the learned new", got)
		}
	})
	t.Run("still unreadable", func(t *testing.T) {
		dir := crewDir(t)
		m := unreadableLoad(t, dir)
		manualSaves(m)
		learnSoon(m, "a", "a.jpg")
		m.flush()
		if fi, err := os.Stat(filepath.Join(dir, crewMemoryFile+crewMemoryAside, "inside")); err != nil || !fi.IsDir() {
			t.Errorf("not kept aside: %v", err)
		}
		if got := readPhotos(t, dir); got["a"] != "a.jpg" {
			t.Errorf("file %v, want a", got)
		}
	})
}

// A panic in the timer's save is logged with its stack and doesn't end
// Kizuna: the memory still answers photo reads, and the save counts as
// failed, so the shutdown's flush writes the file. The panic here is the
// merge of a file read again after all, into photos made nil.
func TestCrewMemoryDelayedSavePanics(t *testing.T) {
	dir := crewDir(t)
	m := unreadableLoad(t, dir)
	mt := manualSaves(m)
	learnSoon(m, "a", "a.jpg")
	healFile(t, dir, map[string]string{"old": "old.jpg"})
	m.photos = nil
	var logs strings.Builder
	restore := captureLogs(&logs)
	mt.fire()
	restore()
	if !crewLogged(logs.String(), dir, "panicked") || !strings.Contains(logs.String(), "goroutine") {
		t.Errorf("the panic isn't logged with its path and stack: %q", logs.String())
	}
	if !m.mu.TryLock() {
		t.Fatal("the memory stays locked after the panic: every photo read would hang")
	}
	m.photos = map[string]string{"a": "a.jpg"}
	m.mu.Unlock()
	m.flush()
	if got := readPhotos(t, dir); len(got) != 2 || got["a"] != "a.jpg" || got["old"] != "old.jpg" {
		t.Errorf("file %v after the flush, want a and old: the panicked save must count as failed", got)
	}
	if m.photo("old") != "old.jpg" {
		t.Errorf("photo read %q, want the file's photo", m.photo("old"))
	}
}

// Builds learning photos at the same time as the timer's saves run lose
// none: once Flush returns, the file holds every photo. The saves run on
// real timers here (no delay), so the race detector sees builds and saves
// side by side; the test waits for nothing but Flush.
func TestCrewMemorySavesBesideBuilds(t *testing.T) {
	dir := crewDir(t)
	m := loadCrewMemory(dir)
	m.after = func(_ time.Duration, f func()) crewTimer { return time.AfterFunc(0, f) } //lint:allow sleep: a real timer with no delay, so no wait
	var wg sync.WaitGroup
	for b := range 4 {
		wg.Go(func() {
			for i := range 25 {
				k := strconv.Itoa(b) + "-" + strconv.Itoa(i)
				learnSoon(m, k, k+".jpg")
				_ = m.photo(k) // an episode answer reading it
			}
			m.save() // a build that learned a first photo, beside the timer's saves
		})
	}
	wg.Wait()
	m.flush()
	got := readPhotos(t, dir)
	if len(got) != 100 {
		t.Errorf("file holds %d photos, want 100", len(got))
	}
	// A timer that fires after Flush has nothing pending: it writes nothing,
	// and Flush waited for any save already under way.
	m.timerMu.Lock()
	pending := m.pending
	m.timerMu.Unlock()
	if pending {
		t.Error("a save is still pending after Flush")
	}
}
