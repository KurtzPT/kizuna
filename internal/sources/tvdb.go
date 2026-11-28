package sources

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"kizuna/internal/fetch"
)

const tvdbAPI = "https://api4.thetvdb.com/v4"

// TVDB is a minimal TheTVDB client (needs your own API key). It speaks v4,
// or the legacy v3 API (tvdb3.go) with a legacy key: when that's the only
// key, or as a fallback once TheTVDB rejects the v4 key.
type TVDB struct {
	f                   *fetch.Client
	key, pin, legacyKey string // key: v4

	mu      sync.Mutex
	legacy  bool // using the legacy key (v3 API)
	token   string
	tokenAt time.Time
	// loggingIn: closed when the login under way ends (nil = none).
	loggingIn chan struct{}
	renewing  sync.WaitGroup // a background renewal under way (tests wait on it)
	// A failed login is remembered until loginRetry, so a wrong key costs
	// one request and one log line, not one per TVDB call of a library scan.
	loginErr   error
	loginRetry time.Time

	// Episode crew records (EpisodeCrew): fetched in the background, and
	// failed ones not asked again until their time.
	crewMu     sync.Mutex
	crewQueued map[int]bool
	crewFailed map[int]time.Time
	crewQueue  sync.WaitGroup // background fetches under way (tests wait on it)

	// series: extended series records decoded lately (DetailsAndSeasons and
	// SeasonOverview both read one in the same build), by series id. One
	// stands for its cache file while the file keeps the time it had when
	// read; it is dropped seriesMemoFor after that, at the next one kept, so
	// a library scan doesn't keep them all.
	series *fetch.Memo[int, *tvdbSeries]
	// pages: v4 episode list pages decoded lately, by cache key: every
	// /anidb entry of a series reads its whole list, one build after the
	// other.
	pages *fetch.Memo[string, *tvdbEpisodePage]
}

const seriesMemoFor = time.Minute

const seriesTTL = 7 * 24 * time.Hour

// tvdbSeries: what DetailsAndSeasons and seasonID read of the extended
// series record.
type tvdbSeries struct {
	FirstAired string `json:"firstAired"`
	Genres     []struct {
		Name string `json:"name"`
	} `json:"genres"`
	Companies []struct {
		Name string `json:"name"`
		Type *int   `json:"primaryCompanyType"`
	} `json:"companies"`
	Characters []struct {
		Name       string `json:"name"` // character
		PersonName string `json:"personName"`
		PeopleType string `json:"peopleType"`
		PersonImg  string `json:"personImgURL"`
		Sort       int    `json:"sort"`
	} `json:"characters"` // in sort order
	Ratings []struct {
		Name    string `json:"name"`
		Country string `json:"country"`
	} `json:"contentRatings"`
	Seasons []struct {
		ID     int `json:"id"`
		Number int `json:"number"`
		Type   struct {
			Type string `json:"type"`
		} `json:"type"`
	} `json:"seasons"`
}

// tvdbTokenLife: a v4 token is renewed after this long. TheTVDB accepts it
// a month: while a renewal fails (not a rejected key), the old one is used up
// to tvdbTokenValid. A v3 token: renewed after 20 hours, valid a day.
const (
	tvdbTokenLife   = 7 * 24 * time.Hour
	tvdbTokenValid  = 28 * 24 * time.Hour
	tvdb3TokenLife  = 20 * time.Hour
	tvdb3TokenValid = 23 * time.Hour
)

// seriesFilesTTL: how long the series text and artwork are cached.
const seriesFilesTTL = 7 * 24 * time.Hour

// seasonTTL: how long a season's text (or its lack of one) and its
// artwork are kept.
const seasonTTL = 7 * 24 * time.Hour

// tvdbLoginPause: how long a failed login is remembered when the key wasn't
// rejected: a network error may be gone in a moment. A rejected key is
// remembered rejectedKeyPause, as for the other sources.
const tvdbLoginPause = 30 * time.Second

// NewTVDB uses the v4 key when there is one, else the legacy v3 key. With
// both, the legacy key takes over (with a warning) once TheTVDB rejects the
// v4 key, until the next restart: the start-up log says so.
func NewTVDB(f *fetch.Client, key, pin, legacyKey string) *TVDB {
	f.Throttle("api4.thetvdb.com", 100*time.Millisecond)
	f.Throttle("api.thetvdb.com", 100*time.Millisecond)
	if key != "" && legacyKey != "" {
		slog.Info("TVDB_API_KEY and TVDB_LEGACY_API_KEY are both set: using the v4 key; the legacy key takes over (with a warning) if TheTVDB rejects it")
	}
	return &TVDB{f: f, key: key, pin: pin, legacyKey: legacyKey, legacy: key == "" && legacyKey != "",
		crewQueued: map[int]bool{}, crewFailed: map[int]time.Time{},
		series: fetch.NewMemo[int, *tvdbSeries](seriesMemoFor, 0), pages: fetch.NewMemo[string, *tvdbEpisodePage](sharedFor, 0)}
}

