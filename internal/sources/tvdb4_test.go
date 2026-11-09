package sources

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kizuna/internal/fetch"
)

// TheTVDB v4 answers shaped exactly as its API documentation describes them
// (https://thetvdb.github.io/v4-api/, swagger.yml): every documented field,
// nulls where the schema allows them, so a parsing mismatch shows up here.

const v4 = "api4.thetvdb.com/v4"

// /series/{id}/episodes/{season-type}/{lang}: data.series is a
// SeriesBaseRecord, its episodes are EpisodeBaseRecords.
const v4EpisodesEng = `{
  "data": {
    "series": {
      "aliases": [{"language": "eng", "name": "AG"}],
      "averageRuntime": 24,
      "country": "jpn",
      "defaultSeasonType": 1,
      "episodes": [
        {
          "absoluteNumber": 1, "aired": "2006-04-04", "airsAfterSeason": 0, "airsBeforeEpisode": 0,
          "airsBeforeSeason": 0, "finaleType": "", "id": 3001, "image": "/banners/episodes/79796/3001.jpg",
          "imageType": 11, "isMovie": 0, "lastUpdated": "2024-01-01 10:00:00", "linkedMovie": 0,
          "name": "The Invincible Baby Face", "nameTranslations": ["eng", "jpn"], "number": 1,
          "overview": "First.", "overviewTranslations": ["eng"], "runtime": 24, "seasonNumber": 1,
          "seasons": [{"id": 1, "image": "", "imageType": 0, "lastUpdated": "", "name": "Season 1",
            "nameTranslations": [], "number": 1, "overviewTranslations": [], "companies": {}, "seriesId": 79796,
            "type": {"alternateName": "", "id": 1, "name": "Aired Order", "type": "official"}, "year": "2006"}],
          "seriesId": 79796, "seasonName": "Season 1", "year": "2006"
        },
        {
          "absoluteNumber": 2, "aired": "2006-04-11", "finaleType": null, "id": 3002, "image": "",
          "imageType": null, "isMovie": 0, "lastUpdated": "", "linkedMovie": null, "name": "Two",
          "nameTranslations": null, "number": 2, "overview": "", "overviewTranslations": null,
          "runtime": null, "seasonNumber": 1, "seasons": null, "seriesId": 79796, "seasonName": null, "year": "2006"
        },
        {
          "absoluteNumber": null, "aired": null, "id": 3003, "image": null, "isMovie": 0, "name": "Special",
          "number": 1, "overview": null, "runtime": 12, "seasonNumber": 0, "seriesId": 79796
        }
      ],
      "firstAired": "2006-04-04", "id": 79796, "image": "/banners/posters/79796-1.jpg",
      "isOrderRandomized": false, "lastAired": "2006-09-26", "lastUpdated": "2024-01-01 10:00:00",
      "name": "Air Gear", "nameTranslations": ["eng"], "nextAired": "", "originalCountry": "jpn",
      "originalLanguage": "jpn", "overviewTranslations": ["eng"], "score": 1000, "slug": "air-gear",
      "status": {"id": 2, "keepUpdated": false, "name": "Ended", "recordType": "series"}, "year": "2006"
    }
  },
  "status": "success",
  "links": {"prev": null, "self": "https://api4.thetvdb.com/v4/series/79796/episodes/official/eng?page=0",
    "next": null, "total_items": 3, "page_size": 500}
}`

// character is a documented Character record.
func character(id int, person, peopleType string, sort int) string {
	return `{"aliases": [], "episode": {"image": "", "name": "", "year": ""}, "episodeId": 3001, "id": ` + strconv.Itoa(id) + `,
	  "image": "", "isFeatured": false, "movieId": null, "movie": {"image": null, "name": null, "year": null},
	  "name": "", "nameTranslations": [], "overviewTranslations": [], "peopleId": ` + strconv.Itoa(id+100) + `,
	  "personImgURL": "", "peopleType": "` + peopleType + `", "seriesId": null,
	  "series": {"image": "", "name": "", "year": ""}, "sort": ` + strconv.Itoa(sort) + `, "tagOptions": [],
	  "type": 1, "url": "", "personName": "` + person + `"}`
}

