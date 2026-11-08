package sources

import "testing"

func TestRatingCountry(t *testing.T) {
	d := Details{}
	d.addRating("US", "TV-14")
	d.addRating("DE", "12")
	d.addRating("JP", "G")
	for _, c := range []struct {
		countries []string
		want      string
	}{{[]string{"US"}, "TV-14"}, {[]string{"de", "US"}, "de/12"}, {[]string{"FR"}, ""}} {
		if got := d.Rating(c.countries); got != c.want {
			t.Errorf("%v: got %q want %q", c.countries, got, c.want)
		}
	}
	if got := (Details{Ratings: map[string]string{"JP": "G", "GB": "15"}}).AnyRating(); got != "gb/15" {
		t.Errorf("any: got %q", got)
	}
	if got := (Details{ContentRating: "X"}).Rating([]string{"DE"}); got != "X" {
		t.Errorf("country-less: got %q", got)
	}
	if alpha2("deu") != "DE" || alpha2("xyz") != "XYZ" {
		t.Error("alpha2")
	}
}
