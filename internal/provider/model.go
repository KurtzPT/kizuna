package provider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"kizuna/internal/anidb"
	"kizuna/internal/fetch"
	"kizuna/internal/sources"
)

// Show is the source-independent model of one Plex show. Both modes build
// it; plex JSON is produced from it in items.go.
type Show struct {
	Root          string // rating key of the show: "anidb-123" or "tvdb-456"
	Title         string
	OriginalTitle string
	SortTitle     string
	Summary       string
	Date          string // YYYY-MM-DD
	Studio        string
	ContentRating string
	Minutes       int
	Genres        []string
	Directors     []string
	Writers       []string
	Roles         []sources.Person // actor + character
	Guids         []string         // tvdb://1, tmdb://2, imdb://tt3
	Art           sources.Art
	Episodes      map[int][]Ep // season -> episodes sorted by index
	// Complete: false when something was missing (AniDB details, a failed or
	// partial TVDB/TMDB episode list, a failed show-level source, the build's
	// time running out): the model is cached briefly and rebuilt.
	Complete bool
	// Placeholders: the episode list is unknown (an /anidb show built without
	// AniDB details), so any S01/S00 episode Plex asks for is a bare
	// "Episode N" and the files still match. The next refresh with AniDB
	// details replaces them.
	Placeholders bool
}

// Ep is one episode at the season/index Plex's scanner gives the file.
type Ep struct {
	Season, Index int
	Title         string // picked per EPISODE_TITLES (episodes.go)
	Summary       string
	Date          string
	Thumb         string
	Minutes       int
	Directors     []string // TheTVDB's, else TheMovieDb's (episodeCrew)
	Writers       []string

	titles   map[string]string // source -> its title for this episode
	anyTitle string            // AniDB title in any language: last resort
	listed   spot              // /tvdb: season and in-season number on the episode list
	// listIndex: /anidb: the episode's number in Anime-Lists' mapping
	// (sources.AniDBEpisode.ListIndex); 0 = Index.
	listIndex int
}

// mapIndex is the number the mapping knows the episode by.
func (e *Ep) mapIndex() int { return cmp.Or(e.listIndex, e.Index) }

