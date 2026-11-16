package sources

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// FuzzParseAniDB: any AniDB API answer parses without a panic, and what it
// returns keeps the shape the provider relies on.
func FuzzParseAniDB(f *testing.F) {
	f.Add([]byte(anidbSample))
	f.Add([]byte(`<anime id="1"><episodes><episode><epno type="3">C1</epno><title>Ending</title></episode></episodes></anime>`))
	f.Add([]byte(`<anime id="2"><episodes><episode><epno type="1"> 007 </epno></episode><episode><epno type="9">x</epno></episode></episodes></anime>`))
	f.Add([]byte(`<error>Banned</error>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		a, err := parseAniDB(data)
		if err != nil {
			return
		}
		if !slices.IsSorted(a.Genres) {
			t.Errorf("genres not sorted: %q", a.Genres)
		}
		for i, c := range a.Characters {
			if c.Seiyuu == "" {
				t.Errorf("character %d without a seiyuu", i)
			}
			if i > 0 && c.Main && !a.Characters[i-1].Main {
				t.Errorf("main character %d after a secondary one", i)
			}
		}
		for i, e := range a.Episodes {
			if e.EpNo == "" {
				t.Errorf("episode %d without an AniDB number", i)
			}
			if e.Season != 0 && e.Season != 1 {
				t.Errorf("episode %s in season %d", e.EpNo, e.Season)
			}
			if i > 0 {
				p := a.Episodes[i-1]
				if p.Season > e.Season || p.Season == e.Season && p.Index > e.Index {
					t.Errorf("episodes out of order: %s before %s", p.EpNo, e.EpNo)
				}
			}
		}
	})
}

// FuzzParseANN: any ANN encyclopedia answer parses without a panic.
func FuzzParseANN(f *testing.F) {
	f.Add([]byte(annSample))
	f.Add([]byte(`<ann><warning>no result for anime=9</warning></ann>`))
	f.Add([]byte(`<ann><anime><info type="Picture"><img src="/thumbnails/x/encyc/A1.jpg" width="-1" height="5"/></info></anime></ann>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		a, err := parseANN(data)
		if err != nil {
			return
		}
		for _, list := range [][]string{a.Details.Directors, a.Details.Writers} {
			seen := map[string]bool{}
			for _, p := range list {
				if seen[p] {
					t.Errorf("%q listed twice in %q", p, list)
				}
				seen[p] = true
			}
		}
	})
}

// FuzzParsePlex: any answer from Plex's metadata service decodes or fails
// without a panic, and what Kizuna takes from it keeps its limits.
//
// The seeds are the shapes of testdata/plex's answers (a show's fields,
// people and pictures, a movie, a season's episodes, a show's /images, no
// match), cut to about 200 bytes each. Not the files themselves: the
// fuzzer minimizes each input that finds new code, trying every pair of
// cut points, so its time grows with the square of the input's size, and
// a seed of a few KB took the whole run.
func FuzzParsePlex(f *testing.F) {
	f.Add([]byte(`{"MediaContainer":{"totalSize":1,"Metadata":[{"ratingKey":"5d9f","type":"show","title":"F","summary":"S",
"index":null,"duration":1440000,"studio":"M","contentRating":"TV-14","originallyAvailableAt":"2023-09-29","thumb":"/1",
"guid":"plex://show/5d9f","Guid":[{"id":"tvdb://4"}]}]}}`))
	f.Add([]byte(`{"MediaContainer":{"Metadata":[{"Genre":[{"tag":"Anime"}],"Director":[{"tag":"D"},{"tag":"E","thumb":"/2"}],
"Writer":[{"tag":"W"}],"Role":[{"order":2,"role":"R","tag":"B","thumb":"/3"},{"order":1,"role":"Q","tag":"A"}]}]}}`))
	f.Add([]byte(`{"MediaContainer":{"Metadata":[{"Guid":[{"id":"tvdb://4"}],"Image":[{"type":"coverPoster","url":"/4"},
{"type":"background","url":"/5"},{"type":"clearLogo","url":""}]}]}}`))
	f.Add([]byte(`{"MediaContainer":{"Metadata":[{"type":"movie","duration":6360000,"contentRating":"de/12",
"Producer":[{"tag":"P","thumb":"/6"}]}]}}`))
	f.Add([]byte(`{"MediaContainer":{"totalSize":3,"Metadata":[{"index":1,"parentIndex":1,"title":"E","summary":null,
"studio":null,"duration":null,"Director":[{"tag":"D"}]},{"index":2,"duration":1500000}]}}`))
	f.Add([]byte(`{"MediaContainer":{"Art":[{"height":1080,"key":"/8","source":"tmdb"}],"Thumb":[{"key":"/9","source":"tvdb"}]}}`))
	f.Add([]byte(`{"MediaContainer":{"identifier":"tv.plex.provider.metadata","offset":0,"size":0,"totalSize":0}}`))
	f.Add([]byte(`{"MediaContainer":{"Metadata":[{"Studio":[{"tag":"x"}],"studio":"y","Guid":[],"guid":"plex://show/1","rating":9,"Rating":[],"contentRating":"/"}]}}`))
	f.Add([]byte(`{"MediaContainer":{"Metadata":[{"duration":-90000,"Role":[{"tag":" ","order":-1}],"Director":[{"tag":""}]}]}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var c plexContainer
		if json.Unmarshal(data, &c) != nil {
			return
		}
		for _, it := range c.MediaContainer.Metadata {
			d, _ := it.details()
			if len(d.Cast) > plexCast {
				t.Errorf("%d cast", len(d.Cast))
			}
			for _, p := range d.Cast {
				if strings.TrimSpace(p.Name) == "" {
					t.Errorf("cast member without a name: %+v", p)
				}
			}
			for _, list := range [][]string{d.Genres, d.Directors, d.Writers} {
				if slices.Contains(list, "") {
					t.Errorf("empty name in %q", list)
				}
			}
			if e := it.episode(1); e.Minutes < 0 || e.Season != 1 {
				t.Errorf("episode %+v", e)
			}
		}
	})
}
