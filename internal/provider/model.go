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
	"kizuna/internal/mapping"
	"kizuna/internal/sources"
)

// The build errors for a show that doesn't exist (buildShow sorts by them).
var (
	errUnknownAID = errors.New("unknown")                               // neither the title list nor AniDB knows the aid
	errNoEpisodes = errors.New("TheTVDB has no episodes for this show") // TheTVDB answered without episodes
)

// Show is the source-independent model of one Plex show. Both modes build
// it; plex JSON is produced from it in items.go.
type Show struct {
	Root          string // rating key of the show: "anidb-123" or "tvdb-456"
	Title         string
	OriginalTitle string
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
	// time running out): the model is cached briefly and rebuilt. Without
	// ANIDB_CLIENT, AniDB details are nothing to wait for.
	Complete bool
	// Placeholders: the episode list is unknown (an /anidb show built without
	// AniDB details), so any S01/S00 episode Plex asks for is a bare
	// "Episode N" and the files still match. The next refresh with AniDB
	// details replaces them.
	Placeholders bool

	build string // match report build that made it (report.Recorder.ID)
}

// Ep is one episode at the season/index Plex's scanner gives the file.
type Ep struct {
	Season, Index int
	Title         string // picked per EPISODE_TITLES (episode_titles.go)
	Summary       string
	Date          string
	Thumb         string
	Minutes       int
	Directors     []string // TheTVDB's, else TheMovieDb's (episodeCrew)
	Writers       []string

	titles   map[string]string // source -> its title for this episode
	titleSrc string            // source of Title, for the match report
	pairs    map[string]string // source -> the episode there ("S01E05", "15207 · 3"), for the match report
	anyTitle string            // AniDB title in any language: last resort
	listed   spot              // /tvdb: season and in-season number on the episode list
	tvdbEp   int               // the TheTVDB episode it is paired with (0 = none): its crew record's id
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

func (s *Show) eachEpisode(f func(*Ep)) {
	for n := range s.Episodes {
		for i := range s.Episodes[n] {
			f(&s.Episodes[n][i])
		}
	}
}

// --- Mode "anidb": one AniDB entry, absolute numbering, no seasons ---------

// buildAniDB: season 1 holds every regular episode (1..n, absolute); season 0
// holds specials, openings (101+), endings (151+), trailers (201+)...
// An AniDB movie is a one-episode entry, so it becomes S01E01.
func (p *Provider) buildAniDB(ctx context.Context, aid int) (*Show, error) {
	a, err := p.anidb.Anime(ctx, aid)
	titles := p.showTitles(aid, a)
	if len(titles) == 0 {
		return nil, fmt.Errorf("anidb %d %w: %v", aid, errUnknownAID, err)
	}
	// Without ANIDB_CLIENT there are no details to wait for: the show is
	// complete as it is (as in buildTVDB), not rebuilt every 2 minutes.
	s := &Show{Root: fmt.Sprintf("anidb-%d", aid), Episodes: map[int][]Ep{}, Complete: a != nil || !p.anidb.Enabled()}
	s.Title, s.OriginalTitle = p.showTitle(titles), p.originalTitle(titles)
	entry := p.lists.ByAID(aid)
	tmdbID, movie := entry.TMDB.ID, false
	// Without AniDB details, an entry with only a TMDB movie id is a movie.
	isMovie := a != nil && a.IsMovie() || a == nil && entry.TMDB.ID == 0 && entry.TMDBMovie > 0
	if isMovie && entry.TMDBMovie > 0 {
		tmdbID, movie = entry.TMDBMovie, true
	}
	if entry.TVDB.ID > 0 && !isMovie { // the same GUIDs with and without AniDB details
		s.Guids = append(s.Guids, fmt.Sprintf("tvdb://%d", entry.TVDB.ID))
	}
	s.addGuids(tmdbID, entry.IMDB)

	if a == nil {
		if p.anidb.Enabled() {
			slog.Info("AniDB details not available; using titles, other sources and placeholder episodes, retried in 2 min",
				"aid", aid, "why", err)
		} else {
			slog.Debug("AniDB not configured; using titles, other sources and placeholder episodes", "aid", aid)
		}
		s.Episodes[1] = nil
		s.Placeholders = true
	} else {
		averageRuntime(s, a)
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

	var wg sync.WaitGroup
	episodesOK := true
	if a != nil && !a.IsMovie() {
		wg.Go(func() { episodesOK = p.fillEpisodes(ctx, s, entry) })
	}
	sourcesOK := p.newSources(ctx, aid, a, entry, entry.TVDB.ID, tmdbID, movie, p.lists.StartsSeries(aid)).fill(s)
	wg.Wait()
	if !episodesOK || !sourcesOK {
		s.Complete = false // thumbnails, titles, art or details missing: rebuild soon
	}
	p.pickEpisodeTitles(s)
	if s.OriginalTitle == s.Title {
		s.OriginalTitle = ""
	}
	if isMovie && a == nil { // a placeholder movie: its one episode is still the movie
		s.Episodes[1] = []Ep{{Season: 1, Index: 1}}
	}
	if isMovie && len(s.Episodes[1]) == 1 { // the movie itself: show its own title and summary
		s.Episodes[1][0].Title, s.Episodes[1][0].Summary = s.Title, s.Summary
	}
	return s, nil
}

// showTitles: an entry's AniDB titles, from its details, else (a nil) from
// the title dump.
func (p *Provider) showTitles(aid int, a *sources.AniDBAnime) []anidb.Title {
	if a != nil {
		return a.Titles
	}
	return p.titles.Of(aid)
}

// addGuids adds the TheMovieDb (tmdbID 0 = none) and IMDb GUIDs.
func (s *Show) addGuids(tmdbID int, imdb string) {
	if tmdbID > 0 {
		s.Guids = append(s.Guids, fmt.Sprintf("tmdb://%d", tmdbID))
	}
	if strings.HasPrefix(imdb, "tt") {
		s.Guids = append(s.Guids, "imdb://"+imdb)
	}
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

// averageRuntime sets the show's runtime: the average of AniDB's regular
// episodes (everything else is picked per setting in fields.go).
func averageRuntime(s *Show, a *sources.AniDBAnime) {
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

// listRuntime: the average runtime of an episode list's regular episodes
// (0 = none has one), for a /tvdb show without AniDB's.
func listRuntime(eps []sources.Episode) int {
	total, n := 0, 0
	for _, e := range eps {
		if e.Season > 0 && e.Minutes > 0 {
			total, n = total+e.Minutes, n+1
		}
	}
	if n == 0 {
		return 0
	}
	return total / n
}

// --- Mode "tvdb": TVDB seasons, absolute episode numbers -----------------

// buildTVDB: seasons follow TheTVDB, but every regular episode keeps its
// absolute number: if season 1 ends at 25, season 2 starts at 26
// (Season 02/Show - S02E26.mkv). Specials stay in season 0 with TVDB numbers.
// Show texts come from AniDB (first entry of the series) when known. The
// episode list always comes from TheTVDB: the mapping places AniDB entries
// on TheTVDB's seasons, and TheMovieDb often splits them differently.
func (p *Provider) buildTVDB(ctx context.Context, tvdbID int) (*Show, error) {
	if !p.tvdb.Enabled() {
		return nil, fmt.Errorf("tvdb %d: TVDB rules need TheTVDB: set TVDB_API_KEY or TVDB_LEGACY_API_KEY", tvdbID)
	}
	s := &Show{Root: fmt.Sprintf("tvdb-%d", tvdbID), Episodes: map[int][]Ep{}, Complete: true,
		Guids: []string{fmt.Sprintf("tvdb://%d", tvdbID)}}
	aid := p.lists.FirstAID(tvdbID)
	entry := p.lists.ByAID(aid)
	tmdbShow := p.tmdbShowOf(ctx, tvdbID, entry)
	go tmdbShow() // looked up while the episode list downloads

	var wg sync.WaitGroup
	var a *sources.AniDBAnime
	if aid > 0 {
		wg.Go(func() {
			var err error
			if a, err = p.anidb.Anime(ctx, aid); a == nil && p.anidb.Enabled() {
				slog.Info("AniDB details not available; using titles and other sources, retried in 2 min", "aid", aid, "why", err)
			}
		})
	}
	eps, epsErr := p.tvdb.Episodes(ctx, tvdbID)
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
		if epsErr == nil || fetch.IsNotFound(epsErr) {
			return nil, fmt.Errorf("tvdb %d: %w; use /anidb for it (or /auto)", tvdbID, errNoEpisodes)
		}
		return nil, fmt.Errorf("tvdb %d: no episode list from TheTVDB: %w", tvdbID, epsErr)
	}
	s.addGuids(tmdbID, entry.IMDB)

	sources.FillAbsolute(eps)
	b := newTVDBBuild(tvdbID, tmdbID, aid, a, eps)
	episodesFromList(s, b)
	s.OriginalTitle = p.originalTitle(p.showTitles(aid, a)) // without details, from the title dump
	if a != nil {
		averageRuntime(s, a)
	} else if p.anidb.Enabled() && aid > 0 {
		// AniDB details missing: rebuild soon (without ANIDB_CLIENT there are
		// none to wait for). Also for an entry the title dump doesn't list
		// yet: a new one, which AniDB has before the next dump.
		s.Complete = false
	}
	if s.Minutes == 0 { // no AniDB details, or no runtimes there
		s.Minutes = listRuntime(b.eps)
	}
	if !p.enrichTVDB(ctx, s, b, entry) {
		s.Complete = false // other AniDB entries or episode crew still being fetched, or a source failed: rebuild soon
	}
	finishTVDB(s, b)
	return s, nil
}

// tmdbShowOf returns, once, a /tvdb show's TMDB show: from the mapping, else
// looked up on TMDB.
func (p *Provider) tmdbShowOf(ctx context.Context, tvdbID int, entry mapping.Entry) func() (int, error) {
	return sync.OnceValues(func() (int, error) {
		if entry.TMDB.ID == 0 && p.tmdb.Enabled() {
			return p.tmdb.TVFromTVDB(ctx, tvdbID)
		}
		return entry.TMDB.ID, nil
	})
}

// episodesFromList adds the episode list's episodes to s, at their season
// and absolute number (specials at their number), with the list's title
// and crew (episodeCrew completes it).
func episodesFromList(s *Show, b *tvdbBuild) {
	for _, e := range b.eps {
		ep := Ep{Season: e.Season, Index: e.Absolute, Summary: e.Summary, Date: e.Aired,
			Thumb: e.Thumb, Minutes: e.Minutes, Directors: e.Directors, Writers: e.Writers,
			listed: spot{e.Season, e.Number}, tvdbEp: e.ID}
		if e.Season == 0 {
			ep.Index = e.Number
		}
		ep.setTitle("tvdb", e.Title)
		s.Episodes[ep.Season] = append(s.Episodes[ep.Season], ep)
	}
	for n := range s.Episodes {
		sort.Slice(s.Episodes[n], func(i, j int) bool { return s.Episodes[n][i].Index < s.Episodes[n][j].Index })
	}
}

// enrichTVDB adds the episode titles and crew and the show-level fields
// (entry: the mapping of b.aid, the series' first AniDB entry). They touch
// different parts of s, so both are fetched at once. TheMovieDb's episode
// list is fetched once, for the titles and the crew. Returns false while
// other AniDB entries or episode crew are still being fetched, or when a
// source failed.
func (p *Provider) enrichTVDB(ctx context.Context, s *Show, b *tvdbBuild, entry mapping.Entry) bool {
	var wg sync.WaitGroup
	var tmdbOK, titlesReady, crewReady bool
	wg.Go(func() {
		tmdbOK = b.loadTMDB(ctx, p)
		var pr tvdbPairing
		pr, titlesReady = p.tvdbEpisodeTitles(ctx, s, b)
		p.pickEpisodeTitles(s)
		crewReady = p.tvdbEpisodeCrew(ctx, s, b, pr)
	})
	sourcesOK := p.newSources(ctx, b.aid, b.first, entry, b.tvdbID, b.tmdbID, false, true).fill(s)
	wg.Wait()
	return tmdbOK && titlesReady && crewReady && sourcesOK
}

// finishTVDB fills what no source gave: a title, and the first regular
// episode's air date as the show's.
func finishTVDB(s *Show, b *tvdbBuild) {
	s.Title = firstOf(s.Title, fmt.Sprintf("TVDB %d", b.tvdbID))
	if s.OriginalTitle == s.Title {
		s.OriginalTitle = ""
	}
	if s.Date == "" {
		for _, e := range b.eps {
			if e.Season > 0 && e.Aired != "" {
				s.Date = e.Aired
				break
			}
		}
	}
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
