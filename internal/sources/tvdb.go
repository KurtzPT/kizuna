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

	// noEnglish, noEnglishSeries: season and series ids TheTVDB has no
	// English text for, and until when that answer stands (seasonTTL), so
	// they aren't asked on every build.
	seasonMu        sync.Mutex
	noEnglish       map[int]time.Time
	noEnglishSeries map[int]time.Time

	// series: extended series records decoded lately (Details and
	// SeasonOverview both read one in the same build), by series id.
	seriesMu sync.Mutex
	series   map[int]seriesMemo
}

// seriesMemo is one decoded extended series record. It stands for its cache
// file while the file keeps the time it had when read; it is dropped
// seriesMemoFor after that, at the next one kept, so a library scan doesn't
// keep them all.
type seriesMemo struct {
	at, kept time.Time // the cache file's time; when it was kept
	rec      *tvdbSeries
}

const seriesMemoFor = time.Minute

// seriesTTL: how long the extended series record is cached.
const seriesTTL = 7 * 24 * time.Hour

// tvdbSeries: what Details and seasonID read of the extended series record.
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

// seasonTTL: how long a season's text (or its lack of one) and its
// artwork are kept.
const seasonTTL = 7 * 24 * time.Hour

// tvdbLoginPause: how long a failed login is remembered. A rejected key
// won't fix itself; a network error may be gone in a moment.
const (
	tvdbBadKeyPause = 10 * time.Minute
	tvdbLoginPause  = 30 * time.Second
)