func (t *TVDB) Enabled() bool { return t != nil && (t.key != "" || t.legacyKey != "") } // nil = not set up

// API names the TheTVDB API in use: "v4", "v3" (legacy key) or "" (off).
func (t *TVDB) API() string {
	if !t.Enabled() {
		return ""
	}
	if t.isLegacy() {
		return "v3"
	}
	return "v4"
}

func (t *TVDB) isLegacy() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.legacy
}

// useLegacy says which API a call should use. With both keys, it first makes
// sure the v4 key still works (a login, normally the remembered token), so a
// rejected v4 key switches to the legacy one before the call picks its API.
// A key rejected at a renewal (which runs in the background) switches from
// the next call on.
// While a login is under way it doesn't wait: a call answered from the cache
// needs no token, and one that does waits in get, which notices a switch
// (errTVDBSwitched). Nor does it log in when the call's v4 answer is
// cached and younger than ttl (key: its cache file, "" = none): that call
// needs no token, and a login could hang on a TheTVDB that's down.
func (t *TVDB) useLegacy(ctx context.Context, key string, ttl time.Duration) bool {
	t.mu.Lock()
	check := t.key != "" && t.legacyKey != "" && !t.legacy && t.loggingIn == nil &&
		(t.token == "" || time.Since(t.tokenAt) >= tvdbTokenLife) // with a valid token, login is instant
	t.mu.Unlock()
	if check && !t.v4Fresh(key, ttl) {
		_, _, _ = t.login(ctx)
	}
	return t.isLegacy()
}

// v4Fresh: the v4 cache file key is younger than ttl (not read).
func (t *TVDB) v4Fresh(key string, ttl time.Duration) bool {
	if key == "" {
		return false
	}
	at, ok := t.f.CachedAt(cacheFolder(false) + key)
	return ok && time.Since(at) < ttl
}

func apiBase(legacy bool) string {
	if legacy {
		return tvdb3API
	}
	return tvdbAPI
}

// cacheFolder: v3 and v4 answers look different, so each API has its own.
func cacheFolder(legacy bool) string {
	if legacy {
		return "tvdb3/"
	}
	return "tvdb/"
}

// rejected: TheTVDB refused the key or PIN (400 is its answer to a missing
// PIN). Trying again won't help.
func rejected(err error) bool {
	var le *loginStatusError
	return errors.As(err, &le) &&
		(le.code == http.StatusBadRequest || le.code == http.StatusUnauthorized || le.code == http.StatusForbidden)
}

// errTVDBLogin marks login errors: they say nothing about the record a call
// asked for.
var errTVDBLogin = errors.New("tvdb login")

// loginStatusError is TheTVDB's non-200 answer to a login. It isn't a
// fetch.StatusError, so callers never read a login's 404 as TheTVDB having
// no such record, nor its 400 as a missing translation. It has no Unwrap on
// purpose: one would bring that back.
type loginStatusError struct {
	code int
	msg  string
}

func (e *loginStatusError) Error() string { return e.msg }

