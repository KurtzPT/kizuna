package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// longShow is the build of a long-running show as the provider records it:
// 1,100 episodes in 20 seasons with everything sent for each, the images
// of the show and every season, a full trace and the show item.
func longShow() *Recorder {
	const episodes, seasons = 1100, 20
	r := NewRecorder("tvdb-81797", "tvdb")
	summary := strings.Repeat("The crew lands on a new island and meets its people, who have a problem only they can solve. ", 5)[:400]
	images := func(level string, n int, kind string, count int) ArtGroup {
		g := ArtGroup{Level: level, Kind: kind}
		if level != "show" {
			g.Season = &n
		}
		for i := range count {
			g.Images = append(g.Images, ArtImage{URL: fmt.Sprintf("https://artworks.thetvdb.com/banners/v4/series/81797/%s/s%02d-%02d-65f1c0de4a2b7.jpg", kind, n, i),
				Src: []string{"tvdb", "tmdb"}[i%2], Primary: i == 0 && kind != "logo"})
		}
		return g
	}
	res := Result{Head: ShowHead{Title: "One Piece", Orig: "ワンピース", IDs: []ID{{"anidb", "69"}, {"tvdb", "81797"}, {"tmdb", "37854"}}},
		Mapping:  []MapRow{{AID: 69, Name: "One Piece", TVDB: 81797, TVDBSeason: "1", TMDB: 37854, TMDBSeason: "1", Exceptions: 40}},
		Complete: true, EpsTotal: episodes}
	for _, kind := range []string{"poster", "background", "logo"} {
		res.Art = append(res.Art, images("show", 0, kind, 10))
	}
	for n := 1; n <= seasons; n++ {
		name := fmt.Sprintf("Season %d", n)
		res.Seasons = append(res.Seasons, Season{Name: name, Count: episodes / seasons, Index: n,
			Thumb: "https://artworks.thetvdb.com/banners/v4/season/poster.jpg", Art: "https://artworks.thetvdb.com/banners/v4/season/background.jpg", Date: "1999-10-20"})
		res.Art = append(res.Art, images(name, n, "poster", 10), images(name, n, "background", 10), images(name, n, "logo", 5))
		for _, kind := range []string{"posters", "backgrounds", "logos"} {
			r.Field(Field{Name: name + " " + kind, Order: []string{"tvdb", "tmdb"}, Asked: []string{"tvdb", "tmdb"},
				Offered: map[string]string{"tvdb": "season " + fmt.Sprint(n) + ": 6 " + kind, "tmdb": "4 " + kind}, Used: []string{"tvdb", "tmdb"}})
		}
	}
	for i := 1; i <= episodes; i++ {
		res.Episodes = append(res.Episodes, Episode{Season: (i-1)/(episodes/seasons) + 1, Index: i,
			Title: fmt.Sprintf("Episode title number %d, of a usual length", i), From: "anidb",
			Titles:  map[string]string{"anidb": fmt.Sprintf("Episode title number %d, of a usual length", i), "tvdb": fmt.Sprintf("Another title for episode %d", i)},
			Pairs:   map[string]string{"anidb": fmt.Sprintf("69 · %d", i), "tmdb": fmt.Sprintf("S%02dE%02d", (i-1)/55+1, (i-1)%55+1)},
			Summary: summary, Date: "1999-10-20", Thumb: fmt.Sprintf("https://artworks.thetvdb.com/banners/episodes/81797/%d.jpg", 360000+i), Minutes: 24,
			Directors: []string{"Konosuke Uda"}, Writers: []string{"Junki Takegami", "Hirohiko Kamisaka"},
			Src: map[string]string{"summary": "tvdb", "date": "tvdb", "thumb": "tvdb", "minutes": "tvdb", "directors": "tvdb", "writers": "tmdb"}})
	}
	item := map[string]any{"ratingKey": "tvdb-81797", "title": "One Piece", "summary": summary, "year": 1999}
	var roles, sent []map[string]any
	for i := range 40 {
		roles = append(roles, map[string]any{"tag": fmt.Sprintf("Voice Actor %d", i), "role": fmt.Sprintf("Character %d", i),
			"thumb": fmt.Sprintf("https://artworks.thetvdb.com/banners/person/%d.jpg", i), "order": i + 1})
	}
	for _, g := range res.Art[:3] {
		for _, im := range g.Images {
			sent = append(sent, map[string]any{"type": g.Kind, "url": im.URL, "alt": "One Piece"})
		}
	}
	item["Role"], item["Image"] = roles, sent
	res.Item, _ = json.Marshal(item)
	start := r.b.Start
	for i := range maxTraceRows {
		r.Span("tvdb", fmt.Sprintf("tvdb/episodes/%d-extended.json", 360000+i), "", SpanCache, start.Add(time.Duration(i)*time.Millisecond), time.Millisecond)
	}
	r.Finish(3*time.Second, nil, res)
	return r
}

// One flush of a long show's page, as Run does it every flushEvery while
// Plex asks about the show: the saved data is read, one request is added,
// and the page and its data are written again. The index is not written.
// It also reports the size of the two files.
func BenchmarkFlushLongShow(b *testing.B) {
	dir := b.TempDir()
	s, err := Open(dir, "bench")
	if err != nil {
		b.Fatal(err)
	}
	const key = "anidb-69"
	request := func(i int) Request {
		return Request{Key: key, Time: time.Now(), Mode: "tvdb", Method: "GET", Status: 200, Bytes: 20000, TookMS: 1.5,
			URL: fmt.Sprintf("/tvdb/library/metadata/tvdb-81797-s1/children?X-Plex-Container-Start=%d&X-Plex-Container-Size=20", i*20), Answer: Answer{Kind: AnswerMemory, Age: 6}}
	}
	s.AddBuild(key, longShow())
	for i := range maxRequests {
		s.AddRequest(request(i))
	}
	s.Flush() // the page as it is on a long scan, and the index
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		s.AddRequest(request(maxRequests + i))
		s.flush(false)
	}
	b.StopTimer()
	for name, unit := range map[string]string{key + ".html": "html-bytes", filepath.Join("data", key+".json"): "json-bytes"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(st.Size()), unit)
	}
	var doc ShowDoc
	data, _ := os.ReadFile(filepath.Join(dir, "data", key+".json"))
	if err := json.Unmarshal(data, &doc); err != nil || len(doc.Builds) != 1 || len(doc.Builds[0].Episodes) != 1100 || len(doc.Requests) != maxRequests {
		b.Fatalf("the page after the flushes (%v): %d builds, %d requests", err, len(doc.Builds), len(doc.Requests))
	}
}
