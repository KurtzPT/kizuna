package provider

import (
	"fmt"
	"strconv"
)

// Rating keys:
//   show     anidb-123          tvdb-456
//   season   anidb-123-s1       tvdb-456-s2
//   episode  anidb-123-s1-e5    tvdb-456-s2-e33

const metadataPath = "/library/metadata"

func base(mode Mode, typ, rk, title string) Item {
	key := metadataPath + "/" + rk
	if typ != "episode" {
		key += "/children"
	}
	return Item{RatingKey: rk, Key: key, GUID: mode.Identifier() + "://" + typ + "/" + rk, Type: typ, Title: title}
}

func (p *Provider) showItem(mode Mode, s *Show, withChildren bool) Item {
	it := base(mode, "show", s.Root, s.Title)
	it.OriginalTitle, it.TitleSort, it.Summary = s.OriginalTitle, s.SortTitle, s.Summary
	it.Studio, it.ContentRating, it.Duration = s.Studio, s.ContentRating, s.Minutes*60000
	setDate(&it, s.Date)
	for _, g := range s.Genres {
		it.Genre = append(it.Genre, Tag{g})
	}
	for _, d := range s.Directors {
		it.Director = append(it.Director, Tag{d})
	}
	for _, w := range s.Writers {
		it.Writer = append(it.Writer, Tag{w})
	}
	for i, r := range s.Roles {
		it.Role = append(it.Role, Role{Tag: r.Name, Role: r.Role, Thumb: r.Photo, Order: i + 1})
	}
	for _, g := range s.Guids {
		it.Guid = append(it.Guid, GuidTag{g})
	}
	it.Image = showImages(s)
	if len(s.Art.Posters) > 0 {
		it.Thumb = s.Art.Posters[0]
	}
	if len(s.Art.Backgrounds) > 0 {
		it.Art = s.Art.Backgrounds[0]
	}
	if withChildren {
		var seasons []Item
		for _, n := range s.Seasons() {
			seasons = append(seasons, *p.seasonItem(mode, s, n, false))
		}
		it.Children = &Children{Size: len(seasons), Metadata: seasons}
	}
	return it
}

func showImages(s *Show) []Image {
	var out []Image
	add := func(typ string, urls []string) {
		for i, u := range urls {
			if i == 10 { // plenty to choose from in Plex's Edit dialog
				break
			}
			out = append(out, Image{Type: typ, URL: u, Alt: s.Title})
		}
	}
	add("coverPoster", s.Art.Posters)
	add("background", s.Art.Backgrounds)
	add("clearLogo", s.Art.Logos)
	return out
}

func seasonKey(s *Show, n int) string { return s.Root + "-s" + strconv.Itoa(n) }

func seasonTitle(n int) string {
	if n == 0 {
		return "Specials"
	}
	return fmt.Sprintf("Season %d", n)
}

func (p *Provider) seasonItem(mode Mode, s *Show, n int, withChildren bool) *Item {
	eps, ok := s.Episodes[n]
	if !ok && !(s.Placeholders && n == 0) {
		return nil
	}
	it := base(mode, "season", seasonKey(s, n), seasonTitle(n))
	it.Index = intp(n)
	it.ParentRatingKey, it.ParentKey = s.Root, metadataPath+"/"+s.Root
	it.ParentGUID, it.ParentType, it.ParentTitle = mode.Identifier()+"://show/"+s.Root, "show", s.Title
	if len(s.Art.Posters) > 0 {
		it.Thumb, it.ParentThumb = s.Art.Posters[0], s.Art.Posters[0]
	}
	for _, e := range eps { // season date = first aired episode
		if e.Date != "" {
			setDate(&it, e.Date)
			break
		}
	}
	if withChildren {
		var children []Item
		for i := range eps {
			children = append(children, p.episodeItem(mode, s, &eps[i]))
		}
		it.Children = &Children{Size: len(children), Metadata: children}
	}
	return &it
}

func (p *Provider) episodeItem(mode Mode, s *Show, e *Ep) Item {
	sk := seasonKey(s, e.Season)
	title := e.Title
	if title == "" {
		title = fmt.Sprintf("Episode %d", e.Index)
	}
	it := base(mode, "episode", fmt.Sprintf("%s-e%d", sk, e.Index), title)
	it.Index, it.ParentIndex = intp(e.Index), intp(e.Season)
	it.Summary, it.Duration = e.Summary, e.Minutes*60000
	setDate(&it, e.Date)
	it.ParentRatingKey, it.ParentKey = sk, metadataPath+"/"+sk
	it.ParentGUID, it.ParentType, it.ParentTitle = mode.Identifier()+"://season/"+sk, "season", seasonTitle(e.Season)
	it.GrandparentRatingKey, it.GrandparentKey = s.Root, metadataPath+"/"+s.Root
	it.GrandparentGUID, it.GrandparentType, it.GrandparentTitle = mode.Identifier()+"://show/"+s.Root, "show", s.Title
	if len(s.Art.Posters) > 0 {
		it.GrandparentThumb = s.Art.Posters[0]
	}
	if e.Thumb != "" {
		it.Thumb = e.Thumb
		it.Image = []Image{{Type: "snapshot", URL: e.Thumb, Alt: title}}
	}
	return it
}

// lightItem is a "Fix Match" search result built from the title dump only.
func (p *Provider) lightItem(mode Mode, c candidate) Item {
	title := c.root
	if m := rootRe.FindStringSubmatch(c.root); m != nil {
		aid, _ := strconv.Atoi(m[2])
		if m[1] == "tvdb" {
			aid = p.lists.FirstAID(aid)
		}
		if t := p.titles.Of(aid); len(t) > 0 {
			title = p.showTitle(t)
		}
	}
	it := base(mode, "show", c.root, title)
	it.Summary = c.root // shows the id in Plex's match list
	it.Score = c.score
	return it
}

func setDate(it *Item, d string) {
	d = fullDate(d)
	if len(d) < 10 {
		return
	}
	it.OriginallyAvailableAt = d[:10]
	it.Year, _ = strconv.Atoi(d[:4])
}