// login returns a bearer token and whether it's for the legacy (v3) API. A
// v4 token is valid a month (renewed weekly), a v3 token a day (renewed after
// 20 hours). A renewal runs in the background (renew) and every caller, the
// one that started it too, keeps the old token while it's valid; while a
// renewal fails without the key being rejected, the old token stays in use.
// t.mu is never held over the network: one login runs at a time; callers
// without a valid token wait for it (or for their ctx).
func (t *TVDB) login(ctx context.Context) (string, bool, error) {
	for {
		t.mu.Lock()
		life, valid := tvdbTokenLife, tvdbTokenValid
		if t.legacy {
			life, valid = tvdb3TokenLife, tvdb3TokenValid
		}
		token, legacy, retryErr := t.token, t.legacy, t.loginErr
		age := time.Since(t.tokenAt)
		switch {
		case token != "" && age < life:
			t.mu.Unlock()
			return token, legacy, nil
		case time.Now().Before(t.loginRetry):
			t.mu.Unlock()
			if token != "" && age < valid { // the renewal failed; a rejected key drops the token
				return token, legacy, nil
			}
			return "", legacy, retryErr
		}
		if running := t.loggingIn; running != nil {
			t.mu.Unlock()
			if token != "" && age < valid { // only due for renewal: don't wait for it
				return token, legacy, nil
			}
			select {
			case <-running:
				continue // its outcome is in t now
			case <-ctx.Done():
				return "", legacy, fmt.Errorf("%w: %w", errTVDBLogin, ctx.Err())
			}
		}
		done := make(chan struct{})
		t.loggingIn = done
		t.mu.Unlock()

		if token != "" && age < valid { // only due: renew in the background, keep the old token
			t.renewing.Go(func() { t.renew(legacy, done) })
			return token, legacy, nil
		}
		token, err := t.requestTokenNow(ctx, legacy, done)

		t.mu.Lock()
		t.loggingIn = nil
		close(done)
		again := t.loginDone(ctx, legacy, token, err)
		t.mu.Unlock()
		// again: the legacy key takes over. A failure set the pause: the
		// next round gives the old token, if still valid, or the error.
		if !again && (err == nil || ctx.Err() != nil) {
			return token, legacy, err
		}
	}
}

// renew is login's background renewal: it isn't tied to a build's ctx, so
// every outcome is recorded (a pause, a rejected key, the switch to the
// legacy key; the next call then logs in with it). done is t.loggingIn.
func (t *TVDB) renew(legacy bool, done chan struct{}) {
	defer t.renewPanicked(done)
	ctx, cancel := context.WithTimeout(context.Background(), tvdbRenewTimeout)
	defer cancel()
	token, err := t.requestToken(ctx, legacy)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loggingIn = nil
	close(done)
	t.loginDone(context.Background(), legacy, token, err) // a timeout counts as a failed login
}

// renewPanicked, deferred by renew, logs a panic the renewal ended with:
// nobody waits for it to raise the panic on. One before the outcome was
// recorded counts as a failed login: the login under way ends (every later
// login would wait for it), the old token stays in use, and the next
// renewal waits for the login pause.
func (t *TVDB) renewPanicked(done chan struct{}) {
	r := recover()
	if r == nil {
		return
	}
	fetch.LogPanic("tvdb: token renewal panicked", r)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loggingIn == done {
		t.loggingIn = nil
		close(done)
		t.loginErr, t.loginRetry = fmt.Errorf("%w: the renewal panicked: %v", errTVDBLogin, r), time.Now().Add(tvdbLoginPause)
	}
}

