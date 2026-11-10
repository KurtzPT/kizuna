package sources

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"kizuna/internal/fetch"
)

const malURL = "api.myanimelist.net/v2/anime/%d?fields=synopsis,main_picture,genres,studios,start_date,rating"

// MAL's anime details (API v2, documented shape): the rewrite credit is
// cut from the synopsis, the rating mapped to the US label, the large
// picture preferred, the medium one when there's no large.
func TestMALAnime(t *testing.T) {
	f := fetch.New(t.TempDir())
	f.SetTransport(&tvdbNet{answers: map[string]string{
		fmt.Sprintf(malURL, 52991): `{"id": 52991, "title": "Sousou no Frieren",
		  "main_picture": {"medium": "https://cdn.myanimelist.net/images/anime/1015/138006.jpg",
		    "large": "https://cdn.myanimelist.net/images/anime/1015/138006l.jpg"},
		  "synopsis": "  During their decade-long quest...\n\n[Written by MAL Rewrite]\n", "mean": 9.3,
		  "genres": [{"id": 2, "name": "Adventure"}, {"id": 8, "name": "Drama"}],
		  "studios": [{"id": 11, "name": "Madhouse"}, {"id": 2, "name": "Other"}],
		  "start_date": "2023-09-29", "rating": "pg_13"}`,
		fmt.Sprintf(malURL, 1): `{"id": 1, "title": "Cowboy Bebop",
		  "main_picture": {"medium": "https://cdn.myanimelist.net/images/anime/4/19644.jpg"},
		  "synopsis": "Bounty hunters.", "genres": [], "studios": [], "start_date": "1998-04", "rating": "unknown"}`,
	}})
	m := NewMAL(f, "client")
	ctx := context.Background()
	a, err := m.Anime(ctx, 52991)
	if err != nil {
		t.Fatal(err)
	}
	if a.Synopsis != "During their decade-long quest..." || a.Poster != "https://cdn.myanimelist.net/images/anime/1015/138006l.jpg" {
		t.Errorf("synopsis %q poster %q", a.Synopsis, a.Poster)
	}
	d := a.Details
	if d.Date != "2023-09-29" || strings.Join(d.Genres, ",") != "Adventure,Drama" || d.Studio != "Madhouse" || d.Ratings["US"] != "PG-13" {
		t.Errorf("details %+v", d)
	}

	b, err := m.Anime(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if b.Synopsis != "Bounty hunters." || b.Poster != "https://cdn.myanimelist.net/images/anime/4/19644.jpg" ||
		b.Details.Date != "1998-04" || b.Details.Studio != "" || len(b.Details.Ratings) != 0 {
		t.Errorf("medium poster, unknown rating: %+v", b)
	}
	if _, err := NewMAL(f, "").Anime(ctx, 1); err == nil {
		t.Error("no client id: want an error")
	}
}