// Seasons returns the season numbers, ascending (0 = Specials first).
func (s *Show) Seasons() []int {
	out := make([]int, 0, len(s.Episodes))
	for n := range s.Episodes {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func (s *Show) Episode(season, index int) *Ep {
	for i := range s.Episodes[season] {
		if s.Episodes[season][i].Index == index {
			return &s.Episodes[season][i]
		}
	}
	if s.Placeholders && (season == 0 || season == 1) && index > 0 {
		return &Ep{Season: season, Index: index}
	}
	return nil
}

// --- Mode "anidb": one AniDB entry, absolute numbering, no seasons ---------

// buildAniDB: season 1 holds every regular episode (1..n, absolute); season 0
// holds specials, openings (101+), endings (151+), trailers (201+)...
// An AniDB movie is a one-episode entry, so it becomes S01E01.
func (p *Provider) buildAniDB(ctx context.Context, aid int) (*Show, error) {
	a, err := p.anidb.Anime(ctx, aid)
	titles := p.titles.Of(aid)
	if a != nil {
		titles = a.Titles
	}
	if len(titles) == 0 {
		return nil, fmt.Errorf("anidb %d unknown: %v", aid, err)
	}
	s := &Show{Root: fmt.Sprintf("anidb-%d", aid), Episodes: map[int][]Ep{}, Complete: a != nil}
	s.Title, s.OriginalTitle = p.showTitle(titles), p.originalTitle(titles)
	entry := p.lists.ByAID(aid)
	if entry.TVDB.ID > 0 && (a == nil || !a.IsMovie()) {
		s.Guids = append(s.Guids, fmt.Sprintf("tvdb://%d", entry.TVDB.ID))
	}
	if a != nil && a.IsMovie() && entry.TMDBMovie > 0 {
		s.Guids = append(s.Guids, fmt.Sprintf("tmdb://%d", entry.TMDBMovie))
	} else if entry.TMDB.ID > 0 {
		s.Guids = append(s.Guids, fmt.Sprintf("tmdb://%d", entry.TMDB.ID))
	}
	if strings.HasPrefix(entry.IMDB, "tt") {
		s.Guids = append(s.Guids, "imdb://"+entry.IMDB)
	}

	if a == nil {
		slog.Info("AniDB details not available; using titles, other sources and placeholder episodes, retried in 2 min",
			"aid", aid, "why", err)
		s.Episodes[1] = nil
		s.Placeholders = true
	} else {
		p.fillFromAniDB(s, a)
		for _, e := range a.Episodes {
			ep := Ep{Season: e.Season, Index: e.Index, Summary: e.Summary, Date: e.AirDate, Minutes: e.Minutes,
				anyTitle: anidb.PickTitle(e.Titles, p.cfg.EpisodeLangs, false), listIndex: e.ListIndex()}
			ep.setTitle("anidb", anidb.FindTitle(e.Titles, p.cfg.EpisodeLangs, false))
			s.Episodes[e.Season] = append(s.Episodes[e.Season], ep)
		}
		if len(s.Episodes[1]) == 0 {
			s.Episodes[1] = nil
		}
	}

	tmdbID, movie := entry.TMDB.ID, false
	if a != nil && a.IsMovie() && entry.TMDBMovie > 0 {
		tmdbID, movie = entry.TMDBMovie, true
	}
	var wg sync.WaitGroup
	episodesOK := true
	if a != nil && !a.IsMovie() {
		wg.Go(func() { episodesOK = p.fillEpisodes(ctx, s, entry) })
	}
	sourcesOK := p.newSources(ctx, aid, a, entry.TVDB.ID, tmdbID, movie, p.lists.StartsSeries(aid)).fill(s)
	wg.Wait()
	if !episodesOK || !sourcesOK {
		s.Complete = false // thumbnails, titles, art or details missing: rebuild soon
	}
	p.pickEpisodeTitles(s)
	if s.OriginalTitle == s.Title {
		s.OriginalTitle = ""
	}
	if a != nil && a.IsMovie() && len(s.Episodes[1]) == 1 { // the movie itself: show its own title and summary
		s.Episodes[1][0].Title, s.Episodes[1][0].Summary = s.Title, s.Summary
	}
	return s, nil
}

// showTitle picks the AniDB show title per TITLE_LANGS / TITLE_OFFICIAL_ONLY.
func (p *Provider) showTitle(titles []anidb.Title) string {
	return anidb.PickTitle(titles, p.cfg.TitleLangs, p.cfg.TitleOfficialOnly)
}

// originalTitle picks the AniDB original title per ORIGINAL_TITLE_LANGS
// (official titles only; "" when none of the languages has one).
func (p *Provider) originalTitle(titles []anidb.Title) string {
	return anidb.FindTitle(titles, p.cfg.OriginalTitleLangs, true)
}

// fillFromAniDB sets the fields only AniDB has (everything else is picked
// per setting in fields.go).
func (p *Provider) fillFromAniDB(s *Show, a *sources.AniDBAnime) {
	total, n := 0, 0
	for _, e := range a.Episodes {
		if e.Season == 1 && e.Minutes > 0 {
			total, n = total+e.Minutes, n+1
		}
	}
	if n > 0 {
		s.Minutes = total / n
	}
}

// --- Mode "tvdb": TVDB seasons, absolute episode numbers -----------------

// buildTVDB: seasons follow TheTVDB, but every regular episode keeps its
// absolute number: if season 1 ends at 25, season 2 starts at 26
// (Season 02/Show - S02E26.mkv). Specials stay in season 0 with TVDB numbers.
// Show texts come from AniDB (first entry of the series) when known.
func (p *Provider) buildTVDB(ctx context.Context, tvdbID int) (*Show, error) {
	s := &Show{Root: fmt.Sprintf("tvdb-%d", tvdbID), Episodes: map[int][]Ep{}, Complete: true,
		Guids: []string{fmt.Sprintf("tvdb://%d", tvdbID)}}
	aid := p.lists.FirstAID(tvdbID)
	entry := p.lists.ByAID(aid)
	// The TMDB show: from the mapping, else looked up on TMDB while the
	// TVDB episode list downloads.
	tmdbShow := sync.OnceValues(func() (int, error) {
		if entry.TMDB.ID == 0 && p.tmdb.Enabled() {
			return p.tmdb.TVFromTVDB(ctx, tvdbID)
		}
		return entry.TMDB.ID, nil
	})
	go tmdbShow()

	var (
		wg      sync.WaitGroup
		a       *sources.AniDBAnime
		eps     []sources.Episode
		epsErr  error
		listSrc string // source of the episode list
	)
	if aid > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if a, err = p.anidb.Anime(ctx, aid); a == nil {
				slog.Info("AniDB details not available; using titles and other sources, retried in 2 min", "aid", aid, "why", err)
			}
		}()
	}
	for _, src := range p.cfg.EpisodeSources { // first source with an episode list wins
		switch {
		case src == "tvdb" && p.tvdb.Enabled():
			eps, epsErr = p.tvdb.Episodes(ctx, tvdbID)
		case src == "tmdb":
			if id, _ := tmdbShow(); id > 0 {
				eps, epsErr = p.tmdb.Episodes(ctx, id)
			}
		}
		if len(eps) > 0 {
			listSrc = src
			break
		}
	}
	wg.Wait()
	tmdbID, findErr := tmdbShow()
	if findErr != nil && !fetch.IsNotFound(findErr) {
		s.Complete = false // the TMDB lookup failed: rebuild soon
		slog.Info("TMDB show for the TVDB id not available; rebuilt in 2 min", "root", s.Root, "err", findErr)
	}
	if errors.Is(epsErr, sources.ErrIncomplete) {
		s.Complete = false // part of the episode list is missing: rebuild soon
		slog.Info("episode list incomplete; using what came back, rebuilt in 2 min", "root", s.Root, "err", epsErr)
	}
	if len(eps) == 0 {
		return nil, fmt.Errorf("tvdb %d: no episode list from EPISODE_SOURCES %v (set TVDB_API_KEY, TVDB_LEGACY_API_KEY or TMDB_API_KEY): %v",
			tvdbID, p.cfg.EpisodeSources, epsErr)
	}
	if tmdbID > 0 {
		s.Guids = append(s.Guids, fmt.Sprintf("tmdb://%d", tmdbID))
	}
	if strings.HasPrefix(entry.IMDB, "tt") {
		s.Guids = append(s.Guids, "imdb://"+entry.IMDB)
	}

	sources.FillAbsolute(eps)
	for _, e := range eps {
		ep := Ep{Season: e.Season, Index: e.Absolute, Summary: e.Summary, Date: e.Aired,
			Thumb: e.Thumb, Minutes: e.Minutes, listed: spot{e.Season, e.Number}}
		if e.Season == 0 {
			ep.Index = e.Number
		}
		ep.setTitle(listSrc, e.Title)
		s.Episodes[ep.Season] = append(s.Episodes[ep.Season], ep)
	}
	for n := range s.Episodes {
		sort.Slice(s.Episodes[n], func(i, j int) bool { return s.Episodes[n][i].Index < s.Episodes[n][j].Index })
	}

	if a != nil {
		s.OriginalTitle = p.originalTitle(a.Titles)
		p.fillFromAniDB(s, a)
	} else if len(p.titles.Of(aid)) > 0 {
		s.Complete = false
	}
	// Episode titles and the show-level fields touch different parts of s:
	// fetch both at once.
	var titlesReady, crewReady bool
	wg.Go(func() {
		var pr tvdbPairing
		pr, titlesReady = p.tvdbEpisodeTitles(ctx, s, tvdbID, tmdbID, listSrc, a)
		p.pickEpisodeTitles(s)
		crewReady = p.tvdbEpisodeCrew(ctx, s, tvdbID, tmdbID, listSrc, eps, pr)
	})
	sourcesOK := p.newSources(ctx, aid, a, tvdbID, tmdbID, false, true).fill(s)
	wg.Wait()
	if !titlesReady || !crewReady || !sourcesOK {
		s.Complete = false // other AniDB entries or episode crew still being fetched, or a source failed: rebuild soon
	}
	s.Title = firstOf(s.Title, fmt.Sprintf("TVDB %d", tvdbID))
	if s.OriginalTitle == s.Title {
		s.OriginalTitle = ""
	}
	if s.Date == "" {
		for _, e := range eps {
			if e.Season > 0 && e.Aired != "" {
				s.Date = e.Aired
				break
			}
		}
	}
	return s, nil
}

func firstOf(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// fullDate: AniDB dates may be "2004" or "2004-05"; Plex wants YYYY-MM-DD.
func fullDate(d string) string {
	switch len(d) {
	case 4:
		return d + "-01-01"
	case 7:
		return d + "-01"
	}
	return d
}
