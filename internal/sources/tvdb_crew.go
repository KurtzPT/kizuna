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
			PersonImg  string `json:"personImgURL"`
			Sort       int    `json:"sort"`
		} `json:"characters"`
	} `json:"data"`
}

func (r episodeRecord) crew() Crew {
	people := r.Data.Characters
	sort.SliceStable(people, func(i, j int) bool { return people[i].Sort < people[j].Sort })
	var c Crew
	var prod []string
	for _, p := range people {
		if p.PersonName = strings.TrimSpace(p.PersonName); p.PersonName == "" {
			continue
		}
		switch p.PeopleType {
		case "Director":
			c.Directors = appendNew(c.Directors, p.PersonName)
		case "Writer":
			c.Writers = appendNew(c.Writers, p.PersonName)
		case "Producer", "Executive Producer": // co-, associate and line producers left out
			prod = append(prod, p.PersonName)
		}
		if crewType(p.PeopleType) {
			c.Photos = crewPhoto(c.Photos, p.PersonName, artURL(p.PersonImg))
		}
	}
	c.Producers = producers(prod)
	return c
}

// crewType: TheTVDB's people types Kizuna sends on episodes (directors,
// writers, producers), whose photos are crew photos. Actors and guest stars
// are the cast.
func crewType(t string) bool {
	switch t {
	case "Director", "Writer", "Producer", "Executive Producer":
		return true
	}
	return false
}

// EpisodeCrew returns the directors, writers, producers and crew photos of
// TheTVDB v4 episodes, by episode id. v4 has them only in one record per
// episode: cached records are
// used (stale ones refreshed in the background), missing ones are fetched for
// up to crewBudget and the rest in the background (queueCrew).
// pending: some records are still missing and worth asking for again, so
// the caller should rebuild soon. They are on their way in the background,
// or their request failed without an answer from TheTVDB (a network error,
// a failed login): the next build asks for those again. A record TheTVDB
// itself answered with an error (crewAnswer) isn't pending.
// With the legacy key it returns nothing: v3 episode lists carry the crew.
func (t *TVDB) EpisodeCrew(ctx context.Context, ids []int) (crew map[int]Crew, pending bool) {
	if len(ids) == 0 || !t.Enabled() {
		return nil, false
	}
	wasV4 := !t.isLegacy()
	if t.useLegacy(ctx, "", 0) { // several records: no one cache file to go by
		// The v4 key was rejected just now: this build's episodes may be v4's
		// cached ones, without crew. pending: the rebuild takes v3's lists.
		return nil, wasV4
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
		wg  sync.WaitGroup
		res crewRun
	)
	jobs := make(chan int)
	for range crewWorkers {
		wg.Go(func() {
			for id := range jobs {
				t.crewJob(bctx, id, crew, &res)
			}
		})
	}
	for _, id := range missing {
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	res.panics.raise() // a record's panic fails the build, not the process
	if len(res.left) > 0 {
		pending = true
		slog.Debug("tvdb: episode records left for the background", "count", len(res.left), "of", len(missing))
	}
	t.queueCrew(append(res.left, stale...))
	return crew, pending || res.failed
}

// crewRun is what EpisodeCrew's workers report, in one variable: each one
// they share is an allocation of the build. mu guards the rest, and the
// crew map the workers fill.
type crewRun struct {
	mu     sync.Mutex
	left   []int  // not fetched within the budget: the background queue takes them
	failed bool   // a request failed without an answer from TheTVDB
	panics caught // a record's panic, raised again by EpisodeCrew once the workers are done
}

// crewJob fetches one record for EpisodeCrew's workers and notes how it
// went in crew and res. A panic ends only this record: the worker goes on
// taking records, so the sender is never left waiting for one, and mu is
// never left held for the others.
func (t *TVDB) crewJob(ctx context.Context, id int, crew map[int]Crew, res *crewRun) {
	defer res.panics.keep()
	if ctx.Err() != nil { // don't start downloads nobody waits for: the background queue takes them
		res.mu.Lock()
		res.left = append(res.left, id)
		res.mu.Unlock()
		return
	}
	c, err := t.fetchCrew(ctx, id, false)
	res.mu.Lock()
	defer res.mu.Unlock()
	switch {
	case err == nil:
		crew[id] = c
	case ctx.Err() != nil:
		res.left = append(res.left, id)
	case crewAnswer(err):
		t.crewFail(id, err)
	default:
		// A login or network blip, or an answer that can't be read: not
		// TheTVDB's answer about the record. Pending, so the next build comes
		// soon and asks again. Not queued for the background: that would ask
		// twice per build while TheTVDB is down.
		res.failed = true
		slog.Debug("tvdb: episode record not fetched", "episode", id, "err", err)
	}
}

// fetchCrew gets an episode record. background: a fetch nobody waits for.
// It gives way to the requests builds wait for, and its failure must show (a
// stale copy would hide it, and the record would be asked again at every
// build).
func (t *TVDB) fetchCrew(ctx context.Context, id int, background bool) (Crew, error) {
	var r episodeRecord
	err := t.getAs(ctx, false, getOpts{noStale: background, background: background}, fmt.Sprintf("/episodes/%d/extended", id), crewKey(id), crewTTL, &r)
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

// queueCrew fetches episode records in the background, so they never hold
// up a show build for long: each call's records one after the other, in a
// goroutine of its own (several builds' batches run side by side). As
// background requests they wait behind every request a build waits for, on
// TheTVDB's host throttle. A record already queued isn't queued again; one
// that failed isn't asked again before its time (a stale cached copy keeps
// answering), and failures past their time are forgotten here.
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
		if _, failed := t.crewFailed[id]; !failed && !t.crewQueued[id] {
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
			t.backgroundCrew(id)
		}
	})
}

// backgroundCrew fetches one record for queueCrew. Nobody waits for it to
// raise a panic on, so a panic is logged and ends only this record: it
// leaves the queue all the same (a later build may queue it again), and the
// batch goes on with the next one.
func (t *TVDB) backgroundCrew(id int) {
	defer func() {
		if r := recover(); r != nil {
			logPanic("tvdb: background episode record panicked", r, "episode", id)
		}
		t.crewMu.Lock()
		delete(t.crewQueued, id)
		t.crewMu.Unlock()
	}()
	if _, err := t.fetchCrew(context.Background(), id, true); crewAnswer(err) {
		t.crewFail(id, err)
	}
}
