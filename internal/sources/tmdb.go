package sources

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"kizuna/internal/fetch"
)

const (
	tmdbAPI = "https://api.themoviedb.org/3"
	tmdbImg = "https://image.tmdb.org/t/p/original"
)

// TMDB is a minimal TheMovieDb v3 client.
type TMDB struct {
	f   *fetch.Client
	key string
}

func NewTMDB(f *fetch.Client, key string) *TMDB {
	f.Throttle("api.themoviedb.org", 50*time.Millisecond)
	return &TMDB{f: f, key: key}
}

func (t *TMDB) Enabled() bool { return t != nil && t.key != "" } // nil = not set up

func (t *TMDB) get(ctx context.Context, path, query, key string, ttl time.Duration, out any) error {
	if !t.Enabled() {
		return fmt.Errorf("tmdb: no api key")
	}
	url := fmt.Sprintf("%s%s?api_key=%s%s", tmdbAPI, path, t.key, query)
	return t.f.GetJSON(ctx, fetch.Request{URL: url, Key: key, TTL: ttl}, out)
}

// TVFromTVDB finds the TMDB TV id for a TVDB id (0 if none).
func (t *TMDB) TVFromTVDB(ctx context.Context, tvdb int) (int, error) {
	var r struct {
		TV []struct {
			ID int `json:"id"`
		} `json:"tv_results"`
	}
	err := t.get(ctx, fmt.Sprintf("/find/%d", tvdb), "&external_source=tvdb_id", fmt.Sprintf("tmdb/find-tvdb-%d.json", tvdb), 30*24*time.Hour, &r)
	if len(r.TV) == 0 {
		return 0, err
	}
	return r.TV[0].ID, err
}

// Show is the part of a TMDB TV show we use.
type Show struct {
	Name     string   `json:"name"`
	Overview string   `json:"overview"`
	Seasons  []Season `json:"seasons"`
}

// Season is one season a TMDB TV show lists.
type Season struct {
	Number int `json:"season_number"`
}

func (t *TMDB) TV(ctx context.Context, id int) (Show, error) {
	var s Show
	err := t.get(ctx, fmt.Sprintf("/tv/%d", id), "&language=en-US", fmt.Sprintf("tmdb/tv-%d.json", id), 2*24*time.Hour, &s)
	return s, err
}

// Text returns the English title and overview of a TV show or a movie. A
// movie's come from its Details request and file: one request for both.
func (t *TMDB) Text(ctx context.Context, id int, movie bool) (name, overview string, err error) {
	if !movie {
		s, err := t.TV(ctx, id)
		return s.Name, s.Overview, err
	}
	var m struct {
		Title    string `json:"title"`
		Overview string `json:"overview"`
	}
	path, query, key := detailsRequest(id, true)
	err = t.get(ctx, path, query, key, detailsTTL, &m)
	return m.Title, m.Overview, err
}

const detailsTTL = 7 * 24 * time.Hour

// detailsRequest: the path, query and cache key of a show's or movie's
// Details, with its people and ratings appended.
func detailsRequest(id int, movie bool) (path, query, key string) {
	if movie {
		return fmt.Sprintf("/movie/%d", id), "&language=en-US&append_to_response=credits,release_dates", fmt.Sprintf("tmdb/movie-%d-details.json", id)
	}
	return fmt.Sprintf("/tv/%d", id), "&language=en-US&append_to_response=aggregate_credits,content_ratings", fmt.Sprintf("tmdb/tv-%d-details.json", id)
}

