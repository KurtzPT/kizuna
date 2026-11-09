package sources

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"kizuna/internal/fetch"
)

// TMDB answers shaped as its API documentation describes them
// (developer.themoviedb.org: TV Series Details with aggregate_credits and
// content_ratings, Movie Details with credits and release_dates), nulls
// where TMDB sends them.

const tmdbQ = "?api_key=key&language=en-US"

func newTMDBTest(t *testing.T, answers map[string]string) *TMDB {
	t.Helper()
	f := fetch.New(t.TempDir())
	f.SetTransport(&tvdbNet{answers: answers}) // tvdbNet answers any host
	return NewTMDB(f, "key")
}

// aggCrew is an aggregate_credits crew member: jobs with episode counts.
func aggCrew(id int, name, department string, episodes int, jobs ...string) string {
	js := "null"
	if len(jobs) > 0 {
		var parts []string
		for _, j := range jobs {
			parts = append(parts, fmt.Sprintf(`{"credit_id": "c%d", "job": %q, "episode_count": %d}`, id, j, episodes))
		}
		js = "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprintf(`{"adult": false, "gender": 2, "id": %d, "known_for_department": %q, "name": %q,
	  "original_name": %q, "popularity": 1.5, "profile_path": null, "jobs": %s, "department": %q,
	  "total_episode_count": %d}`, id, department, name, name, js, department, episodes)
}

const tmdbTVDetails = `{
  "adult": false, "backdrop_path": "/b.jpg", "created_by": [], "episode_run_time": [24],
  "first_air_date": "2023-09-29", "genres": [{"id": 16, "name": "Animation"}, {"id": 18, "name": "Drama"}],
  "homepage": "", "id": 209867, "in_production": false, "languages": ["ja"], "last_air_date": "2024-03-22",
  "name": "Frieren: Beyond Journey's End", "networks": [], "number_of_episodes": 28, "number_of_seasons": 1,
  "origin_country": ["JP"], "original_language": "ja", "original_name": "Sousou no Frieren", "overview": "",
  "popularity": 100, "poster_path": "/p.jpg",
  "production_companies": [{"id": 3464, "logo_path": null, "name": "Madhouse", "origin_country": "JP"},
    {"id": 1, "logo_path": null, "name": "Other", "origin_country": "JP"}],
  "seasons": [], "status": "Ended", "type": "Scripted", "vote_average": 8.8, "vote_count": 400,
  "aggregate_credits": {
    "cast": [
      {"adult": false, "gender": 1, "id": 1, "known_for_department": "Acting", "name": "Second", "original_name": "",
        "popularity": 1, "profile_path": null, "roles": [{"credit_id": "r1", "character": "Fern", "episode_count": 28}],
        "total_episode_count": 28, "order": 1},
      {"adult": false, "gender": 1, "id": 2, "known_for_department": "Acting", "name": "Atsumi Tanezaki", "original_name": "",
        "popularity": 1, "profile_path": "/a.jpg",
        "roles": [{"credit_id": "r2", "character": "Frieren", "episode_count": 28}, {"credit_id": "r3", "character": "Young Frieren", "episode_count": 2}],
        "total_episode_count": 28, "order": 0},
      {"adult": false, "gender": 2, "id": 3, "known_for_department": "Acting", "name": "No Roles", "original_name": "",
        "popularity": 1, "profile_path": null, "roles": null, "total_episode_count": 1, "order": 2}
    ],
    "crew": [
      ` + "%s" + `
    ]
  },
  "content_ratings": {"results": [
    {"descriptors": [], "iso_3166_1": "US", "rating": "TV-14"},
    {"descriptors": [], "iso_3166_1": "US", "rating": "TV-MA"},
    {"descriptors": [], "iso_3166_1": "DE", "rating": "12"},
    {"descriptors": [], "iso_3166_1": "JP", "rating": ""}
  ]}
}`

// TV details: cast by order, a person's first role, no photo without a
// profile; directors (Director, Series Director) on the most episodes, at
// most 5; writers by department or job; the first rating per country, empty
// ones skipped.
func TestTMDBTVDetails(t *testing.T) {
	crew := strings.Join([]string{
		aggCrew(10, "Ep Director", "Directing", 1, "Episode Director"),
		aggCrew(11, "Keiichiro Saito", "Directing", 28, "Director"),
		aggCrew(12, "Dir Two", "Directing", 4, "Director"),
		aggCrew(13, "Dir Three", "Directing", 3, "Director"),
		aggCrew(14, "Dir Four", "Directing", 2, "Director"),
		aggCrew(15, "Dir Five", "Directing", 2, "Series Director"),
		aggCrew(16, "Dir Six", "Directing", 1, "Director"),
		aggCrew(17, "Tomohiro Suzuki", "Writing", 28, "Series Composition"),
		aggCrew(18, "Kanehito Yamada", "Writing", 0, "Original Story"),
		aggCrew(19, "Jobless Writer", "Writing", 1),
		aggCrew(20, "Composer", "Sound", 28, "Music"),
	}, ",\n")
	tm := newTMDBTest(t, map[string]string{
		"api.themoviedb.org/3/tv/209867" + tmdbQ + "&append_to_response=aggregate_credits,content_ratings": fmt.Sprintf(tmdbTVDetails, crew),
	})
	d, err := tm.Details(context.Background(), 209867, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Date != "2023-09-29" || strings.Join(d.Genres, ",") != "Animation,Drama" || d.Studio != "Madhouse" {
		t.Errorf("date %q genres %v studio %q", d.Date, d.Genres, d.Studio)
	}
	wantCast := []Person{{Name: "Atsumi Tanezaki", Role: "Frieren", Photo: tmdbImg + "/a.jpg"}, {Name: "Second", Role: "Fern"}, {Name: "No Roles"}}
	if !slices.Equal(d.Cast, wantCast) {
		t.Errorf("cast %+v, want %+v", d.Cast, wantCast)
	}
	if got := strings.Join(d.Directors, ","); got != "Keiichiro Saito,Dir Two,Dir Three,Dir Four,Dir Five" {
		t.Errorf("directors %q", got)
	}
	if got := strings.Join(d.Writers, ","); got != "Tomohiro Suzuki,Jobless Writer,Kanehito Yamada" {
		t.Errorf("writers %q", got)
	}
	if len(d.Ratings) != 2 || d.Ratings["US"] != "TV-14" || d.Ratings["DE"] != "12" {
		t.Errorf("ratings %v", d.Ratings)
	}
}

// Movie details: credits' character and job, a cast of at most 50 by order,
// and the certification of each country's first release that has one.
func TestTMDBMovieDetails(t *testing.T) {
	var cast []string
	for i := range 55 {
		cast = append(cast, fmt.Sprintf(`{"adult": false, "gender": 0, "id": %d, "known_for_department": "Acting",
		  "name": "Actor %d", "original_name": "", "popularity": 1, "profile_path": null, "cast_id": %d,
		  "character": "Role %d", "credit_id": "c", "order": %d}`, i, i, i, i, 54-i))
	}
	body := `{"adult": false, "genres": [{"id": 16, "name": "Animation"}], "id": 129, "release_date": "2001-07-20",
	  "production_companies": [{"id": 10342, "logo_path": null, "name": "Studio Ghibli", "origin_country": "JP"}],
	  "title": "Spirited Away", "overview": "",
	  "credits": {"cast": [` + strings.Join(cast, ",") + `], "crew": [
	    {"adult": false, "gender": 2, "id": 608, "known_for_department": "Directing", "name": "Hayao Miyazaki",
	      "original_name": "", "popularity": 1, "profile_path": null, "credit_id": "d", "department": "Directing", "job": "Director"},
	    {"adult": false, "gender": 2, "id": 608, "known_for_department": "Directing", "name": "Hayao Miyazaki",
	      "original_name": "", "popularity": 1, "profile_path": null, "credit_id": "w", "department": "Writing", "job": "Screenplay"},
	    {"adult": false, "gender": 2, "id": 9, "known_for_department": "Directing", "name": "Assistant",
	      "original_name": "", "popularity": 1, "profile_path": null, "credit_id": "a", "department": "Directing", "job": "Assistant Director"}
	  ]},
	  "release_dates": {"results": [
	    {"iso_3166_1": "US", "release_dates": [
	      {"certification": "", "descriptors": [], "iso_639_1": "", "note": "", "release_date": "2002-09-20T00:00:00.000Z", "type": 3},
	      {"certification": "PG", "descriptors": [], "iso_639_1": "", "note": "", "release_date": "2002-09-20T00:00:00.000Z", "type": 3},
	      {"certification": "G", "descriptors": [], "iso_639_1": "", "note": "", "release_date": "2003-04-15T00:00:00.000Z", "type": 5}]},
	    {"iso_3166_1": "JP", "release_dates": [
	      {"certification": "G", "descriptors": [], "iso_639_1": "", "note": "", "release_date": "2001-07-20T00:00:00.000Z", "type": 3}]},
	    {"iso_3166_1": "FR", "release_dates": [
	      {"certification": "", "descriptors": [], "iso_639_1": "", "note": "", "release_date": "2002-04-10T00:00:00.000Z", "type": 3}]}
	  ]}
	}`
	tm := newTMDBTest(t, map[string]string{
		"api.themoviedb.org/3/movie/129" + tmdbQ + "&append_to_response=credits,release_dates": body,
	})
	d, err := tm.Details(context.Background(), 129, true)
	if err != nil {
		t.Fatal(err)
	}
	if d.Date != "2001-07-20" || d.Studio != "Studio Ghibli" {
		t.Errorf("date %q studio %q", d.Date, d.Studio)
	}
	if len(d.Cast) != 50 || d.Cast[0] != (Person{Name: "Actor 54", Role: "Role 54"}) || d.Cast[49].Name != "Actor 5" {
		t.Errorf("%d cast, first %+v, last %+v: want 50 by order", len(d.Cast), d.Cast[0], d.Cast[len(d.Cast)-1])
	}
	if strings.Join(d.Directors, ",") != "Hayao Miyazaki" || strings.Join(d.Writers, ",") != "Hayao Miyazaki" {
		t.Errorf("directors %v writers %v", d.Directors, d.Writers)
	}
	if len(d.Ratings) != 2 || d.Ratings["US"] != "PG" || d.Ratings["JP"] != "G" {
		t.Errorf("ratings %v", d.Ratings)
	}
	if d, err := tm.Details(context.Background(), 0, true); err != nil || len(d.Genres) != 0 {
		t.Errorf("id 0: %+v %v, want nothing", d, err)
	}
}

// The episode list: a season TMDB lists but answers 404 for is skipped
// quietly; other failures leave it incomplete, naming the seasons in order.
// Posters only for seasons that have them; null crew is no crew.
func TestTMDBEpisodesAndPosters(t *testing.T) {
	const sq = tmdbQ + "&append_to_response=images&include_image_language=en,ja,null"
	const season = "api.themoviedb.org/3/tv/77/season/"
	answers := map[string]string{
		"api.themoviedb.org/3/tv/77" + tmdbQ: `{"name": "Show", "seasons": [{"season_number": 0}, {"season_number": 1},
		  {"season_number": 2}, {"season_number": 3}, {"season_number": 4}]}`,
		// 0: 404
		season + "1" + sq: `{"_id": "a", "air_date": "2020-01-01", "name": "Season 1", "overview": "", "id": 1, "season_number": 1,
		  "episodes": [
		    {"air_date": "2020-01-08", "episode_number": 2, "episode_type": "standard", "id": 12, "name": "Two", "overview": "",
		      "runtime": null, "season_number": 1, "show_id": 77, "still_path": null, "vote_average": 0, "vote_count": 0,
		      "crew": null, "guest_stars": null},
		    {"air_date": "2020-01-01", "episode_number": 1, "episode_type": "standard", "id": 11, "name": "One", "overview": "First.",
		      "runtime": 24, "season_number": 1, "show_id": 77, "still_path": "/s.jpg", "vote_average": 0, "vote_count": 0,
		      "crew": [{"job": "Director", "department": "Directing", "name": " D "},
		        {"job": "Screenplay", "department": "Writing", "name": "W"}, {"job": "Writer", "department": "Writing", "name": "W"},
		        {"job": "Storyboard", "department": "Directing", "name": "S"}],
		      "guest_stars": []}],
		  "images": {"posters": [{"file_path": "/p1.jpg", "iso_639_1": "en"}]}}`,
		season + "2" + sq: `503 {"status_code": 46, "status_message": "Service unavailable"}`,
		season + "3" + sq: `500 {"status_code": 11, "status_message": "Internal error"}`,
		season + "4" + sq: `{"episodes": [{"episode_number": 1, "name": "Four"}], "images": {"posters": []}}`,
	}
	captureLogs(t) // the failed seasons' warnings
	eps, posters, err := newTMDBTest(t, answers).EpisodesAndPosters(context.Background(), 77)
	if !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), "seasons [2 3]") {
		t.Errorf("err %v, want incomplete, seasons [2 3]", err)
	}
	if len(eps) != 3 || eps[0].Title != "One" || eps[1].Title != "Two" || eps[2].Season != 4 {
		t.Fatalf("episodes %+v", eps)
	}
	if e := eps[0]; e.Thumb != tmdbImg+"/s.jpg" || e.Minutes != 24 || e.Aired != "2020-01-01" || e.Summary != "First." ||
		strings.Join(e.Directors, ",") != "D" || strings.Join(e.Writers, ",") != "W" {
		t.Errorf("episode 1 %+v", e)
	}
	if e := eps[1]; e.Thumb != "" || len(e.Directors)+len(e.Writers) != 0 {
		t.Errorf("episode 2 (nulls) %+v", e)
	}
	if len(posters) != 1 || strings.Join(posters[1], ",") != tmdbImg+"/p1.jpg" {
		t.Errorf("posters %v, want season 1's only", posters)
	}

	// Without the failing seasons the list is complete: the 404 is no failure.
	delete(answers, season+"2"+sq)
	delete(answers, season+"3"+sq)
	if eps, _, err := newTMDBTest(t, answers).EpisodesAndPosters(context.Background(), 77); err != nil || len(eps) != 3 {
		t.Errorf("only 404s: %d episodes, err %v, want 3 and none", len(eps), err)
	}
}
