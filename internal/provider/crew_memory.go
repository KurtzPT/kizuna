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
}

// crewMemoryJSON is the file: {"photos": {"hiroshi kamiya": "https://..."}}.
type crewMemoryJSON struct {
	Photos map[string]string `json:"photos"`
}

// loadCrewMemory reads the memory from dataDir ("": none, kept in memory
// only). A file that can't be read or decoded is a warning: the memory
// starts empty and still works, and its next save writes a good file.
func loadCrewMemory(dataDir string) *crewMemory {
	m := &crewMemory{photos: map[string]string{}}
	if dataDir == "" {
		return m
	}
	m.path = filepath.Join(dataDir, crewMemoryFile)
	data, err := os.ReadFile(m.path)
	if errors.Is(err, fs.ErrNotExist) {
		return m
	}
	var f crewMemoryJSON
	if err == nil {
		err = json.Unmarshal(data, &f)
	}
	if err != nil {
		slog.Warn("crew photos file not read; Kizuna starts remembering crew photos anew, and the next one it learns writes a new file",
			"path", m.path, "err", err)
		return m
	}
	for k, v := range f.Photos {
		if k != "" && v != "" {
			m.photos[k] = v
		}
	}
	return m
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
