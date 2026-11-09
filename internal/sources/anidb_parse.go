package sources

import (
	"bytes"
	"encoding/xml"
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"kizuna/internal/anidb"
)

// validate rejects answers that must not be cached, and returns the
// parsed entry of one that may.
func (c *AniDB) validate(b []byte) (*AniDBAnime, error) {
	if inHead(b, "<error") {
		head := b[:min(len(b), headSize)]
		return nil, &anidbEntryError{msg: strings.TrimSpace(string(head)), banned: bytes.Contains(bytes.ToLower(head), []byte("banned"))}
	}
	if !inHead(b, "<anime") {
		return nil, errors.New("anidb: unexpected answer") // e.g. a maintenance page: back off
	}
	a, err := parseAniDB(b)
	if err != nil {
		return nil, &anidbEntryError{msg: "unreadable XML: " + err.Error()}
	}
	return a, nil
}

// headSize: an answer's root element (or its error tag) is in its first
// bytes; looking only there skips the rest of a big answer.
const headSize = 300

// inHead reports whether tag appears in the first headSize bytes of b.
func inHead(b []byte, tag string) bool {
	return bytes.Contains(b[:min(len(b), headSize)], []byte(tag))
}

const anidbPicture = "https://cdn.anidb.net/images/main/"

type anidbXML struct {
	ID          int           `xml:"id,attr"`
	Restricted  bool          `xml:"restricted,attr"`
	Type        string        `xml:"type"`
	StartDate   string        `xml:"startdate"`
	EndDate     string        `xml:"enddate"`
	Titles      []anidb.Title `xml:"titles>title"`
	Description string        `xml:"description"`
	Picture     string        `xml:"picture"`
	Creators    []struct {
		Type string `xml:"type,attr"`
		Name string `xml:",chardata"`
	} `xml:"creators>name"`
	Tags []struct {
		Infobox bool   `xml:"infobox,attr"`
		Name    string `xml:"name"`
	} `xml:"tags>tag"`
	Characters []struct {
		Type   string `xml:"type,attr"`
		Name   string `xml:"name"`
		Seiyuu struct {
			Picture string `xml:"picture,attr"`
			Name    string `xml:",chardata"`
		} `xml:"seiyuu"`
	} `xml:"characters>character"`
	Resources []struct {
		Type string   `xml:"type,attr"`
		IDs  []string `xml:"externalentity>identifier"`
	} `xml:"resources>resource"`
	Episodes []struct {
		EpNo struct {
			Type int    `xml:"type,attr"`
			Text string `xml:",chardata"`
		} `xml:"epno"`
		Length  int           `xml:"length"`
		AirDate string        `xml:"airdate"`
		Titles  []anidb.Title `xml:"title"`
		Summary string        `xml:"summary"`
	} `xml:"episodes>episode"`
}

