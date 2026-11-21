package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kizuna/internal/anidb"
	"kizuna/internal/mapping"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// Episode crew photos and producers (EPISODE_CREW_SOURCES,
// CREW_PHOTOS_FROM_PLEX) and the photo memory.

const (
	tmdbKamegaki = "https://image.tmdb.org/t/p/original/kamegaki.jpg"
	tmdbOogure   = "https://image.tmdb.org/t/p/original/oogure.jpg"
	tvdbExec     = "https://artworks.thetvdb.com/banners/person/9/p.jpg"
	plexKamegaki = "https://plex/kamegaki.jpg"
	plexProducer = "https://plex/producer.jpg"
)

// photoSeries: testSeries with crew photos. TheTVDB names Kamegaki Hajime
// and Oogure Itou without photos, and an executive producer with one;
// TheMovieDb has Kamegaki's photo on episode 1 and Oogure's on episode 2,
// each spelt its own way; Plex has its own photo of Kamegaki and a
// producer with a photo. AniDB 200 is the series' later entry (season 2).
func photoSeries(path string, q map[string][]string) (int, string) {
	switch strings.TrimPrefix(path, plexHost) {
	case "/library/metadata/p500s1/children":
		ep := func(n int, title, crew string) string {
			return strings.Replace(plexEpisode(1, n, title), `"Director":[{"tag":"Plex Director"}],"Writer":[{"tag":"Plex Writer"}]`, crew, 1)
		}
		return 200, plexList(
			ep(1, "Plex One", `"Director":[{"tag":"Kamegaki Hajime","thumb":"`+plexKamegaki+`"}],"Writer":[{"tag":"Plex Writer"}],`+
				`"Producer":[{"tag":"Plex Producer","thumb":"`+plexProducer+`"}]`),
			ep(2, "Plex Two", `"Director":[{"tag":"Kamegaki Hajime"}]`))
	}
	switch path {
	case "api.anidb.net/httpapi":
		if len(q["aid"]) > 0 && q["aid"][0] == "200" {
			return 200, strings.Replace(secondXML, "<episodes>", `<titles><title type="main" xml:lang="x-jat">Kizuna Test 2</title></titles><episodes>`, 1)
		}
	case "api4.thetvdb.com/v4/episodes/11/extended":
		return 200, `{"data":{"characters":[{"personName":"Kamegaki Hajime","peopleType":"Director"},
			{"personName":"Oogure Itou","peopleType":"Writer"},
			{"personName":"Exec Person","peopleType":"Executive Producer","personImgURL":"/banners/person/9/p.jpg"},
			{"personName":"A Voice","peopleType":"Actor","personImgURL":"/banners/person/1/a.jpg"}]}}`
	case "api4.thetvdb.com/v4/episodes/12/extended":
		return 200, `{"data":{"characters":[{"personName":"Kamegaki Hajime","peopleType":"Director"},
			{"personName":"Second Writer","peopleType":"Writer"}]}}`
	case "api4.thetvdb.com/v4/episodes/13/extended":
		return 200, `{"data":{"characters":[{"personName":"Kamegaki Hajime","peopleType":"Director"}]}}`
	case "api.themoviedb.org/3/tv/77/season/1":
		return 200, `{"episodes":[{"episode_number":1,"name":"TMDB One","crew":[
				{"job":"Director","department":"Directing","name":"Hajime Kamegaki","profile_path":"/kamegaki.jpg"},
				{"job":"Screenplay","department":"Writing","name":"Ito Oogure"}]},
			{"episode_number":2,"name":"TMDB Two","crew":[{"job":"Screenplay","department":"Writing","name":"Itō Ōgure","profile_path":"/oogure.jpg"}]}]}`
	}
	return testSeries(path, q)
}

// photoProvider: a provider on photoSeries (through next, when given),
// with Plex set up and AniDB 200 known.
func photoProvider(t *testing.T, next func(string, map[string][]string) (int, string)) (*Provider, *fakeNet) {
	t.Helper()
	defaultLists(t)
	if next == nil {
		next = photoSeries
	}
	plex := plexService(next)
	net := &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		if path == plexHost+"/library/metadata/p500s1/children" { // photoSeries' own, not plexService's
			return next(path, q)
		}
		return plex(path, q)
	}}
	p := newTestProvider(t, net)
	withPlex(t, p, net)
	p.titles.SetForTest(map[int][]anidb.Title{100: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test"}},
		200: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test 2"}}})
	p.lists.SetForTest(map[int]mapping.Entry{
		100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}, TMDB: mapping.Placement{ID: 77}},
		200: {AID: 200, TVDB: mapping.Placement{ID: 500, Season: 2, Mapped: true}, TMDB: mapping.Placement{ID: 77}},
	})
	if _, err := p.anidb.Anime(context.Background(), 200); err != nil { // cached: a /tvdb build doesn't wait for it
		t.Fatal(err)
	}
	return p, net
}

// sentCrew is one role of an episode as Plex gets it: "name=photo" each.
func sentCrew(tags []Tag) string {
	var out []string
	for _, t := range tags {
		out = append(out, t.Tag+"="+t.Thumb)
	}
	return strings.Join(out, ",")
}

