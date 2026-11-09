package sources

import "testing"

const annSample = `<ann><anime id="13" name="Cowboy Bebop" type="TV">
<info gid="1" type="Picture" src="https://cdn.animenewsnetwork.com/thumbnails/fit200x200/encyc/A13-15.jpg" width="168" height="200"><img src="https://cdn.animenewsnetwork.com/thumbnails/fit200x200/encyc/A13-15.jpg" width="168" height="200"/></info>
<info gid="2" type="Main title" lang="EN">Cowboy Bebop</info>
<info gid="3" type="Genres">science fiction</info>
<info gid="4" type="Genres">drama</info>
<info gid="5" type="Objectionable content">TA</info>
<info gid="6" type="Plot Summary">Bounty hunters in space.</info>
<info gid="7" type="Vintage">2001-09-02 to 2002-02-15 (USA)</info>
<info gid="8" type="Vintage">1998-04-03 to 1998-06-19 (first run, 12 eps)</info>
<staff gid="9"><task>Series Director</task><person id="774">Shinichiro Watanabe</person></staff>
<staff gid="10"><task>Series Composition</task><person id="1026">Keiko Nobumoto</person></staff>
<staff gid="11"><task>Storyboard</task><person id="1">Someone</person></staff>
<cast gid="12" lang="EN"><role>Spike Spiegel</role><person id="2">Steven Blum</person></cast>
<cast gid="13" lang="JA"><role>Spike Spiegel</role><person id="3">Koichi Yamadera</person></cast>
<credit gid="14"><task>Production</task><company id="18">Bandai Visual</company></credit>
<credit gid="15"><task>Animation Production</task><company id="34">Sunrise</company></credit>
</anime></ann>`

func TestParseANN(t *testing.T) {
	a, err := parseANN([]byte(annSample), 13)
	if err != nil {
		t.Fatal(err)
	}
	d := a.Details
	switch {
	case a.Title != "Cowboy Bebop" || a.Summary != "Bounty hunters in space.":
		t.Errorf("texts %q %q", a.Title, a.Summary)
	case a.Poster != "https://www.animenewsnetwork.com/images/encyc/A13-15.jpg":
		t.Errorf("poster %q", a.Poster)
	case len(d.Genres) != 2 || d.Genres[0] != "Science Fiction":
		t.Errorf("genres %v", d.Genres)
	case d.ContentRating != "Teens and Adults" || d.Date != "1998-04-03" || d.Studio != "Sunrise":
		t.Errorf("rating %q date %q studio %q", d.ContentRating, d.Date, d.Studio)
	case len(d.Directors) != 1 || len(d.Writers) != 1:
		t.Errorf("crew %v %v", d.Directors, d.Writers)
	case len(d.Cast) != 1 || d.Cast[0].Name != "Koichi Yamadera" || d.Cast[0].Role != "Spike Spiegel":
		t.Errorf("cast %v", d.Cast)
	}
	if empty, err := parseANN([]byte(`<ann><warning>no result for anime=9</warning></ann>`), 9); err != nil || empty.Title != "" {
		t.Errorf("no-result answer: %+v %v", empty, err)
	}
}

// The earliest Vintage date wins; a less precise one in the same year or
// month gives way to the full date.
func TestEarliestDate(t *testing.T) {
	for _, c := range []struct {
		dates []string
		want  string
	}{
		{nil, ""},
		{[]string{"2001-09-02", "1998-04-03"}, "1998-04-03"},
		{[]string{"1998", "1998-09-25", "1998-04-03"}, "1998-04-03"},
		{[]string{"1998-04", "1998"}, "1998-04"},
		{[]string{"1998", "1999-01-01"}, "1998"},
	} {
		if got := earliestDate(c.dates); got != c.want {
			t.Errorf("%v: got %q, want %q", c.dates, got, c.want)
		}
	}
}