// Details returns genres, people, studio, content rating (US preferred) and
// first air / release date of a TV show or a movie, in one request.
func (t *TMDB) Details(ctx context.Context, id int, movie bool) (Details, error) {
	type person struct {
		Name    string `json:"name"`
		Profile string `json:"profile_path"`
		Order   int    `json:"order"`
		// TV (aggregate_credits)
		Roles []struct {
			Character string `json:"character"`
		} `json:"roles"`
		Jobs []struct {
			Job string `json:"job"`
		} `json:"jobs"`
		Episodes int `json:"total_episode_count"`
		// movie (credits)
		Character  string `json:"character"`
		Job        string `json:"job"`
		Department string `json:"department"`
	}
	type credits struct {
		Cast []person `json:"cast"`
		Crew []person `json:"crew"`
	}
	var r struct {
		Genres []struct {
			Name string `json:"name"`
		} `json:"genres"`
		Companies []struct {
			Name string `json:"name"`
		} `json:"production_companies"`
		FirstAir  string  `json:"first_air_date"`
		Release   string  `json:"release_date"`
		Aggregate credits `json:"aggregate_credits"`
		Credits   credits `json:"credits"`
		TVRatings struct {
			Results []struct {
				Country string `json:"iso_3166_1"`
				Rating  string `json:"rating"`
			} `json:"results"`
		} `json:"content_ratings"`
		Releases struct {
			Results []struct {
				Country string `json:"iso_3166_1"`
				Dates   []struct {
					Cert string `json:"certification"`
				} `json:"release_dates"`
			} `json:"results"`
		} `json:"release_dates"`
	}
	if id == 0 {
		return Details{}, nil
	}
	path, query, key := detailsRequest(id, movie)
	if err := t.get(ctx, path, query, key, detailsTTL, &r); err != nil {
		return Details{}, err
	}
	d := Details{Date: r.FirstAir}
	if movie {
		d.Date = r.Release
	}
	for _, g := range r.Genres {
		d.Genres = append(d.Genres, g.Name)
	}
	if len(r.Companies) > 0 {
		d.Studio = r.Companies[0].Name
	}

	c := r.Aggregate
	if movie {
		c = r.Credits
	}
	sort.SliceStable(c.Cast, func(i, j int) bool { return c.Cast[i].Order < c.Cast[j].Order })
	for i, p := range c.Cast {
		if i == 50 {
			break
		}
		role := p.Character
		if len(p.Roles) > 0 {
			role = p.Roles[0].Character
		}
		ps := Person{Name: p.Name, Role: role}
		if p.Profile != "" {
			ps.Photo = tmdbImg + p.Profile
		}
		d.Cast = append(d.Cast, ps)
	}
	// Anime crews list every episode director: keep the ones on the most episodes.
	sort.SliceStable(c.Crew, func(i, j int) bool { return c.Crew[i].Episodes > c.Crew[j].Episodes })
	for _, p := range c.Crew {
		jobs := []string{p.Job}
		for _, j := range p.Jobs {
			jobs = append(jobs, j.Job)
		}
		for _, job := range jobs {
			switch {
			case (job == "Director" || job == "Series Director") && len(d.Directors) < 5:
				d.Directors = appendNew(d.Directors, p.Name)
			case p.Department == "Writing" || job == "Series Composition" || job == "Writer" || job == "Original Story":
				if len(d.Writers) < 5 {
					d.Writers = appendNew(d.Writers, p.Name)
				}
			}
		}
	}

	for _, x := range r.TVRatings.Results {
		d.addRating(x.Country, x.Rating)
	}
	for _, x := range r.Releases.Results {
		for _, rd := range x.Dates {
			d.addRating(x.Country, rd.Cert)
		}
	}
	return d, nil
}

// Episodes returns all episodes of a TMDB show (seasons fetched in parallel).
func (t *TMDB) Episodes(ctx context.Context, id int) ([]Episode, error) {
	eps, _, err := t.EpisodesAndPosters(ctx, id)
	return eps, err
}

// EpisodesAndPosters is Episodes, plus the posters of each season that has
// any (SeasonArt's), read from the same files.
func (t *TMDB) EpisodesAndPosters(ctx context.Context, id int) ([]Episode, map[int][]string, error) {
	show, err := t.TV(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		out     []Episode
		posters map[int][]string
		failed  []int // seasons that could not be had
		last    error
	)
	for _, s := range show.Seasons {
		wg.Add(1)
		go func(season int) {
			defer wg.Done()
			var r struct {
				Episodes []struct {
					Number   int    `json:"episode_number"`
					Name     string `json:"name"`
					Overview string `json:"overview"`
					Air      string `json:"air_date"`
					Still    string `json:"still_path"`
					Runtime  int    `json:"runtime"`
					Crew     []struct {
						Job        string `json:"job"`
						Department string `json:"department"`
						Name       string `json:"name"`
					} `json:"crew"`
				} `json:"episodes"`
				seasonImages
			}
			err := t.season(ctx, id, season, &r)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !fetch.IsNotFound(err) { // a season TMDB lists but doesn't have: nothing to wait for
					failed, last = append(failed, season), err
				}
				return
			}
			if p := r.posters(); len(p) > 0 {
				if posters == nil {
					posters = map[int][]string{}
				}
				posters[season] = p
			}
			for _, e := range r.Episodes {
				ep := Episode{Season: season, Number: e.Number, Title: e.Name, Summary: e.Overview, Aired: e.Air, Minutes: e.Runtime}
				if e.Still != "" {
					ep.Thumb = tmdbImg + e.Still
				}
				for _, c := range e.Crew {
					switch {
					case c.Job == "Director":
						ep.Directors = append(ep.Directors, c.Name)
					case c.Department == "Writing":
						ep.Writers = append(ep.Writers, c.Name)
					}
				}
				ep.Directors, ep.Writers = people(ep.Directors), people(ep.Writers)
				out = append(out, ep)
			}
		}(s.Number)
	}
	wg.Wait()
	sortEpisodes(out)
	if len(failed) > 0 {
		sort.Ints(failed)
		return out, posters, fmt.Errorf("%w: tmdb %d seasons %v: %v", ErrIncomplete, id, failed, last)
	}
	return out, posters, nil
}

