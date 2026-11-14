package provider

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"kizuna/internal/anidb"
	"kizuna/internal/mapping"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// CAST_PHOTOS_FROM_PLEX: the cast keeps its source, and each person gets the
// photo Plex's own cast has for them.

// The swap: Plex's photo for each person it has with one, paired by name;
// everything else about the cast stays.
func TestCastPhotos(t *testing.T) {
	type person struct{ Name, Role, Photo string } // sources.Person, short to write
	people := func(l []person) []sources.Person {
		var out []sources.Person
		for _, p := range l {
			out = append(out, sources.Person(p))
		}
		return out
	}
	// Plex's people past the 50 its own cast keeps. Letters only: the key
	// drops digits, so each name gets its own count of x's.
	var many []person
	for i := 1; i <= 60; i++ {
		many = append(many, person{Name: "Actor " + strings.Repeat("x", i), Photo: fmt.Sprintf("https://plex/%d.jpg", i)})
	}
	for _, c := range []struct {
		name       string
		cast, plex []person
		want       []person
		n          int
	}{
		{"paired: Plex's photo, the rest unchanged",
			[]person{{"Yamadera Kouichi", "Spike", "own-1"}, {"Hayashibara Megumi", "Faye", "own-2"}},
			[]person{{"Megumi Hayashibara", "Faye Valentine", "plex-2"}, {"Koichi Yamadera", "Spike Spiegel", "plex-1"}},
			[]person{{"Yamadera Kouichi", "Spike", "plex-1"}, {"Hayashibara Megumi", "Faye", "plex-2"}}, 2},
		{"not on Plex, or on Plex without a photo: own photo",
			[]person{{"Ishizuka Unshou", "Jet", "own-1"}, {"Paku Romi", "Ed", "own-2"}, {"Miki Shin`ichirou", "Vicious", ""}},
			[]person{{"Unsho Ishizuka", "Jet Black", ""}, {"Romi Park", "Ed", "plex-2"}},
			[]person{{"Ishizuka Unshou", "Jet", "own-1"}, {"Paku Romi", "Ed", "own-2"}, {"Miki Shin`ichirou", "Vicious", ""}}, 0},
		{"a cast without photos (ANN's)",
			[]person{{"Kōichi Yamadera", "Spike", ""}, {"Ryō Naitō", "", ""}},
			[]person{{"Koichi Yamadera", "", "plex-1"}},
			[]person{{"Kōichi Yamadera", "Spike", "plex-1"}, {"Ryō Naitō", "", ""}}, 1},
		{"two Plex people with one key and different photos: no swap",
			[]person{{"Ono Yuko", "A", "own-1"}},
			[]person{{"Yuko Ono", "A", "plex-1"}, {"Yuuko Oono", "B", "plex-2"}},
			[]person{{"Ono Yuko", "A", "own-1"}}, 0},
		{"one Plex person twice with one photo: swapped",
			[]person{{"Yamadera Kouichi", "Spike", "own-1"}},
			[]person{{"Koichi Yamadera", "Spike", "plex-1"}, {"Koichi Yamadera", "Narrator", "plex-1"}, {"Koichi Yamadera", "Extra", ""}},
			[]person{{"Yamadera Kouichi", "Spike", "plex-1"}}, 1},
		{"one person twice in the cast: both rows",
			[]person{{"Yamadera Kouichi", "Spike", "own-1"}, {"Hayashibara Megumi", "Faye", ""}, {"Yamadera Kouichi", "Narrator", ""}},
			[]person{{"Koichi Yamadera", "Spike", "plex-1"}},
			[]person{{"Yamadera Kouichi", "Spike", "plex-1"}, {"Hayashibara Megumi", "Faye", ""}, {"Yamadera Kouichi", "Narrator", "plex-1"}}, 2},
		{"a person past Plex's first 50",
			[]person{{many[59].Name, "Late", "own"}},
			many,
			[]person{{many[59].Name, "Late", "https://plex/60.jpg"}}, 1},
		{"no Plex people", []person{{"Yamadera Kouichi", "Spike", "own-1"}}, nil, []person{{"Yamadera Kouichi", "Spike", "own-1"}}, 0},
		{"names without a key never pair", []person{{"---", "", "own"}, {"", "", ""}}, []person{{"...", "", "plex-1"}, {"", "", "plex-2"}},
			[]person{{"---", "", "own"}, {"", "", ""}}, 0},
	} {
		cast := people(c.cast)
		if n := castPhotos(cast, people(c.plex)); n != c.n || !slices.Equal(cast, people(c.want)) {
			t.Errorf("%s: %d swapped, cast %+v; want %d, %+v", c.name, n, cast, c.n, c.want)
		}
	}
}

