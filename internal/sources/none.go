package sources

import (
	"net/http"
	"time"

	"kizuna/internal/fetch"
)

// What several sources share about answers that say "nothing there": a
// status remembered on disk (fetch.Request's NotFound), and an empty
// answer asked again sooner than a full one.

// Status lists for fetch.Request.NotFound (TheTVDB's and TheMovieDb's), made
// once: a list built per call would be allocated per call.
var (
	only404      = []int{http.StatusNotFound}
	only400      = []int{http.StatusBadRequest}
	status400404 = []int{http.StatusBadRequest, http.StatusNotFound}
)

// noneTTL: how long a 404 for a list that may fill up later (a legacy
// TheTVDB image or actor list, a TheMovieDb season) stands. A day, not the
// week such a list is kept: new art or a new season shows up soon.
const noneTTL = 24 * time.Hour

// askNoneAgain says whether an empty answer just read from the cache file
// key (kept ttl) is old enough to be asked again, with the shorter life
// none. Only when the cache answered: a copy ttl old or more was just asked
// for (and answers stale when that failed), and one just downloaded is
// new, so asking again would only repeat the request.
func askNoneAgain(f *fetch.Client, key string, none, ttl time.Duration) bool {
	at, ok := f.CachedAt(key)
	if !ok {
		return false
	}
	age := time.Since(at)
	return age >= none && age < ttl
}