// season reads one season file of a TV show, which Episodes, SeasonOverview
// and SeasonArt share: the season's images come appended to it (TMDB's
// append_to_response), so its posters cost no request of their own.
func (t *TMDB) season(ctx context.Context, id, season int, out any) error {
	key := fmt.Sprintf("tmdb/tv-%d-s%d-images.json", id, season) // files from before the images had none: not read
	return t.get(ctx, fmt.Sprintf("/tv/%d/season/%d", id, season),
		"&language=en-US&append_to_response=images&include_image_language=en,ja,null", key, 2*24*time.Hour, out)
}

// SeasonOverview returns the English overview of a TV show's season, from
// the same request and cached file as the episode list. A season the show
// doesn't list gives none and costs no request (see listsSeason).
func (t *TMDB) SeasonOverview(ctx context.Context, id, season int) (string, error) {
	if ok, err := t.listsSeason(ctx, id, season); !ok {
		return "", err
	}
	var r struct {
		Overview string `json:"overview"`
	}
	err := t.season(ctx, id, season, &r)
	return r.Overview, err
}

// SeasonArt returns the posters of a TV show's season, English or textless
// first, as TMDB orders them by votes. TMDB seasons have no backgrounds or
// logos. A season the show doesn't list gives none and costs no request.
func (t *TMDB) SeasonArt(ctx context.Context, id, season int) (Art, error) {
	if ok, err := t.listsSeason(ctx, id, season); !ok {
		return Art{}, err
	}
	var r seasonImages
	if err := t.season(ctx, id, season, &r); err != nil {
		return Art{}, err
	}
	return Art{Posters: r.posters()}, nil
}

// listsSeason: the show lists the season. One it doesn't (often Specials)
// isn't asked for: TMDB would answer 404, which isn't cached. The show's
// file is cached, and usually read already.
func (t *TMDB) listsSeason(ctx context.Context, id, season int) (bool, error) {
	show, err := t.TV(ctx, id)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(show.Seasons, func(s Season) bool { return s.Number == season }), nil
}

// seasonImages is the images part of a season file.
type seasonImages struct {
	Images struct {
		Posters []struct {
			FilePath string `json:"file_path"`
		} `json:"posters"`
	} `json:"images"`
}

func (r seasonImages) posters() []string {
	var out []string
	for _, p := range r.Images.Posters {
		out = append(out, tmdbImg+p.FilePath)
	}
	return out
}

// Art returns images of a TV show (movie=false) or a movie (movie=true),
// English or textless first, as TMDB orders them by votes.
func (t *TMDB) Art(ctx context.Context, id int, movie bool) (Art, error) {
	kind := "tv"
	if movie {
		kind = "movie"
	}
	var r struct {
		Posters []struct {
			FilePath string `json:"file_path"`
		} `json:"posters"`
		Backdrops []struct {
			FilePath string `json:"file_path"`
		} `json:"backdrops"`
		Logos []struct {
			FilePath string `json:"file_path"`
		} `json:"logos"`
	}
	if id == 0 {
		return Art{}, nil
	}
	if err := t.get(ctx, fmt.Sprintf("/%s/%d/images", kind, id), "&include_image_language=en,ja,null",
		fmt.Sprintf("tmdb/%s-%d-images.json", kind, id), 7*24*time.Hour, &r); err != nil {
		return Art{}, err
	}
	var a Art
	for _, p := range r.Posters {
		a.Posters = append(a.Posters, tmdbImg+p.FilePath)
	}
	for _, p := range r.Backdrops {
		a.Backgrounds = append(a.Backgrounds, tmdbImg+p.FilePath)
	}
	for _, p := range r.Logos {
		a.Logos = append(a.Logos, tmdbImg+p.FilePath)
	}
	return a, nil
}