// The rule for a person's photo, per source: the first source's in
// EPISODE_CREW_SOURCES that has one, paired by nameKey; with
// CREW_PHOTOS_FROM_PLEX, Plex's first, then the list.
func TestCrewPhotoRule(t *testing.T) {
	type photos struct {
		src    srcCode
		people []sources.Person
	}
	for _, c := range []struct {
		name      string
		have      []photos
		order     []string
		plexFirst bool
		ask       string
		want      string
		wantSrc   srcCode
	}{
		{"the first source's", []photos{{srcTVDB, []sources.Person{{Name: "Hajime Kamegaki", Photo: "tvdb"}}},
			{srcTMDB, []sources.Person{{Name: "Hajime Kamegaki", Photo: "tmdb"}}}},
			[]string{"tvdb", "tmdb", "plex"}, false, "Hajime Kamegaki", "tvdb", srcTVDB},
		{"the order decides", []photos{{srcTVDB, []sources.Person{{Name: "Hajime Kamegaki", Photo: "tvdb"}}},
			{srcTMDB, []sources.Person{{Name: "Hajime Kamegaki", Photo: "tmdb"}}}},
			[]string{"tmdb", "tvdb"}, false, "Hajime Kamegaki", "tmdb", srcTMDB},
		{"a later source's, across spellings", []photos{{srcTMDB, []sources.Person{{Name: "Kouichi Yamadera", Photo: "tmdb"}}}},
			[]string{"tvdb", "tmdb", "plex"}, false, "Yamadera Kōichi", "tmdb", srcTMDB},
		{"none", []photos{{srcTMDB, []sources.Person{{Name: "Someone Else", Photo: "tmdb"}}}},
			[]string{"tvdb", "tmdb", "plex"}, false, "Hajime Kamegaki", "", srcNone},
		{"two photos under one key: that source has none", []photos{
			{srcTMDB, []sources.Person{{Name: "Hiro Sato", Photo: "a"}, {Name: "Sato Hiro", Photo: "b"}, {Name: "Hiro Sato", Photo: "a"}}},
			{srcPlex, []sources.Person{{Name: "Hiro Sato", Photo: "plex"}}}},
			[]string{"tmdb", "plex"}, false, "Hiro Sato", "plex", srcPlex},
		{"the same photo twice is no conflict", []photos{{srcTMDB, []sources.Person{{Name: "Hiro Sato", Photo: "a"}, {Name: "Sato Hiro", Photo: "a"}}}},
			[]string{"tmdb"}, false, "Hiro Sato", "a", srcTMDB},
		{"with the setting, Plex's first", []photos{{srcTVDB, []sources.Person{{Name: "Hajime Kamegaki", Photo: "tvdb"}}},
			{srcPlex, []sources.Person{{Name: "Kamegaki Hajime", Photo: "plex"}}}},
			[]string{"tvdb", "tmdb", "plex"}, true, "Hajime Kamegaki", "plex", srcPlex},
		{"with the setting, then the list", []photos{{srcTMDB, []sources.Person{{Name: "Hajime Kamegaki", Photo: "tmdb"}}}},
			[]string{"tvdb", "tmdb"}, true, "Hajime Kamegaki", "tmdb", srcTMDB},
		{"with the setting, plex in no list", []photos{{srcTVDB, []sources.Person{{Name: "Hajime Kamegaki", Photo: "tvdb"}}},
			{srcPlex, []sources.Person{{Name: "Hajime Kamegaki", Photo: "plex"}}}},
			[]string{"tvdb", "tmdb"}, true, "Hajime Kamegaki", "plex", srcPlex},
		{"without the setting, Plex in its place", []photos{{srcTVDB, []sources.Person{{Name: "Hajime Kamegaki", Photo: "tvdb"}}},
			{srcPlex, []sources.Person{{Name: "Hajime Kamegaki", Photo: "plex"}}}},
			[]string{"tvdb", "plex"}, false, "Hajime Kamegaki", "tvdb", srcTVDB},
		{"a source not in the order", []photos{{srcTMDB, []sources.Person{{Name: "Hajime Kamegaki", Photo: "tmdb"}}}},
			[]string{"tvdb", "plex"}, false, "Hajime Kamegaki", "", srcNone},
		{"an empty key gets none", []photos{{srcTMDB, []sources.Person{{Name: "!!", Photo: "tmdb"}}}},
			[]string{"tmdb"}, false, "!!", "", srcNone},
	} {
		var cp crewPhotos
		for _, h := range c.have {
			cp.add(h.src, h.people)
		}
		if got, src := cp.photo(cp.key(c.ask), c.order, c.plexFirst); got != c.want || src != c.wantSrc {
			t.Errorf("%s: %q from %q, want %q from %q", c.name, got, src.name(), c.want, c.wantSrc.name())
		}
	}
}

// listAt is an episode list paired with a show by each episode's own
// season and number.
func listAt(eps ...sources.Episode) pairedList {
	return pairedList{indexEpisodes(eps), func(ep *Ep) (spot, bool) { return spot{ep.Season, ep.Index}, true }}
}

// Producers are picked like directors and writers, on their own: from a
// later source than the directors, and none from a source not listed.
func TestPickCrewProducers(t *testing.T) {
	ep := &Ep{Season: 1, Index: 1, tvdbEp: 11}
	records := map[int]sources.Crew{11: {Directors: []string{"TVDB Director"}, Writers: []string{"TVDB Writer"}}}
	tm := listAt(sources.Episode{Season: 1, Number: 1, Crew: sources.Crew{Directors: []string{"TMDB Director"}}})
	px := listAt(sources.Episode{Season: 1, Number: 1, Crew: sources.Crew{Directors: []string{"Plex Director"}, Producers: []string{"Plex Producer"}}})
	c := pickCrew(ep, []string{"tvdb", "tmdb", "plex"}, records, tm, px)
	if strings.Join(c.directors, ",") != "TVDB Director" || c.dSrc != srcTVDB || strings.Join(c.producers, ",") != "Plex Producer" || c.pSrc != srcPlex {
		t.Errorf("tvdb,tmdb,plex: %+v, want TheTVDB's directors and Plex's producers", c)
	}
	if c := pickCrew(ep, []string{"tvdb", "tmdb"}, records, tm, px); c.producers != nil || c.pSrc != srcNone {
		t.Errorf("plex not listed: producers %v from %s, want none", c.producers, c.pSrc.name())
	}
}