// /episodes/{id}/extended: data is an EpisodeExtendedRecord.
var v4EpisodeExtended = `{
  "data": {
    "aired": "2006-04-04", "airsAfterSeason": 0, "airsBeforeEpisode": 0, "airsBeforeSeason": 0,
    "awards": [], "characters": [
      ` + character(1, "Kenji Nojima", "Actor", 0) + `,
      ` + character(2, "Oogure Ito", "Writer", 3) + `,
      ` + character(3, "Kamegaki Hajime", "Director", 2) + `,
      ` + character(4, "Guest Person", "Guest Star", 1) + `,
      ` + character(5, "Second Director", "Director", 4) + `,
      ` + character(6, "Kamegaki Hajime", "Director", 5) + `
    ],
    "companies": [], "contentRatings": [], "finaleType": "", "id": 3001, "image": "", "imageType": 11,
    "isMovie": 0, "lastUpdated": "", "linkedMovie": 0, "name": "The Invincible Baby Face",
    "nameTranslations": [], "networks": [], "nominations": [], "number": 1, "overview": "",
    "overviewTranslations": [], "productionCode": "", "remoteIds": [], "runtime": 24, "seasonNumber": 1,
    "seasons": [], "seriesId": 79796, "studios": [], "tagOptions": [], "trailers": [],
    "translations": {"nameTranslations": [], "overviewTranslations": [], "alias": []}, "year": "2006"
  },
  "status": "success"
}`

func TestTVDBV4DocumentedAnswers(t *testing.T) {
	tv, n := newTVDBTest(t, map[string]string{
		v4 + "/login": `{"data": {"token": "t4"}, "status": "success"}`,
		v4 + "/series/79796/episodes/official/eng?page=0": v4EpisodesEng,
		v4 + "/episodes/3001/extended":                    v4EpisodeExtended,
		v4 + "/episodes/3002/extended":                    `{"data": {"id": 3002, "characters": null}, "status": "success"}`,
		// 3003: 404, as documented for an unknown id
	}, "v4key", "", "")
	ctx := context.Background()

	eps, err := tv.Episodes(ctx, 79796)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 3 {
		t.Fatalf("got %d episodes, want 3: %+v", len(eps), eps)
	}
	if e := eps[0]; e.ID != 3001 || e.Season != 1 || e.Number != 1 || e.Absolute != 1 || e.Title != "The Invincible Baby Face" ||
		e.Aired != "2006-04-04" || e.Minutes != 24 || e.Thumb != "https://artworks.thetvdb.com/banners/episodes/79796/3001.jpg" {
		t.Errorf("first episode %+v", e)
	}
	if e := eps[1]; e.ID != 3002 || e.Minutes != 0 || e.Thumb != "" {
		t.Errorf("second episode (nulls) %+v", e)
	}
	if e := eps[2]; e.ID != 3003 || e.Season != 0 || e.Absolute != 0 { // specials last
		t.Errorf("special %+v", e)
	}
	for _, e := range eps {
		if len(e.Directors)+len(e.Writers) > 0 {
			t.Errorf("episode %d: the v4 list has no crew, got %v %v", e.ID, e.Directors, e.Writers)
		}
	}

	crew, pending := tv.EpisodeCrew(ctx, []int{3001, 3002, 3003})
	if pending {
		t.Error("pending: everything answered within the budget")
	}
	if c := crew[3001]; strings.Join(c.Directors, ",") != "Kamegaki Hajime,Second Director" || strings.Join(c.Writers, ",") != "Oogure Ito" {
		t.Errorf("3001: directors %q writers %q (by sort, no actors or guest stars, no repeats)", c.Directors, c.Writers)
	}
	if c, ok := crew[3002]; !ok || len(c.Directors)+len(c.Writers) != 0 {
		t.Errorf("3002 (no characters): %+v %v", c, ok)
	}
	if _, ok := crew[3003]; ok {
		t.Error("3003 has no record: want no crew")
	}
	for _, u := range n.asked {
		if strings.HasPrefix(u, v3) {
			t.Errorf("v4 key, but v3 was called: %s", u)
		}
	}
}

// season is a documented SeasonBaseRecord, as listed by /series/{id}/extended.
func season(id, number int, kind, name string) string {
	return `{"id": ` + strconv.Itoa(id) + `, "image": "", "imageType": 7, "lastUpdated": "2024-01-01 10:00:00",
	  "name": "", "nameTranslations": null, "number": ` + strconv.Itoa(number) + `, "overviewTranslations": null,
	  "companies": {"studio": null, "network": null, "production": null, "distributor": null, "special_effects": null},
	  "seriesId": 267440, "type": {"alternateName": null, "id": 1, "name": "` + name + `", "type": "` + kind + `"}, "year": "2017"}`
}

