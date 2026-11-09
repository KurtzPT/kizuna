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
}

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
		crewQueued: map[int]bool{}, crewFailed: map[int]time.Time{}}
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
				return "", legacy, fmt.Errorf("tvdb login: %w", ctx.Err())
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
		return "", fmt.Errorf("tvdb login: %w", err)
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
	return "", errors.New("tvdb login: no token in the answer")
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
	if !t.Enabled() {
		return fmt.Errorf("tvdb: no api key")
	}
	key = cacheFolder(legacy) + key
	start := time.Now()
	if data, age, ok := t.f.Cached(key); ok && age < ttl { // skip the login when cached
		slog.Debug("cache hit", "key", key, "age", age.Round(time.Minute), "ttl", ttl)
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
		err = t.f.GetJSON(ctx, fetch.Request{URL: apiBase(legacy) + path, Key: key, TTL: ttl, Headers: headers,
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

// Overview returns the English series name and summary.
func (t *TVDB) Overview(ctx context.Context, id int) (name, overview string, err error) {
	if t.useLegacy(ctx) {
		return t.overview3(ctx, id)
	}
	var r struct {
		Data struct {
			Name     string `json:"name"`
			Overview string `json:"overview"`
		} `json:"data"`
	}
	err = t.get(ctx, false, fmt.Sprintf("/series/%d/translations/eng", id), fmt.Sprintf("%d/eng.json", id), 7*24*time.Hour, &r)
	return r.Data.Name, r.Data.Overview, err
}

// Details returns genres, people, studio, content rating and first air date
// from the extended series record.
func (t *TVDB) Details(ctx context.Context, id int) (Details, error) {
	if t.useLegacy(ctx) {
		return t.details3(ctx, id)
	}
	var r struct {
		Data struct {
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
			} `json:"characters"`
			Ratings []struct {
				Name    string `json:"name"`
				Country string `json:"country"`
			} `json:"contentRatings"`
		} `json:"data"`
	}
	if err := t.get(ctx, false, fmt.Sprintf("/series/%d/extended", id), fmt.Sprintf("%d/extended.json", id), 7*24*time.Hour, &r); err != nil {
		return Details{}, err
	}
	d := Details{Date: r.Data.FirstAired}
	for _, g := range r.Data.Genres {
		d.Genres = append(d.Genres, g.Name)
	}
	for _, want := range []int{2, 3} { // TheTVDB company types: 2 studio, 3 production company
		for _, c := range r.Data.Companies {
			if d.Studio == "" && c.Type != nil && *c.Type == want {
				d.Studio = c.Name
			}
		}
	}
	people := r.Data.Characters
	sort.SliceStable(people, func(i, j int) bool { return people[i].Sort < people[j].Sort })
	for _, c := range people {
		switch c.PeopleType {
		case "Actor":
			d.Cast = append(d.Cast, Person{Name: c.PersonName, Role: c.Name, Photo: artURL(c.PersonImg)})
		case "Director":
			d.Directors = appendNew(d.Directors, c.PersonName)
		case "Writer":
			d.Writers = appendNew(d.Writers, c.PersonName)
		}
	}
	for _, c := range r.Data.Ratings {
		d.addRating(alpha2(c.Country), c.Name)
	}
	return d, nil
}

// Art returns posters, backgrounds and clear logos, highest score first.
func (t *TVDB) Art(ctx context.Context, id int) (Art, error) {
	if t.useLegacy(ctx) {
		return t.art3(ctx, id)
	}
	var r struct {
		Data struct {
			Artworks []struct {
				Type  int    `json:"type"`
				Image string `json:"image"`
				Score int    `json:"score"`
				Lang  string `json:"language"`
			} `json:"artworks"`
		} `json:"data"`
	}
	if err := t.get(ctx, false, fmt.Sprintf("/series/%d/artworks", id), fmt.Sprintf("%d/artworks.json", id), 7*24*time.Hour, &r); err != nil {
		return Art{}, err
	}
	arts := r.Data.Artworks
	sort.SliceStable(arts, func(i, j int) bool { return arts[i].Score > arts[j].Score })
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