// TheTVDB's records are asked for an episode the sources before tvdb left
// without directors, writers or producers, or with a person none of them has
// a photo of; with CREW_PHOTOS_FROM_PLEX, Plex's photos count too.
func TestLacksCrew(t *testing.T) {
	ep := &Ep{Season: 1, Index: 1, tvdbEp: 11}
	full := sources.Crew{Directors: []string{"D"}, Writers: []string{"W"}, Producers: []string{"P"}}
	withPhotos := full
	withPhotos.Photos = []sources.Person{{Name: "D", Photo: "d"}, {Name: "W", Photo: "w"}, {Name: "P", Photo: "p"}}
	noProducer := withPhotos
	noProducer.Producers = nil
	for _, c := range []struct {
		name       string
		tmdb       sources.Crew
		plex       []sources.Person // Plex's photos, read for the setting
		plexPhotos bool
		want       bool
	}{
		{"all three with photos", withPhotos, nil, false, false},
		{"no producer", noProducer, nil, false, true},
		{"a person without a photo", full, nil, false, true},
		{"Plex has the photos, the setting on", full, withPhotos.Photos, true, false},
		{"Plex has the photos, the setting off", full, withPhotos.Photos, false, true},
	} {
		tm := listAt(sources.Episode{Season: 1, Number: 1, Crew: c.tmdb})
		var cp crewPhotos
		cp.addList(srcTMDB, tm)
		cp.add(srcPlex, c.plex)
		if got := lacksCrew(ep, []string{"tmdb"}, &cp, c.plexPhotos, tm, pairedList{}); got != c.want {
			t.Errorf("%s: lacks crew %v, want %v", c.name, got, c.want)
		}
	}
}

// A show answer's crew photos count in their source's place, but only when
// the build read that answer anyway: finishCrew never fetches one.
func TestFinishCrewShowAnswers(t *testing.T) {
	p := &Provider{crewMemory: loadCrewMemory("")}
	p.cfg.EpisodeCrewSources = []string{"tvdb", "tmdb", "plex"}
	fetched := 0
	ss := &showSources{}
	ss.tmdbDetails.f = func() (sources.Details, error) {
		fetched++
		return sources.Details{CrewPhotos: []sources.Person{{Name: "Hajime Kamegaki", Photo: "tmdb-show"}}}, nil
	}
	ss.plexShow.f = func() (sources.PlexShow, error) {
		fetched++
		return sources.PlexShow{Details: sources.Details{CrewPhotos: []sources.Person{{Name: "Writer Person", Photo: "plex-show"}}}}, nil
	}
	s := &Show{Root: "tvdb-1", Episodes: map[int][]Ep{1: {{Season: 1, Index: 1, Directors: []string{"Kamegaki Hajime"}, Writers: []string{"Writer Person"}}}}}
	var cp crewPhotos
	p.finishCrew(s, ss, &cp)
	if fetched != 0 || s.crew["Kamegaki Hajime"].src != srcNone || p.crewMemory.photo(nameKey("Writer Person")) != "" {
		t.Fatalf("answers not read: %d fetched, crew %+v; want none fetched, no photo", fetched, s.crew)
	}
	ss.tmdbDetails.get() // read for a field
	ss.plexShow.get()
	fetched = 0
	p.finishCrew(s, ss, &cp)
	it := p.episodeItem(Modes["tvdb"], s, &s.Episodes[1][0])
	if fetched != 0 || s.crew["Kamegaki Hajime"].src != srcTMDB || s.crew["Writer Person"].src != srcPlex ||
		sentCrew(it.Director) != "Kamegaki Hajime=tmdb-show" || sentCrew(it.Writer) != "Writer Person=plex-show" {
		t.Errorf("answers read: crew %+v, sent %q %q", s.crew, sentCrew(it.Director), sentCrew(it.Writer))
	}
}

// The photos in builds, both modes: TheTVDB's director with TheMovieDb's
// photo (from its episode 1, another spelling), TheTVDB's writer with
// TheMovieDb's photo from another episode, TheTVDB's producer with its own;
// with CREW_PHOTOS_FROM_PLEX, Plex's photo of the director; with plex first,
// Plex's names and photos.
func TestCrewPhotosInBuilds(t *testing.T) {
	for _, c := range []struct {
		name                  string
		set                   func(p *Provider)
		directors, writers    string
		producers, pSrc, dSrc string
	}{
		{"default lists", nil, "Kamegaki Hajime=" + tmdbKamegaki, "Oogure Itou=" + tmdbOogure, "Exec Person=" + tvdbExec, "tvdb", "tvdb"},
		{"CREW_PHOTOS_FROM_PLEX", func(p *Provider) { p.cfg.CrewPhotosFromPlex = true },
			"Kamegaki Hajime=" + plexKamegaki, "Oogure Itou=" + tmdbOogure, "Exec Person=" + tvdbExec, "tvdb", "tvdb"},
		{"plex first", func(p *Provider) { p.cfg.EpisodeCrewSources = []string{"plex", "tvdb", "tmdb"} },
			"Kamegaki Hajime=" + plexKamegaki, "Plex Writer=", "Plex Producer=" + plexProducer, "plex", "plex"},
	} {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			name := c.name + ", " + root
			p, _ := photoProvider(t, nil)
			if c.set != nil {
				c.set(p)
			}
			s, err := p.buildShow(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			ep := s.Episode(1, 1)
			if ep == nil || !s.Complete {
				t.Fatalf("%s: S01E01 %+v, complete %v", name, ep, s.Complete)
			}
			it := p.episodeItem(Modes["anidb"], s, ep)
			if got := sentCrew(it.Director); got != c.directors || ep.src.directors.name() != c.dSrc {
				t.Errorf("%s: directors %q from %s, want %q from %s", name, got, ep.src.directors.name(), c.directors, c.dSrc)
			}
			if got := sentCrew(it.Writer); got != c.writers {
				t.Errorf("%s: writers %q, want %q", name, got, c.writers)
			}
			if got := sentCrew(it.Producer); got != c.producers || ep.src.producers.name() != c.pSrc {
				t.Errorf("%s: producers %q from %s, want %q from %s", name, got, ep.src.producers.name(), c.producers, c.pSrc)
			}
			// S01E02: TheTVDB's writer (Plex has none there), whom no source
			// has a photo of.
			if it := p.episodeItem(Modes["anidb"], s, s.Episode(1, 2)); sentCrew(it.Writer) != "Second Writer=" {
				t.Errorf("%s: S01E02 writers %q", name, sentCrew(it.Writer))
			}
		}
	}
}