// NewTVDB uses the v4 key when there is one, else the legacy v3 key. With
// both, the legacy key takes over (with a warning) once TheTVDB rejects the
// v4 key, until the next restart.
func NewTVDB(f *fetch.Client, key, pin, legacyKey string) *TVDB {
	f.Throttle("api4.thetvdb.com", 100*time.Millisecond)
	f.Throttle("api.thetvdb.com", 100*time.Millisecond)
	return &TVDB{f: f, key: key, pin: pin, legacyKey: legacyKey, legacy: key == "" && legacyKey != "",
		crewQueued: map[int]bool{}, crewFailed: map[int]time.Time{}, noEnglish: map[int]time.Time{}, noEnglishSeries: map[int]time.Time{},
		series: map[int]seriesMemo{}}
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
// While a login is under way it doesn't wait: a call answered from the cache
// needs no token, and one that does waits in get, which notices a switch
// (errTVDBSwitched).
func (t *TVDB) useLegacy(ctx context.Context) bool {
	t.mu.Lock()
	check := t.key != "" && t.legacyKey != "" && !t.legacy && t.loggingIn == nil
	t.mu.Unlock()
	if check {
		_, _, _ = t.login(ctx)
	}
	return t.isLegacy()
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
	var se *fetch.StatusError
	return errors.As(err, &se) &&
		(se.Code == http.StatusBadRequest || se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden)
}

// errTVDBLogin marks login errors: they say nothing about the record a call
// asked for.
var errTVDBLogin = errors.New("tvdb login")

// login returns a bearer token and whether it's for the legacy (v3) API. A
// v4 token is valid a month (renewed weekly), a v3 token a day (renewed after
// 20 hours). t.mu is never held over the network: one login runs at a time,
// and callers that need a token meanwhile wait for it (or for their ctx).
func (t *TVDB) login(ctx context.Context) (string, bool, error) {
	for {
		t.mu.Lock()
		life := 7 * 24 * time.Hour
		if t.legacy {
			life = 20 * time.Hour
		}
		token, legacy, retryErr := t.token, t.legacy, t.loginErr
		switch {
		case token != "" && time.Since(t.tokenAt) < life:
			t.mu.Unlock()
			return token, legacy, nil
		case time.Now().Before(t.loginRetry):
			t.mu.Unlock()
			return "", legacy, retryErr
		}
		if running := t.loggingIn; running != nil {
			t.mu.Unlock()
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

		token, err := t.requestToken(ctx, legacy)

		t.mu.Lock()
		t.loggingIn = nil
		close(done)
		again := t.loginDone(ctx, legacy, token, err)
		t.mu.Unlock()
		if !again {
			return token, legacy, err
		}
	}
}

// requestToken logs in with the v4 or the legacy key.
func (t *TVDB) requestToken(ctx context.Context, legacy bool) (string, error) {
	creds := map[string]string{"apikey": t.key, "pin": t.pin}
	if legacy {
		creds = map[string]string{"apikey": t.legacyKey}
	}
	body, _ := json.Marshal(creds)
	data, err := t.f.Get(ctx, fetch.Request{URL: apiBase(legacy) + "/login", Method: "POST", Body: body, Quiet: true})
	if err != nil {
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
	t.loginErr, t.loginRetry = err, time.Now().Add(tvdbBadKeyPause)
	slog.Error("TVDB rejected the API key: "+hint+"; TVDB is skipped until the next try",
		"legacy", legacy, "retry_in", tvdbBadKeyPause, "err", err)
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
}

// getAs is get, asked the way o says.
func (t *TVDB) getAs(ctx context.Context, legacy bool, o getOpts, path, key string, ttl time.Duration, out any) error {
	if !t.Enabled() {
		return fmt.Errorf("tvdb: no api key")
	}
	key = cacheFolder(legacy) + key
	start := time.Now()
	if data, ok := t.f.Fresh(ctx, key, ttl); ok { // skip the login when cached; an expired file isn't read
		fetch.Observe(ctx, key, fetch.KindCache, start, nil)
		return json.Unmarshal(data, out)
	}
	for retried := false; ; retried = true {
		token, tokenLegacy, err := t.login(ctx)
		if err != nil {
			return err
		}
		if tokenLegacy != legacy {
			return errTVDBSwitched
		}
		headers := map[string]string{"Authorization": "Bearer " + token}
		if legacy {
			headers["Accept-Language"] = "en" // v3 picks the language by header
		}
		err = t.f.GetJSON(ctx, fetch.Request{URL: apiBase(legacy) + path, Key: key, TTL: ttl, Headers: headers, Quiet: o.quiet, NoStale: o.noStale,
			// Done sees a 401 even when a stale copy answers the call.
			Done: func(err error) {
				if unauthorized(err) {
					t.dropToken(token)
				}
			}}, out)
		if retried || !unauthorized(err) {
			return err
		}
		// No stale copy to answer with: try once more with a new token.
	}
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
	if t.useLegacy(ctx) {
		return t.episodes3(ctx, id)
	}
	type ep struct {
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
	var out []Episode
	err := episodePages(id, 0, func(page int) (bool, error) {
		var r struct {
			Data struct {
				Episodes []ep `json:"episodes"`
				Series   struct {
					Episodes []ep `json:"episodes"`
				} `json:"series"`
			} `json:"data"`
			Links struct {
				Next *string `json:"next"`
			} `json:"links"`
		}
		path := fmt.Sprintf("/series/%d/episodes/official/eng?page=%d", id, page)
		if err := t.get(ctx, false, path, fmt.Sprintf("%d/episodes-%d.json", id, page), 2*24*time.Hour, &r); err != nil {
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
	if t.useLegacy(ctx) {
		return t.overview3(ctx, id)
	}
	if t.knownNoEnglish(t.noEnglishSeries, id) {
		return "", "", nil
	}
	var r struct {
		Data struct {
			Name     string `json:"name"`
			Overview string `json:"overview"`
		} `json:"data"`
	}
	path, key := fmt.Sprintf("/series/%d/translations/eng", id), fmt.Sprintf("%d/eng.json", id)
	err = t.getAs(ctx, false, getOpts{quiet: true}, path, key, 7*24*time.Hour, &r)
	switch {
	case noLanguage(err):
		t.noEnglishUntil(t.noEnglishSeries, id)
		slog.Debug("tvdb: no English series text", "series", id, "retry_in", seasonTTL, "err", err)
		return "", "", nil
	case err != nil && !fetch.IsNotFound(err) && worthWarning(ctx, err):
		slog.Warn("tvdb: series text not fetched", "series", id, "err", err) // quiet request: logged here
	}
	return r.Data.Name, r.Data.Overview, err
}

// SeasonOverview returns the English overview of a season in aired
// ("official") order: the season's id is in the extended series record
// (shared with Details), its text in the season's translation. Season 0
// (specials) or less (absolute order) has none. A season TheTVDB doesn't
// list or has no English text for gives "" and no error. The legacy v3 API
// has no season records, so it gives "" too.
func (t *TVDB) SeasonOverview(ctx context.Context, id, season int) (string, error) {
	if season <= 0 || t.useLegacy(ctx) {
		return "", nil
	}
	sid, err := t.seasonID(ctx, id, season)
	if sid == 0 || t.knownNoEnglish(t.noEnglish, sid) {
		return "", err
	}
	var r struct {
		Data struct {
			Overview string `json:"overview"`
		} `json:"data"`
	}
	path, key := fmt.Sprintf("/seasons/%d/translations/eng", sid), fmt.Sprintf("%d/season-%d-eng.json", id, sid)
	err = t.getAs(ctx, false, getOpts{quiet: true}, path, key, seasonTTL, &r)
	switch {
	case noTranslation(err):
		t.noEnglishUntil(t.noEnglish, sid)
		slog.Debug("tvdb: no English season text", "series", id, "season", season, "retry_in", seasonTTL, "err", err)
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

// knownNoEnglish: TheTVDB answered, within seasonTTL, that the season or
// series id in m (noEnglish or noEnglishSeries) has no English text. Expired
// answers are dropped here, and when one is kept.
func (t *TVDB) knownNoEnglish(m map[int]time.Time, id int) bool {
	t.seasonMu.Lock()
	defer t.seasonMu.Unlock()
	until, ok := m[id]
	if ok && !time.Now().Before(until) {
		delete(m, id)
		return false
	}
	return ok
}

// noEnglishUntil keeps TheTVDB's answer that id in m has no English text,
// for seasonTTL. Answers past their time, of ids not asked again, go here.
func (t *TVDB) noEnglishUntil(m map[int]time.Time, id int) {
	t.seasonMu.Lock()
	defer t.seasonMu.Unlock()
	now := time.Now()
	for id, until := range m {
		if !now.Before(until) {
			delete(m, id)
		}
	}
	m[id] = now.Add(seasonTTL)
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
	return noLanguage(err) || (fetch.IsNotFound(err) && !errors.Is(err, errTVDBLogin))
}

// noLanguage: TheTVDB answered 400 ("language not found") for a
// translation; a 400 from the login (a missing PIN) isn't one.
func noLanguage(err error) bool {
	var se *fetch.StatusError
	return errors.As(err, &se) && se.Code == http.StatusBadRequest && !errors.Is(err, errTVDBLogin)
}

// Details returns genres, people, studio, content rating and first air date
// from the extended series record.
func (t *TVDB) Details(ctx context.Context, id int) (Details, error) {
	d, _, err := t.DetailsAndSeasons(ctx, id)
	return d, err
}

// DetailsAndSeasons is Details, plus the ids of the series' aired-order
// ("official") seasons by number (SeasonArt's), from the same record. The
// legacy v3 API has no season ids (nil): its season art goes by number.
func (t *TVDB) DetailsAndSeasons(ctx context.Context, id int) (Details, map[int]int, error) {
	if t.useLegacy(ctx) {
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

// seriesDetails reads Details from the extended series record.
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
	}
	for _, c := range r.Ratings {
		d.addRating(alpha2(c.Country), c.Name)
	}
	return d
}

// seriesRecord returns the extended series record, decoded once while its
// cache file stays the same (seriesMemo). It is shared: read it, don't change it.
func (t *TVDB) seriesRecord(ctx context.Context, id int) (*tvdbSeries, error) {
	key := fmt.Sprintf("%d/extended.json", id)
	t.seriesMu.Lock()
	m, ok := t.series[id]
	t.seriesMu.Unlock()
	if ok {
		start := time.Now()
		if at, cached := t.f.CachedAt(cacheFolder(false) + key); cached && at.Equal(m.at) && time.Since(at) < seriesTTL {
			fetch.Observe(ctx, cacheFolder(false)+key, fetch.KindCache, start, nil)
			return m.rec, nil
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
		now := time.Now()
		t.seriesMu.Lock()
		for id, m := range t.series {
			if now.Sub(m.kept) >= seriesMemoFor {
				delete(t.series, id)
			}
		}
		t.series[id] = seriesMemo{at: at, kept: now, rec: &r.Data}
		t.seriesMu.Unlock()
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

// byScore sorts artwork highest score first.
func byScore(arts []tvdbArtwork) {
	sort.SliceStable(arts, func(i, j int) bool { return arts[i].Score > arts[j].Score })
}

// Art returns posters, backgrounds and clear logos, highest score first.
func (t *TVDB) Art(ctx context.Context, id int) (Art, error) {
	if t.useLegacy(ctx) {
		return t.art3(ctx, id)
	}
	var r struct {
		Data struct {
			Artworks []tvdbArtwork `json:"artworks"`
		} `json:"data"`
	}
	if err := t.get(ctx, false, fmt.Sprintf("/series/%d/artworks", id), fmt.Sprintf("%d/artworks.json", id), 7*24*time.Hour, &r); err != nil {
		return Art{}, err
	}
	arts := r.Data.Artworks
	byScore(arts)
	var a Art
	for _, w := range arts {
		switch w.Type { // TheTVDB series artwork types
		case 2:
			a.Posters = append(a.Posters, artURL(w.Image))
		case 3:
			a.Backgrounds = append(a.Backgrounds, artURL(w.Image))
		case 23:
			if w.Lang == "" || w.Lang == "eng" {
				a.Logos = append(a.Logos, artURL(w.Image))
			}
		}
	}
	return a, nil
}

// SeasonArt returns the posters and backgrounds of a season in aired
// ("official") order, highest score first: the artwork of its season record
// (/seasons/{id}/extended). sid is the season's id, from DetailsAndSeasons
// (0: TheTVDB doesn't list the season, so it has none). TheTVDB seasons have
// posters, backgrounds, banners and icons, no clear logos; Plex has no image
// type for banners. The legacy v3 API goes by the season number, and has
// season posters only.
func (t *TVDB) SeasonArt(ctx context.Context, id, season, sid int) (Art, error) {
	if season < 0 {
		return Art{}, nil
	}
	if t.useLegacy(ctx) {
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
	path, key := fmt.Sprintf("/seasons/%d/extended", sid), fmt.Sprintf("%d/season-%d-extended.json", id, sid)
	if err := t.get(ctx, false, path, key, seasonTTL, &r); err != nil {
		return Art{}, err
	}
	arts := r.Data.Artwork
	byScore(arts)
	var a Art
	for _, w := range arts {
		switch w.Type { // TheTVDB season artwork types
		case 7:
			a.Posters = append(a.Posters, artURL(w.Image))
		case 8:
			a.Backgrounds = append(a.Backgrounds, artURL(w.Image))
		}
	}
	return a, nil
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
