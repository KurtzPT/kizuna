package provider

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"kizuna/internal/anidb"
	"kizuna/internal/mapping"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// Benchmarks of a long show's build, on fake sources only: run them with
// -cpu 2 and a small -benchtime.

// longEps is the number of regular episodes of longSeries.
const longEps = 1100

// longSeries is AniDB 100 as a long series: longEps regular episodes with
// titles in three languages, 20 specials and 4 credits, on TheTVDB 500
// season 1 and TheMovieDb 77 season 1, both with every episode's crew (some
// with photos).
func longSeries(path string, q map[string][]string) (int, string) {
	switch path {
	case "api.anidb.net/httpapi":
		return 200, longSources()[0]
	case "api4.thetvdb.com/v4/series/500/episodes/official/eng":
		return 200, longSources()[1]
	case "api.themoviedb.org/3/tv/77":
		return 200, `{"name":"TMDB Name","seasons":[{"season_number":0},{"season_number":1}]}`
	case "api.themoviedb.org/3/tv/77/season/1":
		return 200, longSources()[2]
	}
	if id, ok := strings.CutPrefix(path, "api4.thetvdb.com/v4/episodes/"); ok {
		if id, ok = strings.CutSuffix(id, "/extended"); ok {
			n, _ := strconv.Atoi(id)
			return 200, `{"data":{"characters":[{"personName":"Director ` + strconv.Itoa(n%40) + `","peopleType":"Director","personImgURL":"/banners/person/` + strconv.Itoa(n%40) + `/d.jpg"},
				{"personName":"Writer ` + strconv.Itoa(n%25) + `","peopleType":"Writer"},{"personName":"Producer","peopleType":"Producer"}]}}`
		}
	}
	return testSeries(path, q)
}

// longSources is made once, by the first benchmark that asks.
var longSources = sync.OnceValue(func() [3]string {
	ad, tv, tm := makeLongSources()
	return [3]string{ad, tv, tm}
})

func makeLongSources() (ad, tv, tm string) {
	var a, t, m strings.Builder
	a.WriteString(`<anime id="100"><type>TV Series</type><startdate>1999-10-20</startdate>
<titles><title type="main" xml:lang="x-jat">Kizuna Test</title><title type="official" xml:lang="en">Bond Test</title></titles>
<description>From AniDB.</description><episodes>`)
	t.WriteString(`{"data":{"episodes":[`)
	m.WriteString(`{"episodes":[`)
	for i := 1; i <= longEps; i++ {
		n := strconv.Itoa(i)
		a.WriteString(`<episode><epno type="1">` + n + `</epno><length>24</length><title xml:lang="ja">第` + n + `話</title>` +
			`<title xml:lang="x-jat">Dai ` + n + ` Wa</title><title xml:lang="en">Episode Title ` + n + `</title></episode>`)
		if i > 1 {
			t.WriteString(",")
			m.WriteString(",")
		}
		t.WriteString(`{"id":` + strconv.Itoa(10000+i) + `,"seasonNumber":1,"number":` + n + `,"name":"TVDB ` + n + `","image":"/e` + n + `.jpg","aired":"2000-01-01","overview":"Text."}`)
		m.WriteString(`{"episode_number":` + n + `,"name":"TMDB ` + n + `","crew":[{"job":"Director","department":"Directing","name":"Director ` + strconv.Itoa(i%40) + `","profile_path":"/d` + strconv.Itoa(i%40) + `.jpg"},` +
			`{"job":"Screenplay","department":"Writing","name":"Writer ` + strconv.Itoa(i%25) + `"}]}`)
	}
	for i := 1; i <= 20; i++ {
		n := strconv.Itoa(i)
		a.WriteString(`<episode><epno type="2">S` + n + `</epno><title xml:lang="en">Special ` + n + `</title></episode>`)
		t.WriteString(`,{"id":` + strconv.Itoa(20000+i) + `,"seasonNumber":0,"number":` + n + `,"name":"TVDB Special ` + n + `"}`)
	}
	for i := 1; i <= 4; i++ {
		a.WriteString(`<episode><epno type="3">C` + strconv.Itoa(i) + `</epno><title xml:lang="en">Opening ` + strconv.Itoa(i) + `</title></episode>`)
	}
	a.WriteString(`</episodes></anime>`)
	t.WriteString(`]},"links":{"next":null}}`)
	m.WriteString(`]}`)
	return a.String(), t.String(), m.String()
}