// A later /anidb entry's crew gets the photos of the series' other
// episodes: TheMovieDb's list is the whole series'.
func TestCrewPhotosLaterEntry(t *testing.T) {
	p, _ := photoProvider(t, nil)
	s, err := p.buildShow(context.Background(), "anidb-200")
	if err != nil {
		t.Fatal(err)
	}
	ep := s.Episode(1, 1)
	if ep == nil {
		t.Fatal("S01E01 missing")
	}
	if got := sentCrew(p.episodeItem(Modes["anidb"], s, ep).Director); got != "Kamegaki Hajime="+tmdbKamegaki {
		t.Errorf("AniDB 200 S01E01 directors %q, want TheTVDB's with TheMovieDb's photo", got)
	}
}

// Plex's list failing makes the show incomplete; Plex not having the series
// is no failure: the photos come from the other sources. Both modes.
func TestCrewPhotosPlexFailsOrLacks(t *testing.T) {
	for _, c := range []struct {
		name     string
		path     string // what Plex answers differently
		code     int
		body     string
		complete bool
	}{
		{"Plex fails", plexHost + "/library/metadata/p500s1/children", 503, `{}`, false},
		{"Plex lacks the series", plexHost + "/library/metadata/matches/2/tvdb://500", 200, plexList(), true},
	} {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			name := c.name + ", " + root
			p, net := photoProvider(t, nil)
			plex := net.answer
			net.answer = func(path string, q map[string][]string) (int, string) {
				if path == c.path {
					return c.code, c.body
				}
				return plex(path, q)
			}
			p.cfg.CrewPhotosFromPlex = true
			s, err := p.buildShow(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if s.Complete != c.complete {
				t.Errorf("%s: complete %v, want %v", name, s.Complete, c.complete)
			}
			if got := sentCrew(p.episodeItem(Modes["anidb"], s, s.Episode(1, 1)).Director); got != "Kamegaki Hajime="+tmdbKamegaki {
				t.Errorf("%s: directors %q, want TheMovieDb's photo", name, got)
			}
		}
	}
}

// What is asked of Plex for crew, both modes: plex listed, its lists are
// read once; with the setting and plex in no list, they are read and give
// only photos (no title, fill value or name); with no crew list, nothing is
// read even with the setting on, and no crew is sent; a show answer is
// never fetched for photos alone.
func TestCrewPhotosPlexRequests(t *testing.T) {
	for _, c := range []struct {
		name  string
		set   func(p *Provider)
		reads int // Plex's season 1 list, read this many times
	}{
		{"plex listed", nil, 1},
		{"the setting, plex in no list", func(p *Provider) {
			noPlexLists(t, p)
			p.cfg.CrewPhotosFromPlex = true
		}, 1},
		{"no crew list, the setting on", func(p *Provider) {
			noPlexLists(t, p)
			p.cfg.EpisodeCrewSources, p.cfg.CrewPhotosFromPlex = nil, true
		}, 0},
	} {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			name := c.name + ", " + root
			p, net := photoProvider(t, nil)
			if c.set != nil {
				c.set(p)
			}
			s, err := p.buildShow(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if n := plexAsked(net, "p500s1/children"); n != c.reads || !s.Complete {
				t.Errorf("%s: Plex's list read %d times, complete %v; want %d, complete", name, n, s.Complete, c.reads)
			}
			if c.set == nil {
				continue
			}
			if n := net.count(plexHost + "/library/metadata/p500"); n != 0 {
				t.Errorf("%s: Plex's show asked %d times, want never: no field lists plex", name, n)
			}
			s.eachEpisode(func(ep *Ep) {
				if ep.titles["plex"] != "" || slices.Contains([]srcCode{ep.src.summary, ep.src.thumb, ep.src.date, ep.src.minutes,
					ep.src.directors, ep.src.writers, ep.src.producers}, srcPlex) {
					t.Errorf("%s S%02dE%02d: title %q, sources %+v; want only photos from Plex", name, ep.Season, ep.Index, ep.titles["plex"], ep.src)
				}
			})
			it := p.episodeItem(Modes["anidb"], s, s.Episode(1, 1))
			want := "Kamegaki Hajime=" + plexKamegaki
			if p.cfg.EpisodeCrewSources == nil {
				want = ""
				if s.crew != nil {
					t.Errorf("%s: crew %+v, want none", name, s.crew)
				}
			}
			if got := sentCrew(it.Director); got != want {
				t.Errorf("%s: directors %q, want %q", name, got, want)
			}
		}
	}
}