// requestTokenNow is requestToken for a login the caller waits for. done is
// t.loggingIn. A panic in it goes on to the caller (its show build fails),
// but first it counts as a failed login, as in renewPanicked: the login
// under way ends, or every later login would wait for it until its ctx
// ends. It isn't recovered here, so the caller sees the panic as it was.
func (t *TVDB) requestTokenNow(ctx context.Context, legacy bool, done chan struct{}) (token string, err error) {
	ended := false
	defer func() {
		if ended {
			return
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.loggingIn == done {
			t.loggingIn = nil
			close(done)
			t.loginErr, t.loginRetry = fmt.Errorf("%w: the login panicked", errTVDBLogin), time.Now().Add(tvdbLoginPause)
		}
	}()
	token, err = t.requestToken(ctx, legacy)
	ended = true
	return token, err
}

// tvdbRenewTimeout bounds a background renewal, like a request's HTTP timeout.
const tvdbRenewTimeout = 30 * time.Second

func (t *TVDB) requestToken(ctx context.Context, legacy bool) (string, error) {
	creds := map[string]string{"apikey": t.key, "pin": t.pin}
	if legacy {
		creds = map[string]string{"apikey": t.legacyKey}
	}
	body, _ := json.Marshal(creds)
	// NoRedirect: a 307/308 redirect would replay the POST body, credentials
	// included, to whatever host it names.
	data, err := t.f.Get(ctx, fetch.Request{URL: apiBase(legacy) + "/login", Method: "POST", Body: body, Quiet: true, NoRedirect: true})
	if err != nil {
		if se := (*fetch.StatusError)(nil); errors.As(err, &se) {
			err = &loginStatusError{code: se.Code, msg: err.Error()}
		}
		return "", fmt.Errorf("%w: %w", errTVDBLogin, err)
	}
	var r struct {
		Token string `json:"token"` // v3
		Data  struct {
			Token string `json:"token"` // v4
		} `json:"data"`
	}
	if json.Unmarshal(data, &r) == nil {
		if token := cmp.Or(r.Data.Token, r.Token); token != "" {
			return token, nil
		}
	}
	return "", fmt.Errorf("%w: no token in the answer", errTVDBLogin)
}

// loginDone records a login's outcome and reports whether to log in again
// (the v4 key was rejected: the legacy key takes over). Needs t.mu.
func (t *TVDB) loginDone(ctx context.Context, legacy bool, token string, err error) (again bool) {
	if err == nil {
		t.token, t.tokenAt, t.loginErr, t.loginRetry = token, time.Now(), nil, time.Time{}
		return false
	}
	if ctx.Err() != nil { // the caller gave up: says nothing about TVDB
		return false
	}
	if !rejected(err) {
		t.loginErr, t.loginRetry = err, time.Now().Add(tvdbLoginPause)
		slog.Warn("TVDB login failed; TVDB is skipped until the next try", "retry_in", tvdbLoginPause, "err", err)
		return false
	}
	if !legacy && t.legacyKey != "" {
		slog.Warn("TVDB rejected TVDB_API_KEY: falling back to TVDB_LEGACY_API_KEY (legacy v3 API) until the next restart; "+
			"TVDB gives no clear logos, studio or show directors and writers this way. Check TVDB_API_KEY (and TVDB_PIN for subscriber keys)",
			"err", err)
		t.legacy, t.token = true, ""
		return true // log in with the legacy key now
	}
	hint := "check TVDB_API_KEY (and TVDB_PIN for subscriber keys)"
	if legacy {
		hint = "check TVDB_LEGACY_API_KEY"
	}
	t.token, t.loginErr, t.loginRetry = "", err, time.Now().Add(rejectedKeyPause)
	slog.Error("TVDB rejected the API key: "+hint+"; TVDB is skipped until the next try",
		"legacy", legacy, "retry_in", rejectedKeyPause, "err", err)
	return false
}

// errTVDBSwitched: the client fell back to the legacy key while a v4 call
// was under way; that one call gives up.
var errTVDBSwitched = errors.New("tvdb: switched to the legacy key during the call")

// get fetches path (relative to the root of the v3 or v4 API) through the
// cache; key is the cache file name without the tvdb/ or tvdb3/ folder.
func (t *TVDB) get(ctx context.Context, legacy bool, path, key string, ttl time.Duration, out any) error {
	return t.getAs(ctx, legacy, getOpts{}, path, key, ttl, out)
}

// getOpts: how getAs asks (fetch.Request's fields of the same names).
type getOpts struct {
	quiet   bool // the caller logs the request's failures itself (fetch only logs them at debug)
	noStale bool // a failed download is the error, not answered by a stale copy
	// background: nobody waits for the answer now; it waits behind every
	// request a build waits for.
	background bool
	// notFound, notFoundTTL: the statuses that mean TheTVDB has nothing
	// there, and how long that answer stands (fetch.Request's NotFound and
	// NotFoundTTL).
	notFound    []int
	notFoundTTL time.Duration
}

// getAs is get, asked the way o says. Its twin is tvdbShared: the two must
// keep the same Enabled check and the same retry after a 401.
func (t *TVDB) getAs(ctx context.Context, legacy bool, o getOpts, path, key string, ttl time.Duration, out any) error {
	if !t.Enabled() {
		return fmt.Errorf("tvdb: no api key")
	}
	r, ready := t.request(ctx, legacy, o, path, key, ttl)
	for retried := false; ; retried = true {
		err := t.f.GetJSON(ctx, r, out, ready)
		if retried || !unauthorized(err) {
			return err
		}
		// No stale copy to answer with: try once more with a new token.
	}
}

// tvdbShared is t.get for a value its callers share and only read
// (fetch.GetShared), kept in m. Its twin is getAs: see there.
func tvdbShared[T any](ctx context.Context, t *TVDB, m *fetch.Memo[string, *T], legacy bool, path, key string, ttl time.Duration) (*T, error) {
	if !t.Enabled() {
		return nil, fmt.Errorf("tvdb: no api key")
	}
	r, ready := t.request(ctx, legacy, getOpts{}, path, key, ttl)
	for retried := false; ; retried = true {
		v, err := fetch.GetShared(ctx, t.f, m, r, ready)
		if retried || !unauthorized(err) {
			return v, err
		}
		// No stale copy to answer with: try once more with a new token.
	}
}

// request is how getAs asks for path: the request, and the ready that logs
// in once the cache has no fresh answer.
func (t *TVDB) request(ctx context.Context, legacy bool, o getOpts, path, key string, ttl time.Duration) (fetch.Request, func(fetch.Request) (fetch.Request, error)) {
	r := fetch.Request{Key: cacheFolder(legacy) + key, TTL: ttl, Quiet: o.quiet, NoStale: o.noStale, Background: o.background,
		NotFound: o.notFound, NotFoundTTL: o.notFoundTTL}
	ready := func(req fetch.Request) (fetch.Request, error) {
		token, tokenLegacy, err := t.login(ctx)
		if err != nil {
			// The expired cached copy answers, as for a failed download: old
			// data beats none. Not when the caller gave up.
			req.NoStale = req.NoStale || ctx.Err() != nil
			return req, err
		}
		if tokenLegacy != legacy {
			req.NoStale = true
			return req, errTVDBSwitched
		}
		headers := map[string]string{"Authorization": "Bearer " + token}
		if legacy {
			headers["Accept-Language"] = "en" // v3 picks the language by header
		}
		req.URL, req.Headers = apiBase(legacy)+path, headers
		// Done sees a 401 even when a stale copy answers the call.
		req.Done = func(err error) {
			if unauthorized(err) {
				t.dropToken(token)
			}
		}
		return req, nil
	}
	return r, ready
}

func unauthorized(err error) bool {
	var se *fetch.StatusError
	return errors.As(err, &se) && se.Code == http.StatusUnauthorized
}

// dropToken forgets token (expired or revoked early), so the next call logs
// in again. A newer token stays.
func (t *TVDB) dropToken(token string) {
	t.mu.Lock()
	if t.token == token {
		t.token = ""
	}
	t.mu.Unlock()
}

// Episodes returns all episodes in aired ("official") order with English
// titles, sorted by season and number.
func (t *TVDB) Episodes(ctx context.Context, id int) ([]Episode, error) {
	if t.useLegacy(ctx, episodesKey(id, 0), episodesTTL) {
		return t.episodes3(ctx, id)
	}
	var out []Episode
	err := episodePages(id, 0, func(page int) (bool, error) {
		path := fmt.Sprintf("/series/%d/episodes/official/eng?page=%d", id, page)
		r, err := tvdbShared(ctx, t, t.pages, false, path, episodesKey(id, page), episodesTTL)
		if err != nil {
			return false, err
		}
		eps := r.Data.Episodes
		if len(eps) == 0 { // the translated list nests episodes under data.series
			eps = r.Data.Series.Episodes
		}
		for _, e := range eps {
			if e.Season == nil || e.Number == nil {
				continue
			}
			out = append(out, Episode{ID: e.ID, Season: *e.Season, Number: *e.Number, Absolute: e.Absolute, Title: e.Name,
				Summary: e.Overview, Aired: e.Aired, Thumb: artURL(e.Image), Minutes: e.Runtime})
		}
		return r.Links.Next != nil && *r.Links.Next != "", nil
	})
	sortEpisodes(out)
	return out, err
}

// tvdbEpisodePage is one page of a v4 episode list, as Episodes reads it.
type tvdbEpisodePage struct {
	Data struct {
		Episodes []tvdbEpisode `json:"episodes"`
		Series   struct {
			Episodes []tvdbEpisode `json:"episodes"`
		} `json:"series"`
	} `json:"data"`
	Links struct {
		Next *string `json:"next"`
	} `json:"links"`
}

type tvdbEpisode struct {
	ID       int    `json:"id"`
	Season   *int   `json:"seasonNumber"`
	Number   *int   `json:"number"`
	Absolute int    `json:"absoluteNumber"`
	Name     string `json:"name"`
	Overview string `json:"overview"`
	Aired    string `json:"aired"`
	Image    string `json:"image"`
	Runtime  int    `json:"runtime"`
}

const episodesTTL = 2 * 24 * time.Hour

func episodesKey(id, page int) string { return fmt.Sprintf("%d/episodes-%d.json", id, page) }

// episodePages reads a paged TheTVDB episode list, at most 50 pages from
// first (v4 counts from 0, v3 from 1). read fetches one page and says whether
// there's another. The first page's error is the list's; past it a 404 just
// ends the list, and other errors leave it incomplete.
func episodePages(id, first int, read func(page int) (more bool, err error)) error {
	for page := first; page < first+50; page++ {
		more, err := read(page)
		switch {
		case err != nil && page == first:
			return err
		case fetch.IsNotFound(err): // past the last page
			return nil
		case err != nil:
			return fmt.Errorf("%w: tvdb %d page %d: %v", ErrIncomplete, id, page, err)
		case !more:
			return nil
		}
	}
	return nil
}

// Overview returns the English series name and summary. A series with no
// English text (TheTVDB answers 400 "language not found", as for seasons)
// gives "" and no error, and isn't asked again for seasonTTL.
func (t *TVDB) Overview(ctx context.Context, id int) (name, overview string, err error) {
	key := fmt.Sprintf("%d/eng.json", id)
	if t.useLegacy(ctx, key, seriesFilesTTL) {
		return t.overview3(ctx, id)
	}
	var r struct {
		Data struct {
			Name     string `json:"name"`
			Overview string `json:"overview"`
		} `json:"data"`
	}
	o := getOpts{quiet: true, notFound: only400, notFoundTTL: seasonTTL}
	err = t.getAs(ctx, false, o, fmt.Sprintf("/series/%d/translations/eng", id), key, seriesFilesTTL, &r)
	switch {
	case noLanguage(err):
		if slog.Default().Enabled(ctx, slog.LevelDebug) { // the log args aren't built otherwise
			slog.Debug("tvdb: no English series text", "series", id, "err", err)
		}
		return "", "", nil
	case err != nil && !fetch.IsNotFound(err) && worthWarning(ctx, err):
		slog.Warn("tvdb: series text not fetched", "series", id, "err", err) // quiet request: logged here
	}
	return r.Data.Name, r.Data.Overview, err
}

// SeasonOverview returns the English overview of a season in aired
// ("official") order: the season's id is in the extended series record
// (shared with DetailsAndSeasons), its text in the season's translation. Season 0
// (specials) or less (absolute order) has none. A season TheTVDB doesn't
// list or has no English text for gives "" and no error. The legacy v3 API
// has no season records, so it gives "" too.
func (t *TVDB) SeasonOverview(ctx context.Context, id, season int) (string, error) {
	if season <= 0 || t.useLegacy(ctx, seriesKey(id), seriesTTL) {
		return "", nil
	}
	sid, err := t.seasonID(ctx, id, season)
	if sid == 0 {
		return "", err
	}
	var r struct {
		Data struct {
			Overview string `json:"overview"`
		} `json:"data"`
	}
	path, key := fmt.Sprintf("/seasons/%d/translations/eng", sid), fmt.Sprintf("%d/season-%d-eng.json", id, sid)
	o := getOpts{quiet: true, notFound: status400404, notFoundTTL: seasonTTL}
	err = t.getAs(ctx, false, o, path, key, seasonTTL, &r)
	switch {
	case noTranslation(err):
		if slog.Default().Enabled(ctx, slog.LevelDebug) { // the log args aren't built otherwise
			slog.Debug("tvdb: no English season text", "series", id, "season", season, "err", err)
		}
		return "", nil
	case err != nil && worthWarning(ctx, err):
		slog.Warn("tvdb: season text not fetched", "series", id, "season", season, "err", err) // quiet request: logged here
	}
	return r.Data.Overview, err
}

// worthWarning: a failed quiet request the caller logs itself. A failed
// login is logged by login; a caller that gave up or a switch to the legacy
// key says nothing about the record.
func worthWarning(ctx context.Context, err error) bool {
	return ctx.Err() == nil && !errors.Is(err, errTVDBLogin) && !errors.Is(err, errTVDBSwitched)
}

// seasonID finds a season's aired-order id in the extended series record
// (0 if it isn't listed).
func (t *TVDB) seasonID(ctx context.Context, id, season int) (int, error) {
	r, err := t.seriesRecord(ctx, id)
	if err != nil {
		return 0, err
	}
	for _, s := range r.Seasons {
		if s.Number == season && s.Type.Type == "official" {
			return s.ID, nil
		}
	}
	return 0, nil
}

// noTranslation: TheTVDB has no English text for the season. Its docs answer
// a missing language with 400 ("language not found"), a missing season with
// 404.
func noTranslation(err error) bool {
	return noLanguage(err) || fetch.IsNotFound(err)
}

// noLanguage: TheTVDB answered 400 ("language not found") for a
// translation.
func noLanguage(err error) bool {
	var se *fetch.StatusError
	return errors.As(err, &se) && se.Code == http.StatusBadRequest
}

// DetailsAndSeasons returns genres, people, studio, content rating and first
// air date from the extended series record, and the ids of the series'
// aired-order ("official") seasons by number (SeasonArt's), from the same
// record. The legacy v3 API has no season ids (nil): its season art goes by
// number.
func (t *TVDB) DetailsAndSeasons(ctx context.Context, id int) (Details, map[int]int, error) {
	if t.useLegacy(ctx, seriesKey(id), seriesTTL) {
		d, err := t.details3(ctx, id)
		return d, nil, err
	}
	r, err := t.seriesRecord(ctx, id)
	if err != nil {
		return Details{}, nil, err
	}
	var ids map[int]int // nil: no season listed
	for _, s := range r.Seasons {
		if s.Type.Type == "official" {
			if ids == nil {
				ids = map[int]int{}
			}
			ids[s.Number] = s.ID
		}
	}
	return seriesDetails(r), ids, nil
}

func seriesDetails(r *tvdbSeries) Details {
	d := Details{Date: r.FirstAired}
	for _, g := range r.Genres {
		d.Genres = append(d.Genres, g.Name)
	}
	for _, want := range []int{2, 3} { // TheTVDB company types: 2 studio, 3 production company
		for _, c := range r.Companies {
			if d.Studio == "" && c.Type != nil && *c.Type == want {
				d.Studio = c.Name
			}
		}
	}
	for _, c := range r.Characters {
		switch c.PeopleType {
		case "Actor":
			d.Cast = append(d.Cast, Person{Name: c.PersonName, Role: c.Name, Photo: artURL(c.PersonImg)})
		case "Director":
			d.Directors = appendNew(d.Directors, c.PersonName)
		case "Writer":
			d.Writers = appendNew(d.Writers, c.PersonName)
		}
		if crewType(c.PeopleType) {
			d.CrewPhotos = crewPhoto(d.CrewPhotos, c.PersonName, artURL(c.PersonImg))
		}
	}
	for _, c := range r.Ratings {
		d.addRating(alpha2(c.Country), c.Name)
	}
	return d
}

func seriesKey(id int) string { return fmt.Sprintf("%d/extended.json", id) }

// seriesRecord returns the extended series record, decoded once while its
// cache file stays the same (TVDB.series). It is shared: read it, don't change it.
func (t *TVDB) seriesRecord(ctx context.Context, id int) (*tvdbSeries, error) {
	key := seriesKey(id)
	if rec, keptAt, ok := t.series.Get(id); ok {
		start := time.Now()
		if at, cached := t.f.CachedAt(cacheFolder(false) + key); cached && at.Equal(keptAt) && time.Since(at) < seriesTTL {
			fetch.Observe(ctx, cacheFolder(false)+key, fetch.KindCache, start, nil)
			return rec, nil
		}
	}
	var r struct {
		Data tvdbSeries `json:"data"`
	}
	if err := t.get(ctx, false, fmt.Sprintf("/series/%d/extended", id), key, seriesTTL, &r); err != nil {
		return nil, err
	}
	people := r.Data.Characters
	sort.SliceStable(people, func(i, j int) bool { return people[i].Sort < people[j].Sort })
	// The file's time now is that of the bytes just read: downloads of one
	// key don't overlap, and a stale copy answers only for a failed one.
	if at, cached := t.f.CachedAt(cacheFolder(false) + key); cached {
		t.series.Put(id, at, &r.Data)
	}
	return &r.Data, nil
}

// tvdbArtwork is one image of an artwork list: a series' (ArtworkExtendedRecord)
// or a season's (ArtworkBaseRecord). type is an id of /artwork/types.
type tvdbArtwork struct {
	Type  int    `json:"type"`
	Image string `json:"image"`
	Score int    `json:"score"`
	Lang  string `json:"language"`
}

// byScore sorts artwork English, Japanese and no-language first (like
// TMDB's en,ja,null), then the other languages; highest score first in each.
// TheTVDB's language codes are ISO 639-2 ("eng", "jpn"); text-free art has
// null.
func byScore(arts []tvdbArtwork) {
	other := func(w tvdbArtwork) bool { return w.Lang != "" && w.Lang != "eng" && w.Lang != "jpn" }
	sort.SliceStable(arts, func(i, j int) bool {
		if oi, oj := other(arts[i]), other(arts[j]); oi != oj {
			return oj
		}
		return arts[i].Score > arts[j].Score
	})
}

// Art returns posters, backgrounds and clear logos, English, Japanese and
// no-language first, highest score first.
func (t *TVDB) Art(ctx context.Context, id int) (Art, error) {
	key := fmt.Sprintf("%d/artworks.json", id)
	if t.useLegacy(ctx, key, seriesFilesTTL) {
		return t.art3(ctx, id)
	}
	var r struct {
		Data struct {
			Artworks []tvdbArtwork `json:"artworks"`
		} `json:"data"`
	}
	if err := t.get(ctx, false, fmt.Sprintf("/series/%d/artworks", id), key, seriesFilesTTL, &r); err != nil {
		return Art{}, err
	}
	return artOf(r.Data.Artworks, 2, 3, 23), nil // TheTVDB series artwork types
}

// artOf ranks arts (byScore) and sorts them by TheTVDB artwork type id:
// posters, backgrounds and, with a logo type (0: none), the clear logos in
// English or no language.
func artOf(arts []tvdbArtwork, poster, background, logo int) Art {
	byScore(arts)
	var a Art
	for _, w := range arts {
		switch {
		case w.Type == poster:
			a.Posters = append(a.Posters, artURL(w.Image))
		case w.Type == background:
			a.Backgrounds = append(a.Backgrounds, artURL(w.Image))
		case logo != 0 && w.Type == logo && (w.Lang == "" || w.Lang == "eng"):
			a.Logos = append(a.Logos, artURL(w.Image))
		}
	}
	return a
}

// SeasonArt returns the posters and backgrounds of a season in aired
// ("official") order, ranked like Art: the artwork of its season record
// (/seasons/{id}/extended). sid is the season's id, from DetailsAndSeasons
// (0: TheTVDB doesn't list the season, so it has none). TheTVDB seasons have
// posters, backgrounds, banners and icons, no clear logos; Plex has no image
// type for banners. The legacy v3 API goes by the season number, and has
// season posters only.
func (t *TVDB) SeasonArt(ctx context.Context, id, season, sid int) (Art, error) {
	if season < 0 {
		return Art{}, nil
	}
	key := fmt.Sprintf("%d/season-%d-extended.json", id, sid)
	if t.useLegacy(ctx, key, seasonTTL) {
		return t.seasonArt3(ctx, id, season)
	}
	if sid == 0 {
		return Art{}, nil
	}
	var r struct {
		Data struct {
			Artwork []tvdbArtwork `json:"artwork"`
		} `json:"data"`
	}
	if err := t.get(ctx, false, fmt.Sprintf("/seasons/%d/extended", sid), key, seasonTTL, &r); err != nil {
		return Art{}, err
	}
	return artOf(r.Data.Artwork, 7, 8, 0), nil // TheTVDB season artwork types
}

func artURL(p string) string {
	switch {
	case p == "" || strings.HasPrefix(p, "http"):
		return p
	case strings.HasPrefix(p, "/"):
		return "https://artworks.thetvdb.com" + p
	default:
		return "https://artworks.thetvdb.com/banners/" + p
	}
}

func sortEpisodes(eps []Episode) {
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].Season != eps[j].Season {
			// specials last, so absolute numbering walks the regular seasons
			if eps[i].Season == 0 || eps[j].Season == 0 {
				return eps[j].Season == 0
			}
			return eps[i].Season < eps[j].Season
		}
		return eps[i].Number < eps[j].Number
	})
}

