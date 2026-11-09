package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"kizuna/internal/fetch"
)

// Crew is one episode's directors and writers.
type Crew struct{ Directors, Writers []string }

const (
	crewTTL     = 7 * 24 * time.Hour
	crewRetry   = time.Hour // a record that failed isn't asked again for this long
	crewWorkers = 4         // the host throttle sets the pace; this just hides latency
)

// crewBudget: how long a show build fetches episode records itself; the rest
// are fetched in the background, for the next build. A var for tests.
var crewBudget = 8 * time.Second

func crewKey(id int) string { return fmt.Sprintf("episodes/%d.json", id) }

// episodeRecord is the part of a v4 extended episode record kept here.
type episodeRecord struct {
	Data struct {
		Characters []struct {
			PersonName string `json:"personName"`
			PeopleType string `json:"peopleType"`
			Sort       int    `json:"sort"`
		} `json:"characters"`
	} `json:"data"`
}

func (r episodeRecord) crew() Crew {
	people := r.Data.Characters
	sort.SliceStable(people, func(i, j int) bool { return people[i].Sort < people[j].Sort })
	var c Crew
	for _, p := range people {
		if p.PersonName = strings.TrimSpace(p.PersonName); p.PersonName == "" {
			continue
		}
		switch p.PeopleType {
		case "Director":
			c.Directors = appendNew(c.Directors, p.PersonName)
		case "Writer":
			c.Writers = appendNew(c.Writers, p.PersonName)
		}
	}
	return c
}

// EpisodeCrew returns the directors and writers of TheTVDB v4 episodes, by
// episode id. v4 has them only in one record per episode: cached records are
// used (stale ones refreshed in the background), missing ones are fetched for
// up to crewBudget and the rest in the background, one at a time.
// pending: some are still on their way, so the caller should rebuild soon.
// With the legacy key it returns nothing: v3 episode lists carry the crew.
func (t *TVDB) EpisodeCrew(ctx context.Context, ids []int) (crew map[int]Crew, pending bool) {
	if len(ids) == 0 || !t.Enabled() || t.useLegacy(ctx) {
		return nil, false
	}
	crew = map[int]Crew{}
	var missing, stale []int
	seen := map[int]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		if data, age, ok := t.f.CachedCtx(ctx, cacheFolder(false)+crewKey(id)); ok {
			var r episodeRecord
			if json.Unmarshal(data, &r) == nil {
				crew[id] = r.crew()
				if age >= crewTTL {
					stale = append(stale, id)
				}
				continue
			}
		}
		t.crewMu.Lock()
		queued, failed := t.crewQueued[id], time.Now().Before(t.crewFailed[id])
		t.crewMu.Unlock()
		switch {
		case queued:
			pending = true
		case !failed:
			missing = append(missing, id)
		}
	}

	bctx, cancel := context.WithTimeout(ctx, crewBudget)
	defer cancel()
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		left []int
	)
	jobs := make(chan int)
	for range crewWorkers {
		wg.Go(func() {
			for id := range jobs {
				if bctx.Err() != nil { // don't start downloads nobody waits for: the background queue takes them
					mu.Lock()
					left = append(left, id)
					mu.Unlock()
					continue
				}
				c, err := t.fetchCrew(bctx, id)
				mu.Lock()
				switch {
				case err == nil:
					crew[id] = c
				case bctx.Err() != nil:
					left = append(left, id)
				case crewAnswer(err):
					t.crewFail(id, err)
				default: // a login or network blip: the next build asks again
					slog.Debug("tvdb: episode record not fetched", "episode", id, "err", err)
				}
				mu.Unlock()
			}
		})
	}
	for _, id := range missing {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	if len(left) > 0 {
		pending = true
		slog.Debug("tvdb: episode records left for the background", "count", len(left), "of", len(missing))
	}
	t.queueCrew(append(left, stale...))
	return crew, pending
}

func (t *TVDB) fetchCrew(ctx context.Context, id int) (Crew, error) {
	var r episodeRecord
	err := t.get(ctx, false, fmt.Sprintf("/episodes/%d/extended", id), crewKey(id), crewTTL, &r)
	return r.crew(), err
}

// crewAnswer: TheTVDB answered the request for the record itself with an
// error (404, 500...), so it isn't asked again for a while. A failed login,
// a switch to the legacy key or a network error says nothing about the record.
func crewAnswer(err error) bool {
	var se *fetch.StatusError
	return errors.As(err, &se) && !errors.Is(err, errTVDBLogin)
}

func (t *TVDB) crewFail(id int, err error) {
	t.crewMu.Lock()
	t.crewFailed[id] = time.Now().Add(crewRetry)
	t.crewMu.Unlock()
	slog.Debug("tvdb: episode record not available", "episode", id, "retry_in", crewRetry, "err", err)
}

// queueCrew fetches episode records in the background, one at a time, so
// they never hold up a show build for long. Failures past their time are
// forgotten here.
func (t *TVDB) queueCrew(ids []int) {
	t.crewMu.Lock()
	now := time.Now()
	for id, until := range t.crewFailed {
		if !now.Before(until) {
			delete(t.crewFailed, id)
		}
	}
	var todo []int
	for _, id := range ids {
		if !t.crewQueued[id] {
			t.crewQueued[id] = true
			todo = append(todo, id)
		}
	}
	t.crewMu.Unlock()
	if len(todo) == 0 {
		return
	}
	t.crewQueue.Go(func() {
		for _, id := range todo {
			if _, err := t.fetchCrew(context.Background(), id); crewAnswer(err) {
				t.crewFail(id, err)
			}
			t.crewMu.Lock()
			delete(t.crewQueued, id)
			t.crewMu.Unlock()
		}
	})
}
