package provider

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"kizuna/internal/fetch"
)

// crewMemoryFile holds every episode crew photo Kizuna sent, in the data
// folder, outside cache/: the cache cleanup would take photos away.
const crewMemoryFile = "crew-photos.json"

// crewMemory is every crew photo Kizuna sent, by nameKey, for all series
// and both modes: the only place a sent photo comes from. Plex keeps one
// photo per name and role for the whole server, and the last answer wins,
// a missing photo too: a show naming a person without one would take the
// photo away everywhere. So every episode answer reads the person's photo
// here, at answer time, and a build only adds to it (the newest photo
// replaces an older one). Nothing expires; deleting the file starts over.
// Cast photos aren't kept here. A nil memory remembers nothing (tests that
// make a Provider by hand).
type crewMemory struct {
	path string // "" = not kept on disk (tests)

	mu     sync.RWMutex
	photos map[string]string // key -> photo

	saveMu sync.Mutex // one save at a time, each with the newest photos
	// keepAside: the file was there but not read. The first save reads it
	// again and merges it, or, still unreadable, moves it to
	// path+crewMemoryAside before writing: one bad read (a sharing
	// violation, a NAS error) never costs the photos it holds. Under saveMu.
	keepAside bool
}

// crewMemoryAside is the suffix of an unreadable memory file kept aside.
// One name: a later unreadable file replaces it, so copies don't pile up.
// A file is moved there only after a second read failed too (a good file
// read again is merged instead), so an older copy is rarely worth more.
const crewMemoryAside = ".unreadable"

// crewMemoryJSON is the file: {"photos": {"hiroshi kamiya": "https://..."}}.
type crewMemoryJSON struct {
	Photos map[string]string `json:"photos"`
}

// loadCrewMemory reads the memory from dataDir ("": none, kept in memory
// only). A file that can't be read or decoded is a warning: the memory
// starts empty and still works, and its first save reads the file again
// (see reread). A missing file is the first start: no warning.
func loadCrewMemory(dataDir string) *crewMemory {
	m := &crewMemory{photos: map[string]string{}}
	if dataDir == "" {
		return m
	}
	m.path = filepath.Join(dataDir, crewMemoryFile)
	photos, err := readCrewMemory(m.path)
	if errors.Is(err, fs.ErrNotExist) {
		return m
	}
	if err != nil {
		slog.Warn("crew photos file not read; Kizuna starts remembering crew photos anew, reads the file again at its first save, and keeps it aside if it still can't be read",
			"path", m.path, "aside", m.path+crewMemoryAside, "err", err)
		m.keepAside = true
		return m
	}
	m.photos = photos
	return m
}

// readCrewMemory reads and decodes the file at path, without empty keys or
// photos. A missing file is an error wrapping fs.ErrNotExist.
func readCrewMemory(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f crewMemoryJSON
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	photos := make(map[string]string, len(f.Photos))
	for k, v := range f.Photos {
		if k != "" && v != "" {
			photos[k] = v
		}
	}
	return photos, nil
}

// reread runs at the first save after a load that failed (under saveMu).
// The file is read again: a read that failed once (the transient case) adds
// its photos, so nothing is lost and nothing is moved. A photo learned
// since the start wins: it is the newer one. Still unreadable: the file is
// moved aside, so the save that follows doesn't overwrite it. ok: the save
// may go on.
func (m *crewMemory) reread() (ok bool) {
	photos, err := readCrewMemory(m.path)
	switch {
	case err == nil:
		m.mu.Lock() // saveMu, then mu, as in save: readers see the merged photos at once
		for k, v := range photos {
			if _, learned := m.photos[k]; !learned {
				m.photos[k] = v
			}
		}
		m.mu.Unlock()
		slog.Info("crew photos file read after all; its photos are remembered again", "path", m.path, "photos", len(photos))
	case errors.Is(err, fs.ErrNotExist):
		// Gone since the start: nothing left to keep.
	default:
		// fetch.Rename replaces an older kept-aside file, and waits out a
		// short sharing violation on Windows.
		aside := m.path + crewMemoryAside
		switch rerr := fetch.Rename(m.path, aside); {
		case rerr == nil:
			slog.Warn("unreadable crew photos file moved aside, and Kizuna writes a new one; to use it again, stop Kizuna, fix it and rename it to "+crewMemoryFile,
				"path", m.path, "aside", aside, "err", err)
		case !errors.Is(rerr, fs.ErrNotExist): // gone just now is fine: nothing to keep
			slog.Warn("unreadable crew photos file not moved aside, so not replaced; the photos stay remembered until a restart, and the next save tries again",
				"path", m.path, "aside", aside, "err", rerr)
			return false
		}
	}
	m.keepAside = false
	return true
}

// photo is the photo remembered for key ("" = none).
func (m *crewMemory) photo(key string) string {
	if m == nil || key == "" {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.photos[key]
}

// learn remembers photo for key, replacing an older one. changed: it wasn't
// remembered already.
func (m *crewMemory) learn(key, photo string) (changed bool) {
	if m == nil || key == "" || photo == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.photos[key] == photo {
		return false
	}
	m.photos[key] = photo
	return true
}

// save writes the memory to its file. Saves run one at a time, each taking
// the photos as they are once its turn comes, so an older snapshot never
// lands last. A failed write is a warning: the memory still works, and the
// next save tries again.
func (m *crewMemory) save() {
	if m == nil || m.path == "" {
		return
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	// A file not moved aside isn't overwritten: the save waits for the next.
	if m.keepAside && !m.reread() {
		return
	}
	m.mu.RLock()
	data, err := json.Marshal(crewMemoryJSON{Photos: m.photos}) // map keys sorted: the same photos, the same file
	m.mu.RUnlock()
	if err == nil {
		err = fetch.WriteFile(m.path, data)
	}
	if err != nil {
		slog.Warn("crew photos file not written; the photos stay remembered until a restart, and the next save tries again",
			"path", m.path, "err", err)
	}
}