// longEntry is longSeries' mapping entry, with exceptions as a long series
// has them: single episodes and specials moved, credits placed, open ranges.
func longEntry() mapping.Entry {
	tvdb := mapping.Placement{ID: 500, Season: 1, Mapped: true}
	tmdb := mapping.Placement{ID: 77, Season: 1, Mapped: true}
	for i := 1; i <= 30; i++ {
		tvdb.Episodes = append(tvdb.Episodes, mapping.EpisodeMap{AniDBSeason: 1, AniDBEp: i * 30, Season: 1, Ep: i*30 + 1})
		tmdb.Episodes = append(tmdb.Episodes, mapping.EpisodeMap{AniDBSeason: 1, AniDBEp: i * 31, Season: 1, Ep: i * 31})
	}
	for i := 1; i <= 10; i++ {
		tvdb.Episodes = append(tvdb.Episodes, mapping.EpisodeMap{AniDBSeason: 0, AniDBEp: i, Season: 0, Ep: i + 5})
	}
	tvdb.Episodes = append(tvdb.Episodes, mapping.EpisodeMap{AniDBSeason: 0, AniDBEp: 101, Season: 0, Ep: 1},
		mapping.EpisodeMap{AniDBSeason: 1, AniDBEp: 7, Season: 1, Ep: 0}) // a credit placed; an episode with no counterpart
	tvdb.Ranges = []mapping.EpisodeRange{{AniDBSeason: 1, From: 1000, To: 1 << 30, Season: 1, Offset: 1}}
	tmdb.Ranges = []mapping.EpisodeRange{{AniDBSeason: 1, From: 900, To: 1 << 30, Season: 1, Offset: -1}}
	return mapping.Entry{AID: 100, TVDB: tvdb, TMDB: tmdb}
}

// longProvider is a provider on longSeries with every answer cached.
func longProvider(tb testing.TB) *Provider {
	tb.Helper()
	defaultLists(tb)
	p := newTestProvider(tb, &fakeNet{answer: longSeries})
	p.lists.SetForTest(map[int]mapping.Entry{100: longEntry()})
	p.titles.SetForTest(map[int][]anidb.Title{100: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test"}}})
	if _, err := p.buildShow(context.Background(), "anidb-100"); err != nil { // fills the cache
		tb.Fatal(err)
	}
	return p
}

func BenchmarkLongAniDBBuild(b *testing.B) {
	p := longProvider(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := p.buildShow(context.Background(), "anidb-100"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCrewMemorySave is what a build that learned a person's first
// photo spends saving the memory, by the number of photos it holds.
func BenchmarkCrewMemorySave(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			m := loadCrewMemory(b.TempDir())
			for i := range size {
				m.learn("person "+strconv.Itoa(i), "https://image.tmdb.org/t/p/original/person"+strconv.Itoa(i)+".jpg")
			}
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				m.learn("new person", "https://image.tmdb.org/t/p/original/"+strconv.Itoa(i)+".jpg")
				m.save()
			}
		})
	}
}

// BenchmarkFinishCrewReplaces is the end of a build that replaced one photo,
// with 10,000 photos remembered in a data folder: what the build's waiters
// wait for (the save itself runs later, BenchmarkCrewMemorySave; a person's
// first photo is saved inside the build and costs that too).
func BenchmarkFinishCrewReplaces(b *testing.B) {
	p := &Provider{crewMemory: loadCrewMemory(b.TempDir())}
	manualSaves(p.crewMemory) // no save lands inside the measurement, or after the folder goes
	p.cfg.EpisodeCrewSources = []string{"tmdb"}
	for i := range 10000 {
		p.crewMemory.learn("person "+strconv.Itoa(i), "https://image.tmdb.org/t/p/original/person"+strconv.Itoa(i)+".jpg")
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		s := &Show{Root: "tvdb-1", Episodes: map[int][]Ep{1: {{Season: 1, Index: 1, Directors: []string{"Hajime Kamegaki"}}}}}
		var cp crewPhotos
		cp.add(srcTMDB, []sources.Person{{Name: "Hajime Kamegaki", Photo: "https://image.tmdb.org/t/p/original/" + strconv.Itoa(i) + ".jpg"}})
		p.finishCrew(s, &showSources{}, &cp)
	}
}

// BenchmarkLongFinishReport is a long build's match report, filled in
// before the build's waiters wake.
func BenchmarkLongFinishReport(b *testing.B) {
	p := longProvider(b)
	s, err := p.buildShow(context.Background(), "anidb-100")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		p.finishReport(report.NewRecorder("anidb-100", "anidb"), "anidb-100", s, nil, 0)
	}
}
