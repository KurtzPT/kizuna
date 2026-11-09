package sources

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"kizuna/internal/fetch"
)

// annAPI is a variable so tests can point it at a fake server.
var annAPI = "https://cdn.animenewsnetwork.com/encyclopedia/api.xml?anime=%d"

// ANN reads the Anime News Network encyclopedia (public XML API, no key).
// ANN's rules: at most 1 request per second, cache for about a week, and
// credit ANN with a link to its encyclopedia. Data from the Anime News
// Network encyclopedia, https://www.animenewsnetwork.com/encyclopedia/ (the
// credit is here and in the README's footer). Like AniDB, ANN has
// one entry per season or movie.
type ANN struct{ f *fetch.Client }

func NewANN(f *fetch.Client) *ANN {
	f.Throttle("cdn.animenewsnetwork.com", time.Second)
	return &ANN{f: f}
}

// ANNAnime is the part of an ANN entry we use.
type ANNAnime struct {
	ID      int
	Title   string // main title
	Summary string
	Poster  string // full-size picture
	Details Details
}

// annRatings: ANN's "Objectionable content" codes.
var annRatings = map[string]string{"AA": "All Ages", "OC": "Older Children", "TA": "Teens and Adults", "MA": "Mature"}

// annDate finds the first date in a "Vintage" line ("1998-04-03 to ... (first run)").
var annDate = regexp.MustCompile(`^\d{4}(-\d{2}(-\d{2})?)?`)

func (n *ANN) Anime(ctx context.Context, id int) (ANNAnime, error) {
	if id <= 0 {
		return ANNAnime{}, errors.New("ann: no id")
	}
	data, err := n.f.Get(ctx, fetch.Request{URL: fmt.Sprintf(annAPI, id), Key: fmt.Sprintf("ann/%d.xml", id),
		TTL: 7 * 24 * time.Hour, Validate: func(b []byte) error {
			if !inHead(b, "<ann") {
				return errors.New("not an ANN answer")
			}
			return nil
		}})
	if err != nil {
		return ANNAnime{}, err
	}
	return parseANN(data, id)
}

func parseANN(data []byte, id int) (ANNAnime, error) {
	var doc struct {
		Anime []struct {
			Info []struct {
				Type string `xml:"type,attr"`
				Lang string `xml:"lang,attr"`
				Text string `xml:",chardata"`
				Img  []struct {
					Src    string `xml:"src,attr"`
					Width  int    `xml:"width,attr"`
					Height int    `xml:"height,attr"`
				} `xml:"img"`
			} `xml:"info"`
			Staff []struct {
				Task   string `xml:"task"`
				Person string `xml:"person"`
			} `xml:"staff"`
			Cast []struct {
				Lang   string `xml:"lang,attr"`
				Role   string `xml:"role"`
				Person string `xml:"person"`
			} `xml:"cast"`
			Credit []struct {
				Task    string `xml:"task"`
				Company string `xml:"company"`
			} `xml:"credit"`
		} `xml:"anime"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return ANNAnime{}, err
	}
	a := ANNAnime{ID: id}
	if len(doc.Anime) == 0 { // <warning>no result for anime=N</warning>
		return a, nil
	}
	x := doc.Anime[0]
	var dates []string
	for _, in := range x.Info {
		text := strings.TrimSpace(in.Text)
		switch in.Type {
		case "Main title":
			a.Title = text
		case "Plot Summary":
			a.Summary = text
		case "Genres":
			a.Details.Genres = append(a.Details.Genres, capitalize(text))
		case "Objectionable content":
			a.Details.ContentRating = annRatings[text]
		case "Vintage":
			if d := annDate.FindString(text); d != "" {
				dates = append(dates, d)
			}
		case "Picture":
			best := 0
			for _, img := range in.Img {
				if size := img.Width * img.Height; size >= best {
					best, a.Poster = size, fullANNImage(img.Src)
				}
			}
		}
	}
	if len(dates) > 0 { // the earliest is the original (Japanese) run
		sort.Strings(dates)
		a.Details.Date = dates[0]
	}
	for _, s := range x.Staff {
		switch s.Task {
		case "Director", "Series Director", "Chief Director":
			a.Details.Directors = appendNew(a.Details.Directors, s.Person)
		case "Series Composition", "Original creator", "Original Concept", "Original story", "Original Work":
			a.Details.Writers = appendNew(a.Details.Writers, s.Person)
		}
	}
	for _, c := range x.Cast {
		if c.Lang == "JA" { // the original cast, like AniDB's seiyuu
			a.Details.Cast = append(a.Details.Cast, Person{Name: c.Person, Role: c.Role})
		}
	}
	for _, c := range x.Credit {
		if c.Task == "Animation Production" && a.Details.Studio == "" {
			a.Details.Studio = c.Company
		}
	}
	return a, nil
}

// fullANNImage turns a thumbnail URL
// (cdn.animenewsnetwork.com/thumbnails/fit200x200/encyc/A13-15.jpg) into the
// full-size picture (www.animenewsnetwork.com/images/encyc/A13-15.jpg).
func fullANNImage(src string) string {
	if i := strings.Index(src, "/encyc/"); i >= 0 && strings.Contains(src, "/thumbnails/") {
		return "https://www.animenewsnetwork.com/images" + src[i:]
	}
	return src
}