// The name key pairs the spellings the sources were measured to use for one
// person, and not different people whose names are nearly the same.
func TestNameKey(t *testing.T) {
	for _, pair := range [][]string{
		{"Kouhei", "Kohei", "Kōhei"}, // the folds one after another
		{"Yamadera Kouichi", "Koichi Yamadera"},
		{"Miki Shin`ichirou", "Shin-ichiro Miki"},
		{"Rina Satô", "Rina Sato"},
		{"Shōko Tsuda", "Showko Tsuda"},
		{"Kōsuke Toriumi", "Kohsuke Toriumi"},
		{"Kobayashi  Yuu", "Kobayashi Yu", " yu KOBAYASHI ", "Kobayashi Yuu", "Yuu	Kobayashi"}, // any space splits words
	} {
		for _, name := range pair[1:] {
			if a, b := nameKey(pair[0]), nameKey(name); a != b || a == "" {
				t.Errorf("%q and %q: keys %q and %q, want one", pair[0], name, a, b)
			}
		}
	}
	for _, pair := range [][2]string{
		{"Hirofumi Nojima", "Hiroshi Nojima"},
		{"Daisuke Hirakawa", "Daisuke Namikawa"},
		{"Kobayashi Yuu", "Yusuke Kobayashi"},
		{"Paku Romi", "Romi Park"},
	} {
		if a, b := nameKey(pair[0]), nameKey(pair[1]); a == b {
			t.Errorf("%q and %q: one key %q", pair[0], pair[1], a)
		}
	}
	if k := nameKey("Kōhei"); k != "koei" {
		t.Errorf("Kōhei: key %q, want koei", k)
	}
	for _, name := range []string{"", "  ", "-", "1984", "`'."} {
		if k := nameKey(name); k != "" {
			t.Errorf("%q: key %q, want none", name, k)
		}
	}
	if castPhotos([]sources.Person{{Name: "-"}}, []sources.Person{{Name: ".", Photo: "plex"}}) != 0 {
		t.Error("an empty key paired with an empty key")
	}
}

// castXML is AniDB 100 (anidbXML) with a cast: AniDB's spellings, photos
// on two of them.
var castXML = strings.Replace(anidbXML, "<episodes>", `<characters>
<character type="main character in"><name>Spike</name><seiyuu picture="yamadera.jpg">Yamadera Kouichi</seiyuu></character>
<character type="secondary cast in"><name>Jet</name><seiyuu picture="ishizuka.jpg">Ishizuka Unshou</seiyuu></character>
<character type="secondary cast in"><name>Faye</name><seiyuu>Hayashibara Megumi</seiyuu></character>
</characters><episodes>`, 1)

// plexCastItem is Plex's p500 with a cast: Yamadera with a photo, Ishizuka
// without, Hayashibara not at all.
const plexCastItem = `{"MediaContainer":{"Metadata":[{"title":"Plex Name","Role":[
	{"tag":"Koichi Yamadera","role":"Spike Spiegel","thumb":"https://plex/yamadera.jpg","order":1},
	{"tag":"Unsho Ishizuka","role":"Jet Black","order":2}]}]}}`

const plexItemPath = plexHost + "/library/metadata/p500"

// castNet is testSeries with AniDB's answer anidbAnswer and Plex's service
// (plexService) with p500's cast (plexCastItem). plex replaces its answers
// by path ("<code> <body>").
func castNet(anidbAnswer string, plex map[string]string) *fakeNet {
	return &fakeNet{answer: func(path string, q map[string][]string) (int, string) {
		if v, ok := plex[path]; ok {
			code, body, _ := strings.Cut(v, " ")
			n, _ := strconv.Atoi(code)
			return n, body
		}
		switch path {
		case plexItemPath:
			return 200, plexCastItem
		case "api.anidb.net/httpapi":
			return 200, anidbAnswer
		}
		return plexService(testSeries)(path, q)
	}}
}

