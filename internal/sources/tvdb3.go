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
// v4 it has no clear logos, no studios and no series-level directors or
// writers, and episodes carry no runtime of their own (the series' is used).

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
	err := t.get(ctx, true, fmt.Sprintf("/series/%d", id), fmt.Sprintf("%d/series.json", id), 7*24*time.Hour, &r)
	return r.Data, err
}

func (t *TVDB) episodes3(ctx context.Context, id int) ([]Episode, error) {
	type ep struct {
		Season   *int   `json:"airedSeason"`
		Number   *int   `json:"airedEpisodeNumber"`
		Absolute *int   `json:"absoluteNumber"`
		Name     string `json:"episodeName"`
		Overview string `json:"overview"`
		Aired    string `json:"firstAired"`
		Image    string `json:"filename"`
	}
	s, _ := t.series3(ctx, id) // only for the runtime
	minutes, _ := strconv.Atoi(s.Runtime)
	var out []Episode
	var partial error
	for page := 1; page <= 50; page++ { // v3 pages count from 1
		var r struct {
			Data  []ep `json:"data"`
			Links struct {
				Next *int `json:"next"`
			} `json:"links"`
		}
		path := fmt.Sprintf("/series/%d/episodes?page=%d", id, page)
		if err := t.get(ctx, true, path, fmt.Sprintf("%d/episodes-%d.json", id, page), 2*24*time.Hour, &r); err != nil {
			if page == 1 {
				return nil, err
			}
			if !fetch.IsNotFound(err) { // a 404 past the last page just ends the list
				partial = fmt.Errorf("%w: tvdb %d page %d: %v", ErrIncomplete, id, page, err)
			}
			break
		}
		for _, e := range r.Data {
			if e.Season == nil || e.Number == nil {
				continue
			}
			abs := 0
			if e.Absolute != nil {
				abs = *e.Absolute
			}
			out = append(out, Episode{Season: *e.Season, Number: *e.Number, Absolute: abs, Title: e.Name,
				Summary: e.Overview, Aired: e.Aired, Thumb: artURL(e.Image), Minutes: minutes})
		}
		if r.Links.Next == nil {
			break
		}
	}
	sortEpisodes(out)
	return out, partial
}

func (t *TVDB) overview3(ctx context.Context, id int) (name, overview string) {
	s, _ := t.series3(ctx, id)
	return s.Name, s.Overview
}

func (t *TVDB) details3(ctx context.Context, id int) Details {
	s, err := t.series3(ctx, id)
	if err != nil {
		return Details{}
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
	if t.get(ctx, true, fmt.Sprintf("/series/%d/actors", id), fmt.Sprintf("%d/actors.json", id), 7*24*time.Hour, &r) == nil {
		sort.SliceStable(r.Data, func(i, j int) bool { return r.Data[i].Sort < r.Data[j].Sort })
		for _, a := range r.Data {
			d.Cast = append(d.Cast, Person{Name: a.Name, Role: a.Role, Photo: artURL(a.Image)})
		}
	}
	return d
}

// art3 returns posters and backgrounds (v3 has no clear logos): the series'
// own pick first, then the rest by rating.
func (t *TVDB) art3(ctx context.Context, id int) Art {
	s, err := t.series3(ctx, id)
	if err != nil {
		return Art{}
	}
	images := func(keyType, main string) []string {
		var r struct {
			Data []struct {
				File    string `json:"fileName"`
				Ratings struct {
					Average float64 `json:"average"`
				} `json:"ratingsInfo"`
			} `json:"data"`
		}
		var out []string
		if main != "" {
			out = append(out, artURL(main))
		}
		path := fmt.Sprintf("/series/%d/images/query?keyType=%s", id, keyType)
		if t.get(ctx, true, path, fmt.Sprintf("%d/%s.json", id, keyType), 7*24*time.Hour, &r) != nil {
			return out
		}
		sort.SliceStable(r.Data, func(i, j int) bool { return r.Data[i].Ratings.Average > r.Data[j].Ratings.Average })
		for _, img := range r.Data {
			if u := artURL(img.File); u != "" && !slices.Contains(out, u) {
				out = append(out, u)
			}
		}
		return out
	}
	return Art{Posters: images("poster", s.Poster), Backgrounds: images("fanart", s.Fanart)}
}
