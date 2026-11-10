package sources

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"

	"kizuna/internal/fetch"
)

// TheTVDB's legacy v3 API (api.thetvdb.com), for keys from before v4
// (TVDB_LEGACY_API_KEY). It's used when there's no v4 key, or once TheTVDB
// rejects the v4 key. Compared with
// v4 it has no clear logos, no studios, no season texts or backgrounds (season
// posters it has) and no series-level directors or writers (episodes have
// theirs), and episodes carry no runtime of their own (the series' is used).

const tvdb3API = "https://api.thetvdb.com"

// series3 is the v3 series record, shared by overview3, details3 and art3.
type series3 struct {
	Name       string   `json:"seriesName"`
	Overview   string   `json:"overview"`
	FirstAired string   `json:"firstAired"`
	Genres     []string `json:"genre"`
	Rating     string   `json:"rating"` // US content rating, e.g. "TV-14"
	Runtime    string   `json:"runtime"`
	Poster     string   `json:"poster"`
	Fanart     string   `json:"fanart"`
}

func (t *TVDB) series3(ctx context.Context, id int) (series3, error) {
	var r struct {
		Data series3 `json:"data"`
	}
	err := t.get(ctx, true, fmt.Sprintf("/series/%d", id), fmt.Sprintf("%d/series.json", id), seriesFilesTTL, &r)
	return r.Data, err
}

func (t *TVDB) episodes3(ctx context.Context, id int) ([]Episode, error) {
	type ep struct {
		ID        int      `json:"id"`
		Season    *int     `json:"airedSeason"`
		Number    *int     `json:"airedEpisodeNumber"`
		Absolute  *int     `json:"absoluteNumber"`
		Name      string   `json:"episodeName"`
		Overview  string   `json:"overview"`
		Aired     string   `json:"firstAired"`
		Image     string   `json:"filename"`
		Directors []string `json:"directors"`
		Writers   []string `json:"writers"`
	}
	s, _ := t.series3(ctx, id) // only for the runtime
	minutes, _ := strconv.Atoi(s.Runtime)
	var out []Episode
	err := episodePages(id, 1, func(page int) (bool, error) { // v3 pages count from 1
		var r struct {
			Data  []ep `json:"data"`
			Links struct {
				Next *int `json:"next"`
			} `json:"links"`
		}
		path := fmt.Sprintf("/series/%d/episodes?page=%d", id, page)
		if err := t.get(ctx, true, path, episodesKey(id, page), episodesTTL, &r); err != nil {
			return false, err
		}
		for _, e := range r.Data {
			if e.Season == nil || e.Number == nil {
				continue
			}
			abs := 0
			if e.Absolute != nil {
				abs = *e.Absolute
			}
			out = append(out, Episode{ID: e.ID, Season: *e.Season, Number: *e.Number, Absolute: abs, Title: e.Name,
				Summary: e.Overview, Aired: e.Aired, Thumb: artURL(e.Image), Minutes: minutes,
				Directors: people(e.Directors), Writers: people(e.Writers)})
		}
		return r.Links.Next != nil, nil
	})
	sortEpisodes(out)
	return out, err
}

func (t *TVDB) overview3(ctx context.Context, id int) (name, overview string, err error) {
	s, err := t.series3(ctx, id)
	return s.Name, s.Overview, err
}

// details3 returns the series' details, with the actors list's error when
// only that failed.
func (t *TVDB) details3(ctx context.Context, id int) (Details, error) {
	s, err := t.series3(ctx, id)
	if err != nil {
		return Details{}, err
	}
	d := Details{Date: s.FirstAired, Genres: s.Genres}
	d.addRating("US", s.Rating)
	var r struct {
		Data []struct {
			Name  string `json:"name"`
			Role  string `json:"role"`
			Image string `json:"image"`
			Sort  int    `json:"sortOrder"`
		} `json:"data"`
	}
	if err = t.get(ctx, true, fmt.Sprintf("/series/%d/actors", id), fmt.Sprintf("%d/actors.json", id), seriesTTL, &r); fetch.IsNotFound(err) {
		err = nil // no actors
	}
	sort.SliceStable(r.Data, func(i, j int) bool { return r.Data[i].Sort < r.Data[j].Sort })
	for _, a := range r.Data {
		d.Cast = append(d.Cast, Person{Name: a.Name, Role: a.Role, Photo: artURL(a.Image)})
	}
	return d, err
}

// art3 returns posters and backgrounds (v3 has no clear logos): the series'
// own pick first, then the rest by rating, with an image list's error when
// only that failed.
func (t *TVDB) art3(ctx context.Context, id int) (Art, error) {
	s, err := t.series3(ctx, id)
	if err != nil {
		return Art{}, err
	}
	images := func(keyType, main string) []string {
		var out []string
		if main != "" {
			out = append(out, artURL(main))
		}
		path := fmt.Sprintf("/series/%d/images/query?keyType=%s", id, keyType)
		found, e := t.images3(ctx, path, fmt.Sprintf("%d/%s.json", id, keyType), seriesFilesTTL)
		if e != nil {
			err = e
		}
		for _, u := range found {
			if !slices.Contains(out, u) {
				out = append(out, u)
			}
		}
		return out
	}
	a := Art{Posters: images("poster", s.Poster), Backgrounds: images("fanart", s.Fanart)}
	return a, err
}

// seasonArt3 returns a season's posters, by rating: v3's season images
// (keyType=season, its subKey the season number). v3 has no season
// backgrounds.
func (t *TVDB) seasonArt3(ctx context.Context, id, season int) (Art, error) {
	path := fmt.Sprintf("/series/%d/images/query?keyType=season&subKey=%d", id, season)
	posters, err := t.images3(ctx, path, fmt.Sprintf("%d/season-%d.json", id, season), seasonTTL)
	return Art{Posters: posters}, err
}

// images3 reads one v3 image query, cached for ttl, best rated first. A 404
// means no images of the kind: none, and no error.
func (t *TVDB) images3(ctx context.Context, path, key string, ttl time.Duration) ([]string, error) {
	var r struct {
		Data []struct {
			File    string `json:"fileName"`
			Ratings struct {
				Average float64 `json:"average"`
			} `json:"ratingsInfo"`
		} `json:"data"`
	}
	if err := t.get(ctx, true, path, key, ttl, &r); err != nil {
		if fetch.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.SliceStable(r.Data, func(i, j int) bool { return r.Data[i].Ratings.Average > r.Data[j].Ratings.Average })
	var out []string
	for _, img := range r.Data {
		if u := artURL(img.File); u != "" && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out, nil
}