// noPlexLists takes plex out of every source list: Plex is then asked
// nothing for a field. It checks with Listed, which knows every list: a
// list added to Config later and missed here fails the test instead of
// leaving plex in it.
func noPlexLists(t *testing.T, p *Provider) {
	t.Helper()
	c := &p.cfg
	for _, l := range []*[]string{&c.TitleSources, &c.SummarySources, &c.PosterSources, &c.BackgroundSources, &c.LogoSources,
		&c.EpisodeFillSources, &c.EpisodeCrewSources, &c.EpisodeTitles, &c.GenreSources, &c.CastSources, &c.StudioSources,
		&c.CrewSources, &c.RatingSources, &c.DateSources} {
		*l = slices.DeleteFunc(slices.Clone(*l), func(s string) bool { return s == "plex" })
	}
	if c.Listed("plex") {
		t.Fatal("a source list still names plex: add it to noPlexLists")
	}
}

func photosOf(roles []sources.Person) string {
	var out []string
	for _, r := range roles {
		out = append(out, r.Photo)
	}
	return strings.Join(out, " ")
}

const (
	anidbPhotos = "https://cdn.anidb.net/images/main/yamadera.jpg https://cdn.anidb.net/images/main/ishizuka.jpg "
	plexPhotos  = "https://plex/yamadera.jpg https://cdn.anidb.net/images/main/ishizuka.jpg "
)

// Off, the cast and its photos are AniDB's and Plex, in no list, gets no
// request. On, Plex's show is fetched once, though no list names plex and
// no field has a gap, and Yamadera gets Plex's photo. In both modes.
func TestCastPhotosBuild(t *testing.T) {
	defaultLists(t)
	for _, on := range []bool{false, true} {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			net := castNet(castXML, nil)
			p := newTestProvider(t, net)
			withPlex(t, p, net)
			noPlexLists(t, p)
			p.cfg.CastPhotosFromPlex = on
			s, err := p.buildShow(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			want, asked := anidbPhotos, 0
			if on {
				want, asked = plexPhotos, 1
			}
			if got := photosOf(s.Roles); got != want || len(s.Roles) != 3 || s.Roles[0].Name != "Yamadera Kouichi" || s.Roles[0].Role != "Spike" {
				t.Errorf("%s, on %v: cast %+v, want AniDB's with photos %q", root, on, s.Roles, want)
			}
			if c, all := plexAsked(net, "/library/metadata/p500"), plexAsked(net, ""); c != asked || all != 2*asked {
				t.Errorf("%s, on %v: Plex's show asked %d times (%d requests in all), want %d", root, on, c, all, asked)
			}
			if !s.Complete {
				t.Errorf("%s, on %v: want complete", root, on)
			}
		}
	}
}

// In /tvdb, Plex's show starts with TheTVDB's and TheMovieDb's values,
// before AniDB has answered.
func TestCastPhotosEarlyInTVDB(t *testing.T) {
	defaultLists(t)
	net := castNet(castXML, nil)
	p := newTestProvider(t, net)
	withPlex(t, p, net)
	noPlexLists(t, p)
	p.cfg.CastPhotosFromPlex = true
	entry := mapping.Entry{AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}}
	ss := p.newSources(context.Background(), 100, entry, 500, 77, false, firstEntry{true, true})
	ss.tvdbTargets(plexSeasons(&Show{Episodes: map[int][]Ep{1: nil}}))
	ss.warmEarly()
	ss.earlyWG.Wait()
	if c := plexAsked(net, "/library/metadata/p500"); c != 1 {
		t.Errorf("Plex's show asked %d times before AniDB's details, want 1", c)
	}
}

