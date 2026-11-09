package provider

import (
	"fmt"
	"strconv"

	"kizuna/internal/sources"
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
	it.OriginalTitle, it.Summary = s.OriginalTitle, s.Summary
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
	it.Image = s.images
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

// setImages makes the show's and its seasons' image lists, once, when the
// build is done: every request shares them.
func (s *Show) setImages() {
	s.images = artImages(s.Art, s.Title)
	if len(s.SeasonArt) > 0 {
		s.seasonImages = make(map[int][]Image, len(s.SeasonArt))
		for n, a := range s.SeasonArt {
			if imgs := artImages(a, s.Title); imgs != nil {
				s.seasonImages[n] = imgs
			}
		}
	}
}

// artImages lists up to 10 images of each kind, in Plex's image types.
func artImages(a sources.Art, alt string) []Image {
	const most = 10 // plenty to choose from in Plex's Edit dialog
	size := min(len(a.Posters), most) + min(len(a.Backgrounds), most) + min(len(a.Logos), most)
	if size == 0 {
		return nil
	}
	out := make([]Image, 0, size)
	for _, k := range []struct {
		typ  string
		urls []string
	}{{"coverPoster", a.Posters}, {"background", a.Backgrounds}, {"clearLogo", a.Logos}} {
		for _, u := range k.urls[:min(len(k.urls), most)] {
			out = append(out, Image{Type: k.typ, URL: u, Alt: alt})
		}
	}
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
		it.ParentThumb = s.Art.Posters[0]
	}
	// The season's own art (art.go). A season built without any (no
	// SeasonArt for it) has the show's poster.
	it.Image = s.seasonImages[n]
	if a, ok := s.SeasonArt[n]; ok {
		if len(a.Posters) > 0 {
			it.Thumb = a.Posters[0]
		}
		if len(a.Backgrounds) > 0 {
			it.Art = a.Backgrounds[0]
		}
	} else {
		it.Thumb = it.ParentThumb
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
	for _, d := range e.Directors {
		it.Director = append(it.Director, Tag{d})
	}
	for _, w := range e.Writers {
		it.Writer = append(it.Writer, Tag{w})
	}
	if s.episodeGuids && e.tvdbEp > 0 {
		it.Guid = []GuidTag{{fmt.Sprintf("tvdb://%d", e.tvdbEp)}}
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
	if site, aid, ok := parseRoot(c.root); ok {
		if site == "tvdb" {
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
