package sources

import (
	"testing"
	"time"

	"kizuna/internal/fetch"
)

// Each memo keeps its values as long as decided: AniDB's entries an hour,
// swept every 5 minutes; the others a minute, swept at each value kept.
// (How a life and a sweep interval work is fetch.Memo's test.)
func TestMemoLives(t *testing.T) {
	f := fetch.New(t.TempDir())
	tv, tm := NewTVDB(f, "key", "", ""), NewTMDB(f, "key")
	type lives interface {
		Life() (time.Duration, time.Duration)
	}
	for _, c := range []struct {
		name        string
		m           lives
		life, every time.Duration
	}{
		{"anidb", newAniDBMemo(), time.Hour, 5 * time.Minute},
		{"tvdb series", tv.series, time.Minute, 0},
		{"tvdb pages", tv.pages, time.Minute, 0},
		{"tmdb shows", tm.shows, time.Minute, 0},
		{"tmdb seasons", tm.seasons, time.Minute, 0},
	} {
		if life, every := c.m.Life(); life != c.life || every != c.every {
			t.Errorf("%s: life %v, sweep every %v; want %v, %v", c.name, life, every, c.life, c.every)
		}
	}
}