// alpha2 converts TheTVDB's ISO 3166-1 alpha-3 country codes ("usa") to the
// alpha-2 codes RATING_COUNTRY uses ("US"). Unknown codes stay as they are.
func alpha2(c string) string {
	if a, ok := alpha3to2[strings.ToLower(c)]; ok {
		return a
	}
	return strings.ToUpper(c)
}

var alpha3to2 = map[string]string{
	"usa": "US", "can": "CA", "mex": "MX", "bra": "BR", "arg": "AR", "chl": "CL", "col": "CO", "per": "PE",
	"gbr": "GB", "irl": "IE", "fra": "FR", "deu": "DE", "aut": "AT", "che": "CH", "ita": "IT", "esp": "ES",
	"prt": "PT", "nld": "NL", "bel": "BE", "lux": "LU", "dnk": "DK", "nor": "NO", "swe": "SE", "fin": "FI",
	"isl": "IS", "pol": "PL", "cze": "CZ", "svk": "SK", "hun": "HU", "rou": "RO", "bgr": "BG", "grc": "GR",
	"tur": "TR", "rus": "RU", "ukr": "UA", "hrv": "HR", "srb": "RS", "svn": "SI", "est": "EE", "lva": "LV",
	"ltu": "LT", "isr": "IL", "are": "AE", "sau": "SA", "egy": "EG", "zaf": "ZA", "ind": "IN", "jpn": "JP",
	"kor": "KR", "chn": "CN", "twn": "TW", "hkg": "HK", "sgp": "SG", "mys": "MY", "tha": "TH", "vnm": "VN",
	"phl": "PH", "idn": "ID", "aus": "AU", "nzl": "NZ",
}