// /series/{id}/extended: data is a SeriesExtendedRecord; its seasons list
// every season type, each with its own id.
var v4SeriesExtended = `{
  "data": {
    "abbreviation": null, "airsDays": {}, "airsTime": "", "aliases": [], "artworks": [], "averageRuntime": 24,
    "characters": [], "contentRatings": [], "country": "jpn", "defaultSeasonType": 1, "episodes": null,
    "firstAired": "2013-04-07", "lists": [], "genres": [], "id": 267440, "image": "", "isOrderRandomized": false,
    "lastAired": "2023-11-05", "lastUpdated": "", "name": "Attack on Titan", "nameTranslations": ["eng"],
    "companies": [], "nextAired": "", "originalCountry": "jpn", "originalLanguage": "jpn",
    "seasons": [
      ` + season(500, 0, "official", "Aired Order") + `,
      ` + season(501, 1, "official", "Aired Order") + `,
      ` + season(612, 2, "dvd", "DVD Order") + `,
      ` + season(502, 2, "official", "Aired Order") + `,
      ` + season(700, 1, "absolute", "Absolute Order") + `
    ],
    "seasonTypes": [{"alternateName": null, "id": 1, "name": "Aired Order", "type": "official"}],
    "slug": "attack-on-titan", "status": {"id": 2, "keepUpdated": false, "name": "Ended", "recordType": "series"},
    "translations": {"nameTranslations": [], "overviewTranslations": [], "alias": []}, "year": "2013"
  },
  "status": "success"
}`

// A season's overview is its aired-order ("official") season's English
// translation; the series record that lists the seasons is the one Details
// reads. A season TheTVDB doesn't list, or one with no English text
// (documented as 400 "language not found", or a 404), gives no text and no
// error.
func TestTVDBSeasonOverview(t *testing.T) {
	tv, n := newTVDBTest(t, map[string]string{
		v4 + "/login":                  `{"data": {"token": "t4"}, "status": "success"}`,
		v4 + "/series/267440/extended": v4SeriesExtended,
		v4 + "/seasons/502/translations/eng": `{"data": {"aliases": null, "isAlias": false, "isPrimary": true,
		  "language": "eng", "name": "Season 2", "overview": "The Scouts face the Beast Titan.", "tagline": null},
		  "status": "success"}`,
		v4 + "/seasons/501/translations/eng": `400 {"status": "failure", "message": "language not found"}`,
	}, "v4key", "", "")
	ctx := context.Background()
	if got, err := tv.SeasonOverview(ctx, 267440, 2); err != nil || got != "The Scouts face the Beast Titan." {
		t.Errorf("season 2: %q %v", got, err)
	}
	for _, s := range []int{1, 3, 0, -1} { // 1: no English; 3: not listed; 0, -1: no season of its own
		if got, err := tv.SeasonOverview(ctx, 267440, s); err != nil || got != "" {
			t.Errorf("season %d: %q %v, want none", s, got, err)
		}
	}
	if got, err := tv.SeasonOverview(ctx, 99, 2); got != "" || !fetch.IsNotFound(err) {
		t.Errorf("unknown series: %q %v, want its 404", got, err)
	}
	if _, err := tv.Details(ctx, 267440); err != nil {
		t.Fatal(err)
	}
	if c := n.count(v4 + "/series/267440/extended"); c != 1 {
		t.Errorf("series record asked %d times, want 1 (cached, shared with Details)", c)
	}
	for _, id := range []string{"500", "612", "700"} { // not the aired-order season asked for
		if c := n.count(v4 + "/seasons/" + id + "/translations/eng"); c != 0 {
			t.Errorf("season record %s fetched %d times", id, c)
		}
	}
}

