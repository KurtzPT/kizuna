package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"kizuna/internal/fetch"
)

// Plex's own metadata service, the one behind Plex's built-in Plex Series
// agent. It speaks the protocol custom metadata providers use (Kizuna's
// own). Plex doesn't document it: what this file relies on was found by
// calling it (2026-11-13), and it can change.
const (
	plexHost = "metadata.provider.plex.tv"
	plexAPI  = "https://" + plexHost
	// plexPage: the most /children sends in one answer (500 answers 400).
	plexPage = 200
	// plexMaxPages: a season is read in at most this many pages, against an
	// answer whose totalSize never ends.
	plexMaxPages = 20
)

// Plex reads Plex's metadata service with the user's PLEX_TOKEN. The token
// goes on every request, only as a header, and to no other host: no request
// follows a redirect.
type Plex struct {
	f     *fetch.Client
	token string
	keys  *keyCheck
}

func NewPlex(f *fetch.Client, token string) *Plex {
	// The service's limit window isn't known (it sends x-ratelimit-limit:
	// 150 and nothing more): if 429s show up, widen the gap.
	f.Throttle(plexHost, 250*time.Millisecond)
	k := newKeyCheck("Plex", "PLEX_TOKEN")
	k.only401 = true // a 403 is Cloudflare's, in front of the service: not the token
	return &Plex{f: f, token: token, keys: k}
}

func (p *Plex) Enabled() bool { return p != nil && p.token != "" } // nil = not set up

var errNoPlexToken = errors.New("plex: no token")

// PlexRef is what Plex is asked for: a series by its TheTVDB id, or a movie
// by its TheMovieDb movie id or IMDb id. A series is never asked for by a
// TheMovieDb id: TheMovieDb's movie and series numbers are separate lists,
// and Plex's seasons and episodes follow TheTVDB's order.
type PlexRef struct {
	Movie bool
	guid  string // "tvdb://424536"; "" = nothing to ask
}

// PlexSeries is a series by its TheTVDB id (0 = none).
func PlexSeries(tvdb int) PlexRef {
	if tvdb <= 0 {
		return PlexRef{}
	}
	return PlexRef{guid: "tvdb://" + strconv.Itoa(tvdb)}
}

// PlexMovie is a movie by its TheMovieDb movie id (0 = none).
func PlexMovie(tmdb int) PlexRef {
	if tmdb <= 0 {
		return PlexRef{}
	}
	return PlexRef{Movie: true, guid: "tmdb://" + strconv.Itoa(tmdb)}
}

var imdbID = regexp.MustCompile(`^tt[0-9]{1,12}$`)

// PlexIMDbMovie is a movie by its IMDb id ("tt5311514"; anything else = none).
func PlexIMDbMovie(imdb string) PlexRef {
	if !imdbID.MatchString(imdb) {
		return PlexRef{}
	}
	return PlexRef{Movie: true, guid: "imdb://" + imdb}
}

// Valid: there is something to ask for.
func (r PlexRef) Valid() bool { return r.guid != "" }

func (r PlexRef) String() string { return r.guid }

// PlexShow is what Plex has for a show or a movie besides its seasons.
type PlexShow struct {
	Title, Summary string
	Details        Details
	Art            Art
	// People: every person Plex lists for it, in Plex's order. Details.Cast
	// is its first plexCast; CAST_PHOTOS_FROM_PLEX looks people up in all of
	// it.
	People []Person
}

// PlexSeason is one season of a Plex show.
type PlexSeason struct {
	Number  int
	Summary string
	Art     Art
}

// PlexPicture is one picture /images lists, with the site it is from
// ("tmdb", "tvdb", "fanarttv", "gracenote", "imdb").
type PlexPicture struct {
	URL, Source string
}

// PlexImages is every picture Plex has for a show or a movie.
type PlexImages struct {
	Posters, Backgrounds []PlexPicture
}

// plexContainer is an answer: a MediaContainer.
type plexContainer struct {
	MediaContainer plexMedia `json:"MediaContainer"`
}

