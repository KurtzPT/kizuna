// Package sources holds the online metadata sources: the AniDB HTTP API,
// TheTVDB (v4), TheMovieDb (v3) and MyAnimeList (API v2).
// Each returns plain structs; nothing here knows about Plex.
package sources

import (
	"errors"
	"sort"
	"strings"
)

// ErrIncomplete: an episode list came back with parts missing (a TVDB page
// or a TMDB season failed and nothing was cached). The episodes returned are
// still usable; the caller should try again soon instead of keeping them.
var ErrIncomplete = errors.New("episode list incomplete")

// Episode is one TVDB/TMDB episode in aired order.
type Episode struct {
	Season   int
	Number   int // episode number inside the season
	Absolute int // absolute number (0 = unknown)
	Title    string
	Summary  string
	Aired    string // YYYY-MM-DD
	Thumb    string // full URL
	Minutes  int
	ID       int // TheTVDB v4 episode id, for EpisodeCrew (0 from other lists)
	// Directors and writers: TheTVDB v3 and TheMovieDb lists carry them; a
	// TheTVDB v4 list doesn't (see EpisodeCrew).
	Directors, Writers []string
}

// people trims names and drops empty and repeated ones.
func people(names []string) []string {
	var out []string
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out = appendNew(out, n)
		}
	}
	return out
}

// Art is a list of image URLs per Plex image type, best first.
type Art struct {
	Posters, Backgrounds, Logos []string
}

// FillAbsolute numbers regular episodes 1..n across seasons in aired order
// when the source has no absolute number of its own. This is exactly the
// "tvdb" mode rule: the first episode of season 2 is (last episode of season 1)+1.
// Specials (season 0) get no absolute number.
func FillAbsolute(eps []Episode) {
	n := 0
	for i := range eps {
		if eps[i].Season == 0 {
			continue
		}
		n++
		if eps[i].Absolute == 0 {
			eps[i].Absolute = n
		} else {
			n = eps[i].Absolute
		}
	}
}

// Details holds the show-level fields besides title, summary and artwork.
// Empty fields mean the source has nothing for them.
type Details struct {
	Genres        []string
	Cast          []Person
	Directors     []string
	Writers       []string
	Studio        string
	ContentRating string            // rating without a country (AniDB's 18+ "X")
	Ratings       map[string]string // country (ISO 3166-1 alpha-2, "US") -> rating
	Date          string            // first air / release date, YYYY-MM-DD (or shorter)
}

// Rating returns the rating of the first listed country that has one, else
// the source's country-less rating. Ratings outside the US are written the
// way Plex's own agents write them: "de/12".
func (d Details) Rating(countries []string) string {
	for _, c := range countries {
		if r := d.Ratings[strings.ToUpper(c)]; r != "" {
			return countryRating(strings.ToUpper(c), r)
		}
	}
	return d.ContentRating
}

// AnyRating returns a rating from any country (US first, then
// alphabetical), for when no source has one for the listed countries.
func (d Details) AnyRating() string {
	if r := d.Ratings["US"]; r != "" {
		return r
	}
	keys := make([]string, 0, len(d.Ratings))
	for c := range d.Ratings {
		keys = append(keys, c)
	}
	sort.Strings(keys)
	for _, c := range keys {
		if d.Ratings[c] != "" {
			return countryRating(c, d.Ratings[c])
		}
	}
	return ""
}

func countryRating(country, rating string) string {
	if country == "US" {
		return rating
	}
	return strings.ToLower(country) + "/" + rating
}

func (d *Details) addRating(country, rating string) {
	if country == "" || rating == "" {
		return
	}
	if d.Ratings == nil {
		d.Ratings = map[string]string{}
	}
	if _, ok := d.Ratings[country]; !ok {
		d.Ratings[country] = rating
	}
}

// Person is one cast member: the actor and the character they play.
type Person struct {
	Name, Role, Photo string
}

func appendNew(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}