// A season with no English text is asked once per seasonTTL, not on every
// build, and its 400 is no warning: it's an answer, not a failure.
func TestTVDBSeasonWithoutEnglishOnce(t *testing.T) {
	logs := captureLogs(t)
	tv, n := newTVDBTest(t, map[string]string{
		v4 + "/login":                        `{"data": {"token": "t4"}, "status": "success"}`,
		v4 + "/series/267440/extended":       v4SeriesExtended,
		v4 + "/seasons/501/translations/eng": `400 {"status": "failure", "message": "language not found"}`,
	}, "v4key", "", "")
	ctx := context.Background()
	const u = v4 + "/seasons/501/translations/eng"
	for range 2 { // two builds of the same entry
		if got, err := tv.SeasonOverview(ctx, 267440, 1); got != "" || err != nil {
			t.Fatalf("season 1: %q %v, want none", got, err)
		}
	}
	if c := n.count(u); c != 1 {
		t.Errorf("asked %d times, want 1", c)
	}
	if s := logs.String(); s != "" {
		t.Errorf("logged above debug:\n%s", s)
	}

	tv.seasonMu.Lock()
	tv.noEnglish[501] = time.Now().Add(-time.Second) // its time is up
	tv.seasonMu.Unlock()
	_, _ = tv.SeasonOverview(ctx, 267440, 1)
	if c := n.count(u); c != 2 {
		t.Errorf("after seasonTTL: asked %d times in all, want 2", c)
	}
}

// The extended series record that SeasonOverview and Details both read is
// decoded once while its cache file keeps its time; a file written since is
// read again. Records kept for seriesMemoFor go at the next one kept.
func TestTVDBSeriesRecordOnce(t *testing.T) {
	dir := t.TempDir()
	f := fetch.New(dir)
	f.SetTransport(&tvdbNet{answers: map[string]string{
		v4 + "/login":                  `{"data": {"token": "t4"}, "status": "success"}`,
		v4 + "/series/267440/extended": v4SeriesExtended,
		v4 + "/series/1/extended":      `{"data": {"firstAired": "2001-01-01"}}`,
	}})
	tv := NewTVDB(f, "v4key", "", "")
	ctx := context.Background()
	if _, err := tv.SeasonOverview(ctx, 267440, 2); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "tvdb", "267440", "extended.json")
	_, at, ok := f.CachedSince("tvdb/267440/extended.json")
	if !ok {
		t.Fatal("series record not cached")
	}
	if err := os.WriteFile(file, []byte(`{"data": {"firstAired": "2099-01-01"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, at, at); err != nil { // other bytes, same time: the decoded record stands
		t.Fatal(err)
	}
	if d, err := tv.Details(ctx, 267440); err != nil || d.Date != "2013-04-07" {
		t.Errorf("Details after SeasonOverview: %q %v, want the record already decoded", d.Date, err)
	}
	now := time.Now()
	if err := os.Chtimes(file, now, now); err != nil { // written since
		t.Fatal(err)
	}
	if d, err := tv.Details(ctx, 267440); err != nil || d.Date != "2099-01-01" {
		t.Errorf("Details after the file changed: %q %v, want it read again", d.Date, err)
	}

	tv.seriesMu.Lock()
	m := tv.series[267440]
	m.kept = now.Add(-seriesMemoFor)
	tv.series[267440] = m
	tv.seriesMu.Unlock()
	if _, err := tv.Details(ctx, 1); err != nil {
		t.Fatal(err)
	}
	tv.seriesMu.Lock()
	defer tv.seriesMu.Unlock()
	if _, ok := tv.series[267440]; ok || len(tv.series) != 1 {
		t.Errorf("%d records kept, want only the new one", len(tv.series))
	}
}

// Past their time, answers of seasons not asked again are dropped when the
// next one is kept.
func TestTVDBNoEnglishSwept(t *testing.T) {
	tv, _ := newTVDBTest(t, map[string]string{
		v4 + "/login":                        `{"data": {"token": "t4"}, "status": "success"}`,
		v4 + "/series/267440/extended":       v4SeriesExtended,
		v4 + "/seasons/501/translations/eng": `400 {"status": "failure", "message": "language not found"}`,
	}, "v4key", "", "")
	tv.seasonMu.Lock()
	tv.noEnglish[900] = time.Now().Add(-time.Second) // its time is up
	tv.noEnglish[901] = time.Now().Add(time.Hour)
	tv.seasonMu.Unlock()
	if _, err := tv.SeasonOverview(context.Background(), 267440, 1); err != nil {
		t.Fatal(err)
	}
	tv.seasonMu.Lock()
	defer tv.seasonMu.Unlock()
	if _, ok := tv.noEnglish[900]; ok || len(tv.noEnglish) != 2 {
		t.Errorf("noEnglish %v, want 901 and 501", tv.noEnglish)
	}
}