// exactKeys decodes the keys of a JSON object that fields names, each into
// the pointer it gives, and leaves the others out. Plex's answers have keys
// that differ only in case ("Studio", a list, next to "studio", a string),
// and Go's JSON matches a struct's keys regardless of case: the list would
// land in the string and fail the decode, for every such pair Plex has or
// adds. A map's keys are matched exactly.
func exactKeys(data []byte, fields map[string]any) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for key, to := range fields {
		if v, ok := raw[key]; ok {
			if err := json.Unmarshal(v, to); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	return nil
}

type plexMedia struct {
	TotalSize int
	Metadata  []plexItem
	Thumb     []plexPicture // /images: posters
	Art       []plexPicture // /images: backgrounds
}

func (m *plexMedia) UnmarshalJSON(data []byte) error {
	return exactKeys(data, map[string]any{"totalSize": &m.TotalSize, "Metadata": &m.Metadata, "Thumb": &m.Thumb, "Art": &m.Art})
}

type plexItem struct {
	RatingKey   string
	Title       string
	Summary     string
	Index       int
	ParentIndex int
	Thumb       string
	Date        string // originallyAvailableAt
	Duration    int    // milliseconds
	Studio      string
	Rating      string // contentRating
	Genre       []plexTag
	Director    []plexTag
	Writer      []plexTag
	Producer    []plexTag
	Role        []plexRole
	Image       []plexImage
}

func (it *plexItem) UnmarshalJSON(data []byte) error {
	return exactKeys(data, map[string]any{
		"ratingKey": &it.RatingKey, "title": &it.Title, "summary": &it.Summary, "index": &it.Index,
		"parentIndex": &it.ParentIndex, "thumb": &it.Thumb, "originallyAvailableAt": &it.Date, "duration": &it.Duration,
		"studio": &it.Studio, "contentRating": &it.Rating, "Genre": &it.Genre, "Director": &it.Director,
		"Writer": &it.Writer, "Producer": &it.Producer, "Role": &it.Role, "Image": &it.Image,
	})
}

type plexTag struct {
	Tag   string
	Thumb string // a person's photo (crew tags)
}

func (t *plexTag) UnmarshalJSON(data []byte) error {
	return exactKeys(data, map[string]any{"tag": &t.Tag, "thumb": &t.Thumb})
}

type plexRole struct {
	Tag, Role, Thumb string
	Order            int
}

func (r *plexRole) UnmarshalJSON(data []byte) error {
	return exactKeys(data, map[string]any{"tag": &r.Tag, "role": &r.Role, "thumb": &r.Thumb, "order": &r.Order})
}

type plexImage struct {
	Type, URL string
}

func (im *plexImage) UnmarshalJSON(data []byte) error {
	return exactKeys(data, map[string]any{"type": &im.Type, "url": &im.URL})
}

type plexPicture struct {
	Key, Source string
}

func (x *plexPicture) UnmarshalJSON(data []byte) error {
	return exactKeys(data, map[string]any{"key": &x.Key, "source": &x.Source})
}

// get reads one answer from the service, in English.
func (p *Plex) get(ctx context.Context, r fetch.Request, out *plexContainer) error {
	if !p.Enabled() {
		return errNoPlexToken
	}
	h := map[string]string{"X-Plex-Token": p.token, "X-Plex-Language": "en-US", "Accept": "application/json"}
	maps.Copy(h, r.Headers)
	r.Headers, r.NoRedirect = h, true // a redirect would carry the token to another host
	return p.keys.getJSON(ctx, p.f, r, out)
}

// plexKeyChars: Plex's ids are 24 hex digits; anything else isn't put in a
// URL or a cache file name.
var plexKeyChars = regexp.MustCompile(`^[0-9A-Za-z]{1,64}$`)

func plexID(s string) string {
	if plexKeyChars.MatchString(s) {
		return s
	}
	return ""
}

// plexMatchTTL: how long Plex's match is kept; an empty one (Plex doesn't
// have the show yet) is asked again once it is plexMatchNoneTTL old.
const (
	plexMatchTTL     = 30 * 24 * time.Hour
	plexMatchNoneTTL = 24 * time.Hour
)

// match finds Plex's id for r: "" when Plex doesn't have it. The answer is
// kept 30 days; an empty one is asked again once it is a day old, so a new
// show gets Plex's fields soon after Plex adds it.
func (p *Plex) match(ctx context.Context, r PlexRef) (string, error) {
	if !p.Enabled() {
		return "", errNoPlexToken
	}
	if !r.Valid() {
		return "", nil
	}
	typ, kind := 2, "show"
	if r.Movie {
		typ, kind = 1, "movie"
	}
	body, err := json.Marshal(struct {
		Type  int    `json:"type"`
		Title string `json:"title"`
		GUID  string `json:"guid"`
	}{typ, "x", r.guid})
	if err != nil {
		return "", err
	}
	req := fetch.Request{URL: plexAPI + "/library/metadata/matches", Method: http.MethodPost, Body: body,
		Key: "plex/match-" + kind + "-" + strings.Replace(r.guid, "://", "-", 1) + ".json", TTL: plexMatchTTL}
	var c plexContainer
	err = p.get(ctx, req, &c)
	if err == nil && len(c.MediaContainer.Metadata) == 0 && askNoneAgain(p.f, req.Key, plexMatchNoneTTL, plexMatchTTL) {
		// A failed re-ask (the copy pruned in between) keeps the empty
		// answer: it is still Plex's, and the show stays complete.
		req.TTL = plexMatchNoneTTL
		var again plexContainer
		if p.get(ctx, req, &again) == nil {
			c = again
		}
	}
	if err != nil {
		if fetch.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	for _, m := range c.MediaContainer.Metadata {
		if id := plexID(m.RatingKey); id != "" {
			return id, nil
		}
	}
	return "", nil
}

// item reads the show or movie r is (empty: Plex doesn't have it).
func (p *Plex) item(ctx context.Context, r PlexRef) (plexItem, error) {
	id, err := p.match(ctx, r)
	if id == "" {
		return plexItem{}, err
	}
	var c plexContainer
	if err := p.get(ctx, fetch.Request{URL: plexAPI + "/library/metadata/" + id, Key: "plex/item-" + id + ".json",
		TTL: 7 * 24 * time.Hour}, &c); err != nil || len(c.MediaContainer.Metadata) == 0 {
		return plexItem{}, err
	}
	return c.MediaContainer.Metadata[0], nil
}

// Show returns a show's or a movie's title, summary, details and pictures,
// all from one request (and one cached file). Plex not having it is no
// error: everything is empty.
func (p *Plex) Show(ctx context.Context, r PlexRef) (PlexShow, error) {
	it, err := p.item(ctx, r)
	if err != nil {
		return PlexShow{}, err
	}
	d, people := it.details()
	return PlexShow{Title: it.Title, Summary: it.Summary, Details: d, Art: plexArt(it.Image), People: people}, nil
}

// plexCast: how much of the cast is kept, as from TheMovieDb.
const plexCast = 50

// details: the item's genres, people, studio, content rating and date, and
// every person with a name in Plex's order (people). The cast is people's
// first plexCast, one list: its capacity stops there too, so an append to
// the cast can't write over the people after it.
func (it plexItem) details() (d Details, people []Person) {
	d = Details{Studio: strings.TrimSpace(it.Studio), Date: it.Date, Genres: tags(it.Genre),
		Directors: tags(it.Director), Writers: tags(it.Writer), CrewPhotos: it.crewPhotos()}
	roles := append([]plexRole(nil), it.Role...)
	sort.SliceStable(roles, func(i, j int) bool { return roles[i].Order < roles[j].Order })
	for _, r := range roles {
		if name := strings.TrimSpace(r.Tag); name != "" {
			people = append(people, Person{Name: name, Role: strings.TrimSpace(r.Role), Photo: r.Thumb})
		}
	}
	n := min(len(people), plexCast)
	d.Cast = people[:n:n]
	// "TV-14" is a US rating; another country's comes as "de/16".
	rating := strings.TrimSpace(it.Rating)
	if country, r, ok := strings.Cut(rating, "/"); ok {
		d.addRating(strings.ToUpper(strings.TrimSpace(country)), strings.TrimSpace(r))
	} else {
		d.addRating("US", rating)
	}
	return d, people
}

func tags(t []plexTag) []string { return people(tagNames(t)) }

func tagNames(t []plexTag) []string {
	names := make([]string, 0, len(t))
	for _, x := range t {
		names = append(names, x.Tag)
	}
	return names
}

// crewPhotos: the item's directors, writers and producers with a photo.
func (it plexItem) crewPhotos() []Person {
	var out []Person
	for _, list := range [...][]plexTag{it.Director, it.Writer, it.Producer} {
		for _, x := range list {
			out = crewPhoto(out, x.Tag, x.Thumb)
		}
	}
	return out
}

// plexArt: the posters, backgrounds and logos of an item's Image list.
func plexArt(images []plexImage) Art {
	var a Art
	for _, im := range images {
		if im.URL == "" {
			continue
		}
		switch im.Type {
		case "coverPoster":
			a.Posters = append(a.Posters, im.URL)
		case "background":
			a.Backgrounds = append(a.Backgrounds, im.URL)
		case "clearLogo":
			a.Logos = append(a.Logos, im.URL)
		}
	}
	return a
}

// Images returns every poster and background Plex lists for a show or a
// movie, each with the site it is from (ART_ALL_SOURCES).
func (p *Plex) Images(ctx context.Context, r PlexRef) (PlexImages, error) {
	id, err := p.match(ctx, r)
	if id == "" {
		return PlexImages{}, err
	}
	var c plexContainer
	if err := p.get(ctx, fetch.Request{URL: plexAPI + "/library/metadata/" + id + "/images",
		Key: "plex/images-" + id + ".json", TTL: 7 * 24 * time.Hour}, &c); err != nil {
		return PlexImages{}, err
	}
	pictures := func(list []plexPicture) []PlexPicture {
		var out []PlexPicture
		for _, x := range list {
			if x.Key != "" {
				out = append(out, PlexPicture{URL: x.Key, Source: x.Source})
			}
		}
		return out
	}
	return PlexImages{Posters: pictures(c.MediaContainer.Thumb), Backgrounds: pictures(c.MediaContainer.Art)}, nil
}

// seasonItems reads a series' season list, in one answer (a movie has
// none). It isn't paged: no show has more seasons than a page holds. If one
// ever does, the seasons that came are returned with ErrIncomplete. An empty
// list whose totalSize says there are seasons is refused (cutShort), like an
// episode page: kept, every rebuild for 2 days would read it again.
func (p *Plex) seasonItems(ctx context.Context, r PlexRef) ([]plexItem, error) {
	if r.Movie {
		return nil, nil
	}
	id, err := p.match(ctx, r)
	if id == "" {
		return nil, err
	}
	c, err := p.children(ctx, id, 0, cutShort(id, 0))
	m := c.MediaContainer
	if err == nil && m.TotalSize > len(m.Metadata) {
		err = fmt.Errorf("%w: plex %s: %d of %d seasons", ErrIncomplete, r, len(m.Metadata), m.TotalSize)
	}
	return m.Metadata, err
}

// children reads one page of an item's children (a show's seasons, a
// season's episodes), from the one numbered start, plexPage at once.
// validate, if set, refuses an answer that must not be kept.
func (p *Plex) children(ctx context.Context, id string, start int, validate func([]byte) error) (plexContainer, error) {
	key := "plex/children-" + id + ".json"
	h := map[string]string{"X-Plex-Container-Size": strconv.Itoa(plexPage)}
	if start > 0 {
		key = "plex/children-" + id + "-" + strconv.Itoa(start) + ".json"
		h["X-Plex-Container-Start"] = strconv.Itoa(start)
	}
	var c plexContainer
	err := p.get(ctx, fetch.Request{URL: plexAPI + "/library/metadata/" + id + "/children", Key: key, TTL: 2 * 24 * time.Hour,
		Headers: h, Validate: validate}, &c)
	return c, err
}

// plexPageSize is what tells whether a page was cut short: its totalSize and
// how many items it holds. The items aren't decoded.
type plexPageSize struct {
	total, items int
}

func (s *plexPageSize) UnmarshalJSON(data []byte) error {
	var items []json.RawMessage
	err := exactKeys(data, map[string]any{"totalSize": &s.total, "Metadata": &items})
	if items != nil { // a second key that only differs in case mustn't empty the count
		s.items = len(items)
	}
	return err
}

// cutShort refuses a page (of a season's episodes, or a show's season list)
// that has no items while its totalSize says there are more from start on:
// Plex cut the list short. Kept, it would answer every rebuild for the
// page's 2 days, and the list would stay incomplete with no request to Plex.
// Refused, nothing is written and the next build asks again. An empty page
// with nothing after it (a season without episodes, the page past the last
// one) is kept like any other.
func cutShort(id string, start int) func([]byte) error {
	return func(data []byte) error {
		var c struct {
			MediaContainer plexPageSize `json:"MediaContainer"`
		}
		if err := json.Unmarshal(data, &c); err != nil {
			return nil // not a page at all: the decode after this reports it
		}
		if m := c.MediaContainer; m.items == 0 && m.total > start {
			return fmt.Errorf("list cut short: no items of %s from %d of %d", id, start, m.total)
		}
		return nil
	}
}

// Seasons returns each season's summary and pictures (Specials is 0), from
// the season list: one request for every season.
func (p *Plex) Seasons(ctx context.Context, r PlexRef) ([]PlexSeason, error) {
	items, err := p.seasonItems(ctx, r)
	if err != nil && !errors.Is(err, ErrIncomplete) {
		return nil, err
	}
	out := make([]PlexSeason, 0, len(items))
	for _, it := range items {
		out = append(out, PlexSeason{Number: it.Index, Summary: it.Summary, Art: plexArt(it.Image)})
	}
	return out, err
}

// Episodes returns every episode of a series, in TheTVDB's aired order (the
// one Plex sends without an episodeOrder): each season's /children, 200 a
// page. Not /grandchildren, which sends at most 20 a page. The seasons are
// read one after the other: the host's throttle spaces the requests anyway.
// Like the other sources' lists, a season that failed makes the list
// ErrIncomplete.
func (p *Plex) Episodes(ctx context.Context, r PlexRef) ([]Episode, error) {
	seasons, listErr := p.seasonItems(ctx, r) // ErrIncomplete: the seasons that came are still read
	if listErr != nil && !errors.Is(listErr, ErrIncomplete) {
		return nil, listErr
	}
	var (
		out    []Episode
		failed []int
		last   error
	)
	for _, s := range seasons {
		id := plexID(s.RatingKey)
		if id == "" {
			failed, last = append(failed, s.Index), fmt.Errorf("season %d: bad id %q", s.Index, s.RatingKey)
			continue
		}
		eps, err := p.seasonEpisodes(ctx, id, s.Index)
		out = append(out, eps...)
		if err != nil && !fetch.IsNotFound(err) {
			failed, last = append(failed, s.Index), err
		}
	}
	sortEpisodes(out)
	if len(failed) > 0 {
		return out, fmt.Errorf("%w: plex %s seasons %v: %v", ErrIncomplete, r, failed, last)
	}
	if listErr != nil {
		return out, listErr
	}
	return out, nil
}

// seasonEpisodes reads one season's episodes, page by page, until it has
// the totalSize Plex gives (a full page without one: the next page too). An
// empty page before that is an error: the list was cut short. Such a page is
// not cached (cutShort), so the next build asks Plex for it again. When an
// older copy of the page is in the cache, that copy answers instead, like
// for any failed download, and the reading goes on from it.
func (p *Plex) seasonEpisodes(ctx context.Context, id string, season int) ([]Episode, error) {
	var out []Episode
	for page, start := 0, 0; ; page++ {
		if page == plexMaxPages {
			return out, fmt.Errorf("season %d: more than %d pages", season, plexMaxPages)
		}
		c, err := p.children(ctx, id, start, cutShort(id, start))
		if err != nil {
			return out, err
		}
		m := c.MediaContainer
		for _, it := range m.Metadata {
			out = append(out, it.episode(season))
		}
		start += len(m.Metadata)
		if m.TotalSize > 0 && start >= m.TotalSize || m.TotalSize == 0 && len(m.Metadata) < plexPage {
			return out, nil
		}
		if len(m.Metadata) == 0 { // only a copy cached before such pages were refused
			return out, fmt.Errorf("season %d: %d of %d episodes", season, start, m.TotalSize)
		}
	}
}

// episode: an episode item of the season numbered season.
func (it plexItem) episode(season int) Episode {
	return Episode{Season: season, Number: it.Index, Title: it.Title, Summary: it.Summary, Aired: it.Date, Thumb: it.Thumb,
		Minutes: max(0, (it.Duration+30_000)/60_000), Crew: Crew{Directors: tags(it.Director), Writers: tags(it.Writer),
			Producers: producers(tagNames(it.Producer)), Photos: it.crewPhotos()}}
}