// With no crew list, CREW_PHOTOS_FROM_PLEX does nothing even when Plex's
// list is read for its titles: the walk takes no photo from it (not a key
// worked out), and the build sends no crew and remembers no photo. Both
// modes.
func TestCrewPhotosNoCrewListPlexRead(t *testing.T) {
	px := listAt(sources.Episode{Season: 1, Number: 1, Crew: sources.Crew{Directors: []string{"Kamegaki Hajime"},
		Photos: []sources.Person{{Name: "Kamegaki Hajime", Photo: plexKamegaki}}}})
	noRecords := func([]string) (map[int]sources.Crew, bool) { return nil, false }
	for _, c := range []struct {
		name  string
		order []string
		want  bool // Plex's photos taken
	}{
		{"no crew list", nil, false},
		{"a crew list without plex", []string{"tmdb"}, true},
	} {
		p := &Provider{}
		p.cfg.EpisodeCrewSources, p.cfg.CrewPhotosFromPlex = c.order, true
		var cp crewPhotos
		p.walkCrew(&cp, pairedList{}, px, noRecords)
		if got := cp.by[srcPlex] != nil || cp.keys != nil; got != c.want {
			t.Errorf("%s: Plex photos %v, keys %v; want taken %v", c.name, cp.by[srcPlex], cp.keys, c.want)
		}
	}

	for _, root := range []string{"tvdb-500", "anidb-100"} {
		p, net := photoProvider(t, nil)
		p.cfg.EpisodeCrewSources, p.cfg.CrewPhotosFromPlex = nil, true
		p.cfg.EpisodeTitles = []string{"plex", "tvdb"} // Plex first: its list is read for the titles
		s, err := p.buildShow(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if n := plexAsked(net, "p500s1/children"); n != 1 || !s.Complete {
			t.Errorf("%s: Plex's list read %d times, complete %v; want once, complete", root, n, s.Complete)
		}
		ep := s.Episode(1, 1)
		it := p.episodeItem(Modes["anidb"], s, ep)
		if it.Title != "Plex One" || it.Director != nil || it.Writer != nil || it.Producer != nil || s.crew != nil {
			t.Errorf("%s: S01E01 %q, crew %v %v %v (%+v); want Plex's title, no crew", root, it.Title, it.Director, it.Writer, it.Producer, s.crew)
		}
		if ph := p.crewMemory.photo(nameKey("Kamegaki Hajime")); ph != "" {
			t.Errorf("%s: photo %q remembered, want none", root, ph)
		}
	}
}

// A movie gets no episode crew, and with CREW_PHOTOS_FROM_PLEX asks Plex
// for no episode list.
func TestCrewPhotosMovie(t *testing.T) {
	p, net := photoProvider(t, func(path string, q map[string][]string) (int, string) {
		if path == "api.anidb.net/httpapi" {
			return 200, movieXML(300, "Film", "", 0, "2022-08-06")
		}
		return photoSeries(path, q)
	})
	p.titles.SetForTest(map[int][]anidb.Title{300: {{Type: "main", Lang: "x-jat", Text: "Film"}}})
	// A TheTVDB id, so Plex could be asked for the series' episodes.
	p.lists.SetForTest(map[int]mapping.Entry{300: {AID: 300, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}, TMDBMovie: 900}})
	p.cfg.CrewPhotosFromPlex = true
	s, err := p.buildShow(context.Background(), "anidb-300")
	if err != nil {
		t.Fatal(err)
	}
	if n := plexAsked(net, "/children"); n != 0 || s.crew != nil {
		t.Errorf("movie: %d Plex lists read, crew %+v; want none", n, s.crew)
	}
}

// The memory: a photo learned on one series is sent on another whose
// sources have none, also on a show built before it was learned, without a
// rebuild; a newer photo replaces it on both; it is kept across a restart.
func TestCrewMemory(t *testing.T) {
	dir := t.TempDir()
	p := &Provider{crewMemory: loadCrewMemory(dir)}
	manualSaves(p.crewMemory)
	p.cfg.EpisodeCrewSources = []string{"tvdb", "tmdb"}
	show := func(root, name string, photo string) *Show {
		s := &Show{Root: root, Episodes: map[int][]Ep{1: {{Season: 1, Index: 1, Directors: []string{name}}}}}
		var cp crewPhotos
		if photo != "" {
			cp.add(srcTMDB, []sources.Person{{Name: name, Photo: photo}})
		}
		p.finishCrew(s, &showSources{}, &cp)
		return s
	}
	sent := func(s *Show) string { return sentCrew(p.episodeItem(Modes["tvdb"], s, &s.Episodes[1][0]).Director) }

	early := show("tvdb-1", "Hajime Kamegaki", "") // built before anyone knew the photo
	if got := sent(early); got != "Hajime Kamegaki=" {
		t.Fatalf("no photo known: %q", got)
	}
	a := show("tvdb-2", "Kamegaki Hajime", "old.jpg")
	b := show("tvdb-3", "Kamegaki Hajime", "") // its sources have none
	if sent(a) != "Kamegaki Hajime=old.jpg" || sent(b) != "Kamegaki Hajime=old.jpg" || sent(early) != "Hajime Kamegaki=old.jpg" {
		t.Errorf("learned: %q %q %q, want old.jpg on all three", sent(a), sent(b), sent(early))
	}
	if b.crew["Kamegaki Hajime"].src != srcNone {
		t.Errorf("b's sources gave no photo: src %s", b.crew["Kamegaki Hajime"].src.name())
	}
	show("tvdb-4", "Kamegaki Hajime", "new.jpg")
	if sent(a) != "Kamegaki Hajime=new.jpg" || sent(b) != "Kamegaki Hajime=new.jpg" {
		t.Errorf("newer photo: %q %q, want new.jpg on both", sent(a), sent(b))
	}
	p.Flush() // the shutdown's save
	restarted := &Provider{crewMemory: loadCrewMemory(dir)}
	if got := restarted.crewMemory.photo(nameKey("Hajime Kamegaki")); got != "new.jpg" {
		t.Errorf("after a restart: %q, want new.jpg", got)
	}
}

