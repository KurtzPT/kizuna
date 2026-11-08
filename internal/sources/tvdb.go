package sources

import (
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

// TVDB is a minimal TheTVDB v4 client (needs your own API key).
type TVDB struct {
	f        *fetch.Client
	key, pin string

	mu      sync.Mutex
	token   string
	tokenAt time.Time
	// A failed login is remembered until loginRetry, so a wrong key costs
	// one request and one log line, not one per TVDB call of a library scan.
	loginErr   error
	loginRetry time.Time
}

// tvdbLoginPause: how long a failed login is remembered. A rejected key
// won't fix itself; a network error may be gone in a moment.
const (
	tvdbBadKeyPause = 10 * time.Minute
	tvdbLoginPause  = 30 * time.Second
)

func NewTVDB(f *fetch.Client, key, pin string) *TVDB {
	f.Throttle("api4.thetvdb.com", 100*time.Millisecond)
	return &TVDB{f: f, key: key, pin: pin}
}

func (t *TVDB) Enabled() bool { return t.key != "" }

// login returns a bearer token (valid a month; renewed weekly).
func (t *TVDB) login(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Since(t.tokenAt) < 7*24*time.Hour {
		return t.token, nil
	}
	if time.Now().Before(t.loginRetry) {
		return "", t.loginErr
	}
	body, _ := json.Marshal(map[string]string{"apikey": t.key, "pin": t.pin})
	data, err := t.f.Get(ctx, fetch.Request{URL: tvdbAPI + "/login", Method: "POST", Body: body, Quiet: true})
	var r struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err == nil {
		if json.Unmarshal(data, &r) != nil || r.Data.Token == "" {
			err = errors.New("no token in the answer")
		}
	}
	if err != nil {
		err = fmt.Errorf("tvdb login: %w", err)
		if ctx.Err() != nil { // the caller gave up: says nothing about TVDB
			return "", err
		}
		var se *fetch.StatusError
		if errors.As(err, &se) && (se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden) {
			t.loginErr, t.loginRetry = err, time.Now().Add(tvdbBadKeyPause)
			slog.Error("TVDB rejected the API key: check TVDB_API_KEY (and TVDB_PIN for subscriber keys); TVDB is skipped until the next try",
				"retry_in", tvdbBadKeyPause, "err", err)
		} else {
			t.loginErr, t.loginRetry = err, time.Now().Add(tvdbLoginPause)
			slog.Warn("TVDB login failed; TVDB is skipped until the next try", "retry_in", tvdbLoginPause, "err", err)
		}
		return "", err
	}
	t.token, t.tokenAt, t.loginErr, t.loginRetry = r.Data.Token, time.Now(), nil, time.Time{}
	return t.token, nil
}

func (t *TVDB) get(ctx context.Context, path, key string, ttl time.Duration, out any) error {
	if !t.Enabled() {
		return fmt.Errorf("tvdb: no api key")
	}
	if data, age, ok := t.f.Cached(key); ok && age < ttl { // skip the login when cached
		slog.Debug("cache hit", "key", key, "age", age.Round(time.Minute), "ttl", ttl)
		return json.Unmarshal(data, out)
	}
	token, err := t.login(ctx)
	if err != nil {
		return err
	}
	data, err := t.f.Get(ctx, fetch.Request{URL: tvdbAPI + path, Key: key, TTL: ttl,
		Headers: map[string]string{"Authorization": "Bearer " + token}})
	var se *fetch.StatusError
	if errors.As(err, &se) && se.Code == http.StatusUnauthorized {
		t.mu.Lock()
		if t.token == token { // expired or revoked early: log in again on the next call
			t.token = ""
		}
		t.mu.Unlock()
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// Episodes returns all episodes in aired ("official") order with English
// titles, sorted by season and number.
func (t *TVDB) Episodes(ctx context.Context, id int) ([]Episode, error) {
	type ep struct {
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
	var partial error
	for page := 0; page < 50; page++ {
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
		if err := t.get(ctx, path, fmt.Sprintf("tvdb/%d/episodes-%d.json", id, page), 2*24*time.Hour, &r); err != nil {
			if page == 0 {
				return nil, err
			}
			if !fetch.IsNotFound(err) { // a 404 past the last page just ends the list
				partial = fmt.Errorf("%w: tvdb %d page %d: %v", ErrIncomplete, id, page, err)
			}
			break
		}
		eps := r.Data.Episodes
		if len(eps) == 0 { // the translated list nests episodes under data.series
			eps = r.Data.Series.Episodes
		}
		for _, e := range eps {
			if e.Season == nil || e.Number == nil {
				continue
			}
			out = append(out, Episode{Season: *e.Season, Number: *e.Number, Absolute: e.Absolute, Title: e.Name,
				Summary: e.Overview, Aired: e.Aired, Thumb: artURL(e.Image), Minutes: e.Runtime})
		}
		if r.Links.Next == nil || *r.Links.Next == "" {
			break
		}
	}
	sortEpisodes(out)
	return out, partial
}

// Overview returns the English series name and summary.
func (t *TVDB) Overview(ctx context.Context, id int) (name, overview string) {
	var r struct {
		Data struct {
			Name     string `json:"name"`
			Overview string `json:"overview"`
		} `json:"data"`
	}
	_ = t.get(ctx, fmt.Sprintf("/series/%d/translations/eng", id), fmt.Sprintf("tvdb/%d/eng.json", id), 7*24*time.Hour, &r)
	return r.Data.Name, r.Data.Overview
}

// Details returns genres, people, studio, content rating and first air date
// from the extended series record.
func (t *TVDB) Details(ctx context.Context, id int) Details {
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
	if err := t.get(ctx, fmt.Sprintf("/series/%d/extended", id), fmt.Sprintf("tvdb/%d/extended.json", id), 7*24*time.Hour, &r); err != nil {
		return Details{}
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
	return d
}

// Art returns posters, backgrounds and clear logos, highest score first.
func (t *TVDB) Art(ctx context.Context, id int) Art {
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
	if err := t.get(ctx, fmt.Sprintf("/series/%d/artworks", id), fmt.Sprintf("tvdb/%d/artworks.json", id), 7*24*time.Hour, &r); err != nil {
		return Art{}
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
	return a
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