func parseAniDB(data []byte) (*AniDBAnime, error) {
	var x anidbXML
	if err := xml.Unmarshal(data, &x); err != nil {
		return nil, err
	}
	a := &AniDBAnime{AID: x.ID, Type: x.Type, Restricted: x.Restricted, StartDate: x.StartDate, EndDate: x.EndDate,
		Titles: x.Titles, Description: anidb.CleanSummary(x.Description)}
	if x.Picture != "" {
		a.Picture = anidbPicture + x.Picture
	}
	for _, c := range x.Creators {
		a.Creators = append(a.Creators, AniDBCreator{c.Type, strings.TrimSpace(c.Name)})
	}
	for _, t := range x.Tags {
		if t.Infobox { // only AniDB's "infobox" tags become genres
			a.Genres = append(a.Genres, capitalize(t.Name))
		}
	}
	sort.Strings(a.Genres)
	for _, ch := range x.Characters {
		if ch.Type != "main character in" && ch.Type != "secondary cast in" || strings.TrimSpace(ch.Seiyuu.Name) == "" {
			continue
		}
		c := AniDBCharacter{Name: ch.Name, Seiyuu: strings.TrimSpace(ch.Seiyuu.Name), Main: ch.Type == "main character in"}
		if ch.Seiyuu.Picture != "" {
			c.Photo = anidbPicture + ch.Seiyuu.Picture
		}
		a.Characters = append(a.Characters, c)
	}
	sort.SliceStable(a.Characters, func(i, j int) bool { return a.Characters[i].Main && !a.Characters[j].Main })
	for _, r := range x.Resources {
		for _, id := range r.IDs {
			n, err := strconv.Atoi(id)
			if err != nil {
				continue
			}
			switch r.Type { // AniDB resource types
			case "1":
				a.ANNIDs = append(a.ANNIDs, n)
			case "2":
				a.MALIDs = append(a.MALIDs, n)
			}
		}
	}

	// Episodes: see the AniDBEpisode doc comment for the numbering.
	type raw struct {
		typ, n int
		i      int
	}
	order := make([]raw, 0, len(x.Episodes))
	for i, e := range x.Episodes {
		no := strings.TrimSpace(e.EpNo.Text)
		digits := strings.TrimLeft(no, "SCTPO")
		n, err := strconv.Atoi(digits)
		if err != nil {
			continue
		}
		order = append(order, raw{e.EpNo.Type, n, i})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].typ != order[j].typ {
			return order[i].typ < order[j].typ
		}
		return order[i].n < order[j].n
	})
	endingStart := 1 << 30 // first "C" number that is an ending (its title starts with "Ending")
	for _, r := range order {
		e := x.Episodes[r.i]
		if r.typ == 3 && r.n < endingStart && len(e.Titles) > 0 && strings.HasPrefix(e.Titles[0].Text, "Ending") {
			endingStart = r.n
		}
	}
	for _, r := range order {
		e := x.Episodes[r.i]
		ep := AniDBEpisode{EpNo: strings.TrimSpace(e.EpNo.Text), Titles: e.Titles, AirDate: e.AirDate,
			Summary: anidb.CleanSummary(e.Summary), Minutes: e.Length}
		switch r.typ {
		case 1:
			ep.Season, ep.Index = 1, r.n
		case 2:
			ep.Index = r.n
		case 3:
			if r.n >= endingStart {
				ep.Index = 151 + r.n - endingStart
			} else {
				ep.Index = 100 + r.n
			}
		case 4:
			ep.Index = 200 + r.n
		case 5:
			ep.Index = 300 + r.n
		case 6:
			ep.Index = 400 + r.n
		default:
			continue
		}
		a.Episodes = append(a.Episodes, ep)
	}
	sort.SliceStable(a.Episodes, func(i, j int) bool {
		if a.Episodes[i].Season != a.Episodes[j].Season {
			return a.Episodes[i].Season < a.Episodes[j].Season
		}
		return a.Episodes[i].Index < a.Episodes[j].Index
	})
	return a, nil
}

// Details converts AniDB's show fields to the common shape.
func (a *AniDBAnime) Details() Details {
	d := Details{Genres: a.Genres, Date: a.StartDate} // may be "2004" or "2004-05"; the provider completes it
	if a.Restricted {
		d.ContentRating = "X"
	}
	for _, c := range a.Characters {
		d.Cast = append(d.Cast, Person{Name: c.Seiyuu, Role: c.Name, Photo: c.Photo})
	}
	for _, c := range a.Creators {
		switch c.Role {
		case "Animation Work":
			if d.Studio == "" {
				d.Studio = c.Name
			}
		case "Direction":
			d.Directors = append(d.Directors, c.Name)
		case "Original Work", "Series Composition":
			d.Writers = append(d.Writers, c.Name)
		}
	}
	return d
}

// IsMovie: an AniDB movie becomes S01E01 in a TV library.
func (a *AniDBAnime) IsMovie() bool { return a.Type == "Movie" }

// capitalize: "action" -> "Action", "science fiction" -> "Science Fiction".
func capitalize(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		r, n := utf8.DecodeRuneInString(w)
		words[i] = string(unicode.ToUpper(r)) + w[n:]
	}
	return strings.Join(words, " ")
}