// A broken memory file is a warning: the memory starts empty, and the next
// save writes a good file. A build that changes nothing leaves the file as
// it is.
func TestCrewMemoryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, crewMemoryFile)
	if err := os.WriteFile(path, []byte(`{"photos": {"broken`), 0o644); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	restore := captureLogs(&logs)
	m := loadCrewMemory(dir)
	restore()
	if !strings.Contains(logs.String(), "crew photos file not read") {
		t.Errorf("no warning for the broken file: %q", logs.String())
	}
	p := &Provider{crewMemory: m}
	timers := manualSaves(m)
	p.cfg.EpisodeCrewSources = []string{"tmdb"}
	build := func(photo string) {
		s := &Show{Root: "tvdb-1", Episodes: map[int][]Ep{1: {{Season: 1, Index: 1, Directors: []string{"Hajime Kamegaki"}}}}}
		var cp crewPhotos
		cp.add(srcTMDB, []sources.Person{{Name: "Hajime Kamegaki", Photo: photo}})
		p.finishCrew(s, &showSources{}, &cp)
		timers.fire()
	}
	build("a.jpg")
	var f crewMemoryJSON
	readJSON(t, path, &f)
	if f.Photos[nameKey("Hajime Kamegaki")] != "a.jpg" || len(f.Photos) != 1 {
		t.Errorf("repaired file: %+v", f)
	}
	// The same photo again: nothing to save. The file keeps the bytes it has.
	const mark = `{"photos":{"hajime kamegaki":"a.jpg"},"left":"alone"}`
	if err := os.WriteFile(path, []byte(mark), 0o644); err != nil {
		t.Fatal(err)
	}
	build("a.jpg")
	if data, err := os.ReadFile(path); err != nil || string(data) != mark {
		t.Errorf("a build that changed nothing rewrote the file: %q %v", data, err)
	}
	build("b.jpg")
	readJSON(t, path, &f)
	if f.Photos[nameKey("Hajime Kamegaki")] != "b.jpg" {
		t.Errorf("a new photo: file %+v", f)
	}
}

// crewDir is a data folder named after the test, so crewLogged can tell
// this test's log lines from others': the logger is global, and other
// tests' background work may write to it.
func crewDir(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), t.Name())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// crewLogged reports whether a captured line has msg and names dir (by its
// base name: the text handler escapes the full path on Windows).
func crewLogged(logs, dir, msg string) bool {
	for line := range strings.Lines(logs) {
		if strings.Contains(line, msg) && strings.Contains(line, filepath.Base(dir)) {
			return true
		}
	}
	return false
}

// loadLogged loads the memory from dir and returns the log it wrote.
func loadLogged(dir string) (*crewMemory, string) {
	var logs strings.Builder
	restore := captureLogs(&logs)
	defer restore()
	return loadCrewMemory(dir), logs.String()
}

// saveLogged learns photo for key, saves, and returns the log it wrote.
func saveLogged(m *crewMemory, key, photo string) string {
	var logs strings.Builder
	restore := captureLogs(&logs)
	defer restore()
	m.learn(key, photo)
	m.save()
	return logs.String()
}

// unreadableLoad loads the memory from dir with a folder in the file's
// place: a read error, standing in for a sharing violation or a NAS error.
func unreadableLoad(t *testing.T, dir string) *crewMemory {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, crewMemoryFile, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	m, logs := loadLogged(dir)
	if !crewLogged(logs, dir, "crew photos file not read") {
		t.Errorf("no warning for the unreadable file: %q", logs)
	}
	return m
}

