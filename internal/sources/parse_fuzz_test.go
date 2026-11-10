package sources

import (
	"slices"
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