// Plex failing leaves the cast its own photos and the show incomplete,
// rebuilt soon; Plex not having the show leaves it complete. Without a
// cast, Plex's answer isn't read: a failure doesn't matter.
func TestCastPhotosPlexFails(t *testing.T) {
	defaultLists(t)
	for _, c := range []struct {
		name     string
		anidb    string
		plex     map[string]string
		complete bool
	}{
		{"plex fails", castXML, map[string]string{plexItemPath: "503 {}"}, false},
		{"plex doesn't have it", castXML, map[string]string{plexHost + "/library/metadata/matches/2/tvdb://500": "404 {}"}, true},
		{"no cast, plex fails", anidbXML, map[string]string{plexItemPath: "503 {}"}, true},
	} {
		for _, root := range []string{"tvdb-500", "anidb-100"} {
			net := castNet(c.anidb, c.plex)
			p := newTestProvider(t, net)
			withPlex(t, p, net)
			noPlexLists(t, p)
			p.cfg.CastPhotosFromPlex = true
			s, err := p.buildShow(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			want := anidbPhotos
			if c.anidb == anidbXML {
				want = ""
			}
			if got := photosOf(s.Roles); got != want {
				t.Errorf("%s, %s: photos %q, want %q", c.name, root, got, want)
			}
			if s.Complete != c.complete {
				t.Errorf("%s, %s: complete %v, want %v", c.name, root, s.Complete, c.complete)
			}
		}
	}
}

// A cast from plex is Plex's own, photos included: nothing to swap, and
// Plex's show is asked once.
func TestCastPhotosCastFromPlex(t *testing.T) {
	defaultLists(t)
	net := castNet(castXML, nil)
	p := newTestProvider(t, net)
	withPlex(t, p, net)
	p.cfg.CastSources = []string{"plex"}
	p.cfg.CastPhotosFromPlex = true
	s, err := p.buildShow(context.Background(), "tvdb-500")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Roles) != 2 || s.Roles[0] != (sources.Person{Name: "Koichi Yamadera", Role: "Spike Spiegel", Photo: "https://plex/yamadera.jpg"}) ||
		s.Roles[1].Photo != "" {
		t.Errorf("cast %+v, want Plex's as it is", s.Roles)
	}
	if c := plexAsked(net, "/library/metadata/p500"); c != 1 {
		t.Errorf("Plex's show asked %d times, want 1", c)
	}
}

// A series without a TheTVDB id isn't asked for: no Plex request, the cast
// keeps its photos, the show is complete.
func TestCastPhotosNoTVDBID(t *testing.T) {
	defaultLists(t)
	net := castNet(castXML, nil)
	p := newTestProvider(t, net)
	withPlex(t, p, net)
	noPlexLists(t, p)
	p.cfg.CastPhotosFromPlex = true
	entry := mapping.Entry{AID: 100, TMDB: mapping.Placement{ID: 77}}
	ctx := context.Background()
	a := &sources.AniDBAnime{Characters: []sources.AniDBCharacter{{Name: "Spike", Seiyuu: "Yamadera Kouichi", Photo: "own.jpg"}}}
	ss := p.newSources(ctx, 100, entry, 0, 77, false, firstEntry{tmdb: true})
	ss.setAniDB(ctx, a, entry)
	s := &Show{Episodes: map[int][]Ep{1: nil}}
	ss.anidbTargets(entry, plexSeasons(s))
	ss.fill(s)
	if photosOf(s.Roles) != "own.jpg" {
		t.Errorf("cast %+v, want its own photo", s.Roles)
	}
	if c := plexAsked(net, ""); c != 0 {
		t.Errorf("%d requests to Plex, want none", c)
	}
	if !ss.finish(s) {
		t.Error("want complete")
	}
}