// healFile replaces the folder unreadableLoad put in the file's place with
// a good file holding photos: the read that failed would work now.
func healFile(t *testing.T, dir string, photos map[string]string) {
	t.Helper()
	path := filepath.Join(dir, crewMemoryFile)
	if err := os.Rename(path, filepath.Join(dir, "folder")); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(crewMemoryJSON{Photos: photos})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A read that failed at the start but works at the first save (the
// transient case) loses nothing: the file's photos are remembered again,
// the saved file holds them and the new one, and nothing is moved aside.
func TestCrewMemoryRereadMerges(t *testing.T) {
	dir := crewDir(t)
	m := unreadableLoad(t, dir)
	healFile(t, dir, map[string]string{"old": "old.jpg"})
	logs := saveLogged(m, "new", "new.jpg")
	if !crewLogged(logs, dir, "crew photos file read after all") || crewLogged(logs, dir, "aside") {
		t.Errorf("log: %q, want only the read after all", logs)
	}
	var f crewMemoryJSON
	readJSON(t, filepath.Join(dir, crewMemoryFile), &f)
	if len(f.Photos) != 2 || f.Photos["old"] != "old.jpg" || f.Photos["new"] != "new.jpg" {
		t.Errorf("saved file: %+v, want old and new", f)
	}
	if m.photo("old") != "old.jpg" {
		t.Errorf("the file's photo isn't sent: %q", m.photo("old"))
	}
	if _, err := os.Stat(filepath.Join(dir, crewMemoryFile+crewMemoryAside)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file was moved aside: %v", err)
	}
	// Merged once: the next save neither reads nor logs again.
	if logs := saveLogged(m, "later", "later.jpg"); crewLogged(logs, dir, "crew photos") {
		t.Errorf("second save logged: %q", logs)
	}
}

// When the file read again has a photo for a key learned since the start,
// the learned one wins: it is the newer.
func TestCrewMemoryRereadLearnedWins(t *testing.T) {
	dir := crewDir(t)
	m := unreadableLoad(t, dir)
	healFile(t, dir, map[string]string{"both": "file.jpg", "only file": "f.jpg"})
	saveLogged(m, "both", "learned.jpg")
	var f crewMemoryJSON
	readJSON(t, filepath.Join(dir, crewMemoryFile), &f)
	if f.Photos["both"] != "learned.jpg" || f.Photos["only file"] != "f.jpg" {
		t.Errorf("saved file: %+v, want the learned photo and the file's other one", f)
	}
	if m.photo("both") != "learned.jpg" {
		t.Errorf("memory: %q, want the learned photo", m.photo("both"))
	}
}

// A memory file that still can't be read at the first save isn't
// replaced: it is moved aside, the warning says where and what to do, and
// a new file is written. Later saves leave the kept-aside file alone.
func TestCrewMemoryUnreadableKeptAside(t *testing.T) {
	dir := crewDir(t)
	path := filepath.Join(dir, crewMemoryFile)
	m := unreadableLoad(t, dir)
	logs := saveLogged(m, "a", "a.jpg")
	aside := path + crewMemoryAside
	if !crewLogged(logs, dir, "moved aside") || !crewLogged(logs, dir, crewMemoryFile+crewMemoryAside) ||
		!crewLogged(logs, dir, "rename it to "+crewMemoryFile) {
		t.Errorf("no warning naming the kept-aside file and what to do: %q", logs)
	}
	if fi, err := os.Stat(filepath.Join(aside, "inside")); err != nil || !fi.IsDir() {
		t.Errorf("old file not kept aside: %v", err)
	}
	var f crewMemoryJSON
	readJSON(t, path, &f)
	if f.Photos["a"] != "a.jpg" || len(f.Photos) != 1 {
		t.Errorf("new file: %+v", f)
	}
	// The next save writes over the new file only, without a word.
	if logs := saveLogged(m, "b", "b.jpg"); crewLogged(logs, dir, "crew photos") {
		t.Errorf("second save logged: %q", logs)
	}
	readJSON(t, path, &f)
	if f.Photos["b"] != "b.jpg" {
		t.Errorf("second save: %+v", f)
	}
	if _, err := os.Stat(filepath.Join(aside, "inside")); err != nil {
		t.Errorf("second save touched the kept-aside file: %v", err)
	}
}

// A memory file with bad JSON, still bad at the first save, is kept aside
// the same way, replacing an older kept-aside file: one copy, never a pile.
func TestCrewMemoryBadJSONKeptAside(t *testing.T) {
	dir := crewDir(t)
	path := filepath.Join(dir, crewMemoryFile)
	const bad = `{"photos": {"broken`
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+crewMemoryAside, []byte("older"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := loadLogged(dir)
	saveLogged(m, "a", "a.jpg")
	if data, err := os.ReadFile(path + crewMemoryAside); err != nil || string(data) != bad {
		t.Errorf("kept-aside file: %q %v, want the bad file", data, err)
	}
	var f crewMemoryJSON
	readJSON(t, path, &f)
	if f.Photos["a"] != "a.jpg" || len(f.Photos) != 1 {
		t.Errorf("new file: %+v", f)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("data folder holds %d entries, want the file and one kept aside", len(entries))
	}
}

// When the unreadable file can't be moved aside (a folder holds the name),
// the save is skipped: the old file stays as it was, and the memory keeps
// the photo. The next save tries again.
func TestCrewMemoryAsideFails(t *testing.T) {
	dir := crewDir(t)
	path := filepath.Join(dir, crewMemoryFile)
	const bad = `not json`
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(path+crewMemoryAside, "blocker")
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	m, _ := loadLogged(dir)
	logs := saveLogged(m, "a", "a.jpg")
	if !crewLogged(logs, dir, "not moved aside") {
		t.Errorf("no warning for the failed move: %q", logs)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != bad {
		t.Errorf("old file overwritten: %q %v", data, err)
	}
	if m.photo("a") != "a.jpg" {
		t.Errorf("photo forgotten")
	}
	if err := os.Rename(path+crewMemoryAside, filepath.Join(dir, "moved")); err != nil {
		t.Fatal(err)
	}
	saveLogged(m, "b", "b.jpg")
	if data, err := os.ReadFile(path + crewMemoryAside); err != nil || string(data) != bad {
		t.Errorf("retried move: kept-aside file %q %v", data, err)
	}
	var f crewMemoryJSON
	readJSON(t, path, &f)
	if f.Photos["a"] != "a.jpg" || f.Photos["b"] != "b.jpg" {
		t.Errorf("new file: %+v", f)
	}
}

// No file yet is the first start: no warning, nothing kept aside.
func TestCrewMemoryMissingFile(t *testing.T) {
	dir := crewDir(t)
	m, logs := loadLogged(dir)
	if crewLogged(logs, dir, "crew photos") {
		t.Errorf("first start logged: %q", logs)
	}
	if logs := saveLogged(m, "a", "a.jpg"); crewLogged(logs, dir, "crew photos") {
		t.Errorf("first save logged: %q", logs)
	}
	if _, err := os.Stat(filepath.Join(dir, crewMemoryFile+crewMemoryAside)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file was kept aside on the first start: %v", err)
	}
	var f crewMemoryJSON
	readJSON(t, filepath.Join(dir, crewMemoryFile), &f)
	if f.Photos["a"] != "a.jpg" {
		t.Errorf("file: %+v", f)
	}
}

// A build in a data folder keeps its photos in crew-photos.json there,
// outside the cache.
func TestCrewMemoryFileFromBuild(t *testing.T) {
	p, _ := photoProvider(t, nil)
	dir := t.TempDir()
	p.crewMemory = loadCrewMemory(dir)
	timers := manualSaves(p.crewMemory)
	if _, err := p.buildShow(context.Background(), "tvdb-500"); err != nil {
		t.Fatal(err)
	}
	// First photos: on disk when the build ends, with no save left for later.
	if n := timers.fire(); n != 0 {
		t.Errorf("%d delayed saves asked for, want 0", n)
	}
	var f crewMemoryJSON
	readJSON(t, filepath.Join(dir, crewMemoryFile), &f)
	if f.Photos[nameKey("Kamegaki Hajime")] != tmdbKamegaki || f.Photos[nameKey("Exec Person")] != tvdbExec {
		t.Errorf("file %+v", f)
	}
}

// Without photos or producers an episode is sent as before them, byte for
// byte; the show's item has no crew photos at all.
func TestCrewItemsAsBefore(t *testing.T) {
	p := &Provider{crewMemory: loadCrewMemory("")}
	s := &Show{Root: "tvdb-1", Title: "Show", Directors: []string{"Show Director"}, Writers: []string{"Show Writer"},
		Episodes: map[int][]Ep{1: {{Season: 1, Index: 1, Title: "One", Directors: []string{"D"}, Writers: []string{"W"}}}}}
	p.crewMemory.learn(nameKey("Show Director"), "photo.jpg") // a crew photo of the show's director: not on the show
	ep, err := json.Marshal(p.episodeItem(Modes["tvdb"], s, &s.Episodes[1][0]))
	if err != nil {
		t.Fatal(err)
	}
	const wantEp = `{"ratingKey":"tvdb-1-s1-e1","key":"/library/metadata/tvdb-1-s1-e1","guid":"tv.plex.agents.custom.kizuna.tvdb://episode/tvdb-1-s1-e1",` +
		`"type":"episode","title":"One","index":1,"parentIndex":1,"parentRatingKey":"tvdb-1-s1","parentKey":"/library/metadata/tvdb-1-s1",` +
		`"parentGuid":"tv.plex.agents.custom.kizuna.tvdb://season/tvdb-1-s1","parentType":"season","parentTitle":"Season 1",` +
		`"grandparentRatingKey":"tvdb-1","grandparentKey":"/library/metadata/tvdb-1","grandparentGuid":"tv.plex.agents.custom.kizuna.tvdb://show/tvdb-1",` +
		`"grandparentType":"show","grandparentTitle":"Show","Director":[{"tag":"D"}],"Writer":[{"tag":"W"}]}`
	if string(ep) != wantEp {
		t.Errorf("episode\n%s\nwant\n%s", ep, wantEp)
	}
	show, err := json.Marshal(p.showItem(Modes["tvdb"], s, false))
	if err != nil {
		t.Fatal(err)
	}
	const wantShow = `{"ratingKey":"tvdb-1","key":"/library/metadata/tvdb-1/children","guid":"tv.plex.agents.custom.kizuna.tvdb://show/tvdb-1",` +
		`"type":"show","title":"Show","Director":[{"tag":"Show Director"}],"Writer":[{"tag":"Show Writer"}]}`
	if string(show) != wantShow {
		t.Errorf("show\n%s\nwant\n%s", show, wantShow)
	}
}

// The match report: each episode's producers and their source, and the
// episodes' count of crew photos, by source, "remembered" for a photo no
// source of the build gave.
func TestCrewPhotosReport(t *testing.T) {
	p, _ := photoProvider(t, nil)
	// Second Writer's photo is known from another show only.
	p.crewMemory.learn(nameKey("Second Writer"), "https://elsewhere/second.jpg")
	store, dir, serve := reportsOn(t, p)
	serve("GET", "/tvdb/library/metadata/tvdb-500", "")
	store.Flush()
	var doc report.ShowDoc
	readJSON(t, filepath.Join(dir, "data", "anidb-100.json"), &doc)
	if len(doc.Builds) != 1 {
		t.Fatalf("%d builds", len(doc.Builds))
	}
	b := doc.Builds[0]
	// Sent: Kamegaki Hajime (tmdb), Oogure Itou (tmdb), Exec Person (tvdb),
	// Second Writer (remembered), and Plex's director and writer of season
	// 2 and the special (none).
	if want := "crew photos: 4 of 6 (tmdb 2, tvdb 1, remembered 1)"; b.CrewPhotos != want {
		t.Errorf("count %q, want %q", b.CrewPhotos, want)
	}
	i := slices.IndexFunc(b.Episodes, func(e report.Episode) bool { return e.Season == 1 && e.Index == 1 })
	if i < 0 {
		t.Fatal("S01E01 not in the report")
	}
	if e := b.Episodes[i]; strings.Join(e.Producers, ",") != "Exec Person" || e.Src["producers"] != "tvdb" {
		t.Errorf("S01E01 producers %v from %q", e.Producers, e.Src["producers"])
	}
}

// The report's crew photo count comes from the episode rows' items. It says
// what a count over items built for it alone says (crewCountApart): on a
// show with photos from three sources and one remembered, in both modes.
func TestCrewPhotoCountFromRows(t *testing.T) {
	p, _ := photoProvider(t, nil)
	p.crewMemory.learn(nameKey("Second Writer"), "https://elsewhere/second.jpg")
	check := func(p *Provider, root string, mode Mode) {
		t.Helper()
		s, err := p.buildShow(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		_, _, crew := p.episodeRows(mode, s)
		got, want := crewPhotoCount(s, crew), crewCountApart(p, mode, s)
		if got != want || want == "" {
			t.Errorf("%s in %s: %q, want %q", root, mode.Path, got, want)
		}
	}
	check(p, "tvdb-500", Modes["tvdb"])
	check(p, "anidb-100", Modes["anidb"])
}

// crewCountApart is the crew photo count from the episodes' items built for
// the count alone.
func crewCountApart(p *Provider, mode Mode, s *Show) string {
	withPhoto := map[string]bool{}
	for _, n := range s.Seasons() {
		for i := range s.Episodes[n] {
			it := p.episodeItem(mode, s, &s.Episodes[n][i])
			for _, tags := range [...][]Tag{it.Director, it.Writer, it.Producer} {
				for _, t := range tags {
					withPhoto[t.Tag] = withPhoto[t.Tag] || t.Thumb != ""
				}
			}
		}
	}
	return crewPhotoCount(s, withPhoto)
}