// An /anidb entry that doesn't start its series gets the photos too, from
// its series' cast on Plex, though none of Plex's show fields.
func TestCastPhotosLaterEntry(t *testing.T) {
	defaultLists(t)
	second := strings.Replace(secondXML, "<episodes>", `<titles><title type="main" xml:lang="x-jat">Kizuna Test 2</title></titles><characters>
<character type="main character in"><name>Spike</name><seiyuu picture="yamadera.jpg">Yamadera Kouichi</seiyuu></character>
</characters><episodes>`, 1)
	// A studio on Plex's show: the entry must not get it.
	net := castNet(anidbXML, map[string]string{plexItemPath: "200 " +
		strings.Replace(plexCastItem, `"title":"Plex Name"`, `"title":"Plex Name","studio":"Plex Studio"`, 1)})
	inner := net.answer
	net.answer = func(path string, q map[string][]string) (int, string) {
		if path == "api.anidb.net/httpapi" && q["aid"][0] == "200" {
			return 200, second
		}
		return inner(path, q)
	}
	p := newTestProvider(t, net)
	withPlex(t, p, net)
	noPlexLists(t, p)
	p.cfg.CastPhotosFromPlex = true
	p.cfg.StudioSources = []string{"plex"}
	p.titles.SetForTest(map[int][]anidb.Title{100: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test"}},
		200: {{Type: "main", Lang: "x-jat", Text: "Kizuna Test 2"}}})
	p.lists.SetForTest(map[int]mapping.Entry{
		100: {AID: 100, TVDB: mapping.Placement{ID: 500, Season: 1, Mapped: true}, TMDB: mapping.Placement{ID: 77}},
		200: {AID: 200, TVDB: mapping.Placement{ID: 500, Season: 2, Mapped: true}},
	})
	s, err := p.buildShow(context.Background(), "anidb-200")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Roles) != 1 || s.Roles[0] != (sources.Person{Name: "Yamadera Kouichi", Role: "Spike", Photo: "https://plex/yamadera.jpg"}) {
		t.Errorf("cast %+v, want AniDB's with Plex's photo", s.Roles)
	}
	if s.Studio != "" {
		t.Errorf("studio %q: Plex's show fields are the first entry's", s.Studio)
	}
	if !s.Complete {
		t.Error("want complete")
	}
}

// The match report's Cast field ends the used source's text with the
// count of Plex photos, 0 too; without the swap, no count. With the count,
// the text describes the cast once its repeats are dropped, so both numbers
// count the same people.
func TestCastPhotosReport(t *testing.T) {
	defaultLists(t)
	const cast = "3: Yamadera Kouichi as Spike, Ishizuka Unshou as Jet, Hayashibara Megumi as Faye"
	// castXML with Spike's row twice: AniDB's cast has 4 rows, Plex gets 3.
	repeated := strings.Replace(castXML, "<character type=\"secondary cast in\"><name>Jet</name>",
		"<character type=\"main character in\"><name>Spike</name><seiyuu>Yamadera Kouichi</seiyuu></character>\n"+
			"<character type=\"secondary cast in\"><name>Jet</name>", 1)
	for _, c := range []struct {
		name  string
		anidb string
		on    bool
		plex  map[string]string
		want  string
	}{
		{"swapped", castXML, true, nil, cast + " · photos from Plex: 1 of 3"},
		{"none paired", castXML, true, map[string]string{plexItemPath: `200 {"MediaContainer":{"Metadata":[{"title":"Plex Name"}]}}`},
			cast + " · photos from Plex: 0 of 3"},
		{"repeated row", repeated, true, nil, cast + " · photos from Plex: 1 of 3"},
		{"off", castXML, false, nil, cast},
		{"off, repeated row", repeated, false, nil,
			"4: Yamadera Kouichi as Spike, Yamadera Kouichi as Spike, Ishizuka Unshou as Jet, …"},
	} {
		net := castNet(c.anidb, c.plex)
		p := newTestProvider(t, net)
		withPlex(t, p, net)
		noPlexLists(t, p)
		p.cfg.CastPhotosFromPlex = c.on
		store, dir, serve := reportsOn(t, p)
		serve("GET", "/tvdb/library/metadata/tvdb-500", "")
		store.Flush()
		var doc report.ShowDoc
		readJSON(t, filepath.Join(dir, "data", "anidb-100.json"), &doc)
		if len(doc.Builds) != 1 {
			t.Fatalf("%s: %d builds", c.name, len(doc.Builds))
		}
		i := slices.IndexFunc(doc.Builds[0].Fields, func(f report.Field) bool { return f.Name == "Cast" })
		if i < 0 {
			t.Fatalf("%s: no Cast field", c.name)
		}
		if f := doc.Builds[0].Fields[i]; f.Offered["anidb"] != c.want || !slices.Equal(f.Used, []string{"anidb"}) {
			t.Errorf("%s: Cast field %+v, want anidb's text %q", c.name, f, c.want)
		}
	}
}
