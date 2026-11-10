package provider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

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
	// SeasonArt: each Plex season's art (art.go), by season number.
	SeasonArt map[int]sources.Art
	Episodes  map[int][]Ep // season -> episodes sorted by index
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
	// anidbBlocked: an /anidb show built without AniDB details because the
	// anime is in its 24 h block after an AniDB error (zero = not): complete
	// all the same, as no build before then can get them.
	anidbBlocked time.Time
	// episodeGuids: each episode paired with a TheTVDB episode carries its
	// tvdb:// GUID (a special-only show, whose show has none of TheTVDB's).
	episodeGuids bool

	build string // match report build that made it (report.Recorder.ID)

	// images, seasonImages: the show's and each season's Plex image list,
	// made once by setImages when the build is done (items.go).
	images       []Image
	seasonImages map[int][]Image

	// artBy: which sources gave the art, for the match report (nil with the
	// reports off).
	artBy *artSources
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
	src       epSources // where the values above came from, for the match report
}

// srcCode names the source of an episode's value, for the match report. A
// byte: noting it allocates nothing, so it is noted with the reports off too.
type srcCode uint8

const (
	srcNone srcCode = iota
	srcAniDB
	srcTVDB
	srcTMDB
	srcShow // a movie's episode takes the show's
)

func (c srcCode) name() string {
	return [...]string{"", "anidb", "tvdb", "tmdb", "show"}[c]
}

// epSources is where an episode's values came from (its title's source is
// Ep.titleSrc). Each is set where the value is; one left on a value that
// ended up empty means nothing.
type epSources struct {
	summary, date, thumb, minutes, directors, writers srcCode
}

// mapIndex is the number the mapping knows the episode by.
func (e *Ep) mapIndex() int { return cmp.Or(e.listIndex, e.Index) }

// Seasons returns the season numbers, ascending (0 = Specials first).
func (s *Show) Seasons() []int {
	out := make([]int, 0, len(s.Episodes))
	for n := range s.Episodes {
		out = append(out, n)
	}
	slices.Sort(out)
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
	if p.unknownAID(aid) {
		return nil, fmt.Errorf("anidb %d %w: not in AniDB's title list", aid, errUnknownAID)
	}
	entry := p.lists.ByAID(aid)
	lists := p.episodeLists(ctx, entry)
	var early sync.WaitGroup
	defer early.Wait()
	if p.listsEarly(aid, entry) {
		lists.start(&early)
	}
	a, err := p.anidb.Anime(ctx, aid)
	titles := p.showTitles(aid, a)
	if len(titles) == 0 {
		return nil, fmt.Errorf("anidb %d %w: %v", aid, errUnknownAID, err)
	}
	// Without ANIDB_CLIENT, or while the anime is in its 24 h block after an
	// AniDB error, there are no details to wait for: the show is complete as
	// it is (as in buildTVDB), not rebuilt every 2 minutes.
	s := &Show{Root: fmt.Sprintf("anidb-%d", aid), Episodes: map[int][]Ep{}, Complete: a != nil || !p.anidb.Enabled()}
	if a == nil && p.anidb.Enabled() {
		if until, blocked := p.anidb.BlockedUntil(aid); blocked {
			s.Complete, s.anidbBlocked = true, until
		}
	}
	s.Title, s.OriginalTitle = p.showTitle(titles), p.originalTitle(titles)
	// movie: TheMovieDb's movie, by the mapping alone (mapping.Entry.Movie),
	// so the GUIDs and TheMovieDb id are the same with and without AniDB
	// details. isMovie: the show is one movie episode (AniDB's type too).
	tmdbID, movie := entry.TMDB.ID, entry.Movie()
	if movie {
		tmdbID = entry.TMDBMovie
	}
	isMovie := movie || a != nil && a.IsMovie()
	// A special-only entry isn't its TheTVDB series: its episodes carry
	// their TheTVDB specials' GUIDs instead (episodeItem).
	special := entry.SpecialOnly()
	if entry.TVDB.ID > 0 && !movie && !special {
		s.Guids = append(s.Guids, fmt.Sprintf("tvdb://%d", entry.TVDB.ID))
	}
	s.addGuids(tmdbID, entry.IMDB)
	s.episodeGuids = special

	if a == nil {
		switch {
		case !s.anidbBlocked.IsZero(): // the block itself is logged once, at Info (sources.AniDB)
			slog.Debug("AniDB details not available; using titles, other sources and placeholder episodes until the anime's 24 h block ends",
				"aid", aid, "blocked_until", s.anidbBlocked.Format(time.RFC3339), "why", err)
		case p.anidb.Enabled():
			slog.Info("AniDB details not available; using titles, other sources and placeholder episodes, retried in 2 min",
				"aid", aid, "why", err)
		default:
			slog.Debug("AniDB not configured; using titles, other sources and placeholder episodes", "aid", aid)
		}
		s.Episodes[1] = nil
		if special { // at their spots in TheTVDB's specials, filled from there (fillEpisodes)
			s.Episodes[1] = p.specialEpisodes(entry, isMovie)
		}
		s.Placeholders = true
	} else {
		averageRuntime(s, a)
		for _, e := range a.Episodes {
			title, anyTitle := p.anidbEpisodeTitle(e.Titles)
			ep := Ep{Season: e.Season, Index: e.Index, Summary: e.Summary, Date: e.AirDate, Minutes: e.Minutes,
				anyTitle: anyTitle, listIndex: e.ListIndex(), src: epSources{summary: srcAniDB, date: srcAniDB, minutes: srcAniDB}}
			ep.setTitle("anidb", title)
			s.Episodes[e.Season] = append(s.Episodes[e.Season], ep)
		}
		if len(s.Episodes[1]) == 0 {
			s.Episodes[1] = nil
		}
	}

	// Each site's series-level data describes the entry when it starts its
	// series there: TheTVDB's and TheMovieDb's seasons may split differently.
	ss := p.newSources(ctx, aid, entry, entry.TVDB.ID, tmdbID, movie,
		firstEntry{tvdb: p.lists.StartsShow("tvdb", aid), tmdb: movie || p.lists.StartsShow("tmdb", aid)})
	ss.setAniDB(ctx, a, entry)
	ss.anidbTargets(entry, plexSeasons(s)) // before fillEpisodes runs: it doesn't add seasons
	var wg sync.WaitGroup
	episodesOK := true
	if a != nil && !a.IsMovie() || special { // a special-only movie is paired too: it is a TheTVDB special
		wg.Go(func() { episodesOK = p.fillEpisodes(ctx, s, entry, lists) })
	}
	ss.fill(s)
	ss.fillSeasons(s)
	sourcesOK := ss.finish(s)
	wg.Wait()
	if !episodesOK || !sourcesOK {
		s.Complete = false // thumbnails, titles, art or details missing: rebuild soon
	}
	if special && a == nil {
		finishSpecial(s)
	}
	p.pickEpisodeTitles(s)
	if s.OriginalTitle == s.Title {
		s.OriginalTitle = ""
	}
	if isMovie && len(s.Episodes[1]) == 0 { // a placeholder movie: its one episode is still the movie
		s.Episodes[1] = []Ep{{Season: 1, Index: 1}}
	}
	if isMovie && len(s.Episodes[1]) == 1 { // the movie itself: show its own title and summary
		ep := &s.Episodes[1][0]
		if strings.TrimSpace(s.Summary) != "" { // firstOf takes the show's
			ep.src.summary = srcShow
		}
		ep.Title, ep.Summary = s.Title, firstOf(s.Summary, ep.Summary)
		ep.titleSrc = "show" // for the match report: the show's title, not the episode source picked above
		if special && a == nil {
			s.Summary = firstOf(s.Summary, ep.Summary) // what no source gave: TheTVDB's special's
		}
	}
	return s, nil
}

// unknownAID: neither the loaded title list nor AniDB's cache knows the aid,
// so AniDB isn't asked: a made-up id would only spend its daily limit. An
// entry newer than the last title dump waits for the next one (daily).
// Before the title list has loaded, AniDB is asked as before.
func (p *Provider) unknownAID(aid int) bool {
	if !p.anidb.Enabled() || !p.titles.Ready() || len(p.titles.Of(aid)) > 0 {
		return false
	}
	a, _ := p.anidb.Peek(aid)
	return a == nil
}

// maxSpecialEpisodes caps how many episodes specialEpisodes guesses.
const maxSpecialEpisodes = 12

// specialEpisodes: without AniDB details, a special-only entry's episodes
// (mapping.Entry.SpecialOnly), bare, at their AniDB numbers in season 1, for
// fillEpisodes to pair with TheTVDB's specials. AniDB's episode count is
// unknown: a movie has one, an OVA runs up to the spot of the series' next
// entry in the specials (one episode when there is none), plus any episode
// an exception places.
func (p *Provider) specialEpisodes(e mapping.Entry, movie bool) []Ep {
	n := 1
	if !movie {
		next := 0
		for _, aid := range p.lists.AIDsForTVDB(e.TVDB.ID) {
			o := p.lists.ByAID(aid).TVDB
			if aid != e.AID && o.Mapped && o.Season == 0 && o.Offset > e.TVDB.Offset && (next == 0 || o.Offset < next) {
				next = o.Offset
			}
		}
		if next > 0 {
			n = min(next-e.TVDB.Offset, maxSpecialEpisodes)
		}
		for _, m := range e.TVDB.Episodes {
			if m.AniDBSeason == 1 && m.Ep > 0 && m.AniDBEp <= maxSpecialEpisodes {
				n = max(n, m.AniDBEp)
			}
		}
	}
	eps := make([]Ep, n)
	for i := range eps {
		eps[i] = Ep{Season: 1, Index: i + 1}
	}
	return eps
}

// finishSpecial: a special-only show built without AniDB details keeps the
// episodes TheTVDB's specials had (the rest stay placeholders), and takes
// their runtime and first air date where no source gave the show's.
func finishSpecial(s *Show) {
	s.Episodes[1] = slices.DeleteFunc(s.Episodes[1], func(e Ep) bool { return e.tvdbEp == 0 })
	total, n := 0, 0
	for _, e := range s.Episodes[1] {
		if e.Minutes > 0 {
			total, n = total+e.Minutes, n+1
		}
		if s.Date == "" && e.Date != "" {
			s.Date = e.Date
		}
	}
	if s.Minutes == 0 && n > 0 {
		s.Minutes = total / n
	}
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
// buildShow calls it only with TheTVDB set up.
func (p *Provider) buildTVDB(ctx context.Context, tvdbID int) (*Show, error) {
	s := &Show{Root: fmt.Sprintf("tvdb-%d", tvdbID), Episodes: map[int][]Ep{}, Complete: true,
		Guids: []string{fmt.Sprintf("tvdb://%d", tvdbID)}}
	aid := p.lists.FirstAID(tvdbID)
	entry := p.lists.ByAID(aid)
	tmdbShow := p.tmdbShowOf(ctx, tvdbID, entry)
	b := newTVDBBuild(tvdbID, 0, aid, nil, nil)
	b.startTMDB(ctx, p, tmdbShow) // while TheTVDB's episode list downloads
	defer b.wait()                // a failed build too: nothing it started outlives it

	var wg sync.WaitGroup
	defer wg.Wait() // a failed build too: AniDB's answer is cached for the next
	var a *sources.AniDBAnime
	var blocked bool // a is nil because the anime is in its 24 h block after an AniDB error
	if aid > 0 {
		wg.Go(func() {
			var err error
			if a, err = p.anidb.Anime(ctx, aid); a == nil && p.anidb.Enabled() {
				var until time.Time
				if until, blocked = p.anidb.BlockedUntil(aid); blocked { // the block itself is logged once, at Info (sources.AniDB)
					slog.Debug("AniDB details not available; using titles and other sources until the anime's 24 h block ends",
						"aid", aid, "blocked_until", until.Format(time.RFC3339), "why", err)
				} else {
					slog.Info("AniDB details not available; using titles and other sources, retried in 2 min", "aid", aid, "why", err)
				}
			}
		})
	}
	eps, epsErr := p.tvdb.Episodes(ctx, tvdbID)
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
	b.tmdbID, b.eps = tmdbID, eps
	episodesFromList(s, b)
	// What doesn't depend on AniDB's answer starts before it is in, as that
	// can take seconds (its fair-use gap): TheTVDB's crew records, and
	// TheTVDB's and TheMovieDb's art (TheMovieDb's episode list is on its way).
	b.start(ctx, p)
	ss := p.newSources(ctx, aid, entry, tvdbID, tmdbID, false, firstEntry{true, true})
	ss.tvdbTargets(plexSeasons(s))
	ss.warmEarly()
	wg.Wait()
	b.first = a
	ss.setAniDB(ctx, a, entry)
	s.OriginalTitle = p.originalTitle(p.showTitles(aid, a)) // without details, from the title dump
	if a != nil {
		averageRuntime(s, a)
	} else if p.anidb.Enabled() && aid > 0 && !blocked {
		// AniDB details missing: rebuild soon (without ANIDB_CLIENT, or while
		// the anime is in its 24 h block after an AniDB error, there are none
		// to wait for). Also for an entry the title dump doesn't list yet: a
		// new one, which AniDB has before the next dump.
		s.Complete = false
	}
	if s.Minutes == 0 { // no AniDB details, or no runtimes there
		s.Minutes = listRuntime(b.eps)
	}
	if !p.enrichTVDB(ctx, s, b, ss) {
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
// and crew (tvdbEpisodeCrew completes it, with applyCrew).
func episodesFromList(s *Show, b *tvdbBuild) {
	for _, e := range b.eps {
		ep := Ep{Season: e.Season, Index: e.Absolute, Summary: e.Summary, Date: e.Aired,
			Thumb: e.Thumb, Minutes: e.Minutes, Directors: e.Directors, Writers: e.Writers,
			listed: spot{e.Season, e.Number}, tvdbEp: e.ID,
			src: epSources{summary: srcTVDB, date: srcTVDB, thumb: srcTVDB, minutes: srcTVDB, directors: srcTVDB, writers: srcTVDB}}
		if e.Season == 0 {
			ep.Index = e.Number
		}
		ep.setTitle("tvdb", e.Title)
		s.Episodes[ep.Season] = append(s.Episodes[ep.Season], ep)
	}
	for n := range s.Episodes {
		slices.SortStableFunc(s.Episodes[n], func(a, b Ep) int { return cmp.Compare(a.Index, b.Index) }) // sort.Slice allocates
	}
}

// enrichTVDB adds the episode titles and crew and the show-level fields (ss:
// the show's sources, its targets set). They touch different parts of s, so
// both are fetched at once. TheMovieDb's episode list is fetched once, for
// the titles and the crew. Returns false while other AniDB entries or
// episode crew are still being fetched, or when a source failed.
func (p *Provider) enrichTVDB(ctx context.Context, s *Show, b *tvdbBuild, ss *showSources) bool {
	var wg sync.WaitGroup
	var tmdbOK, titlesReady, crewReady bool
	var pairs map[int]int
	wg.Go(func() {
		tmdbOK = b.loadTMDB(p)
		var pr tvdbPairing
		pr, titlesReady = p.tvdbEpisodeTitles(ctx, s, b)
		pairs = tmdbSeasonPairs(s, b.tmdbEps, pr)
		p.pickEpisodeTitles(s)
		crewReady = p.tvdbEpisodeCrew(s, b, pr)
	})
	ss.fill(s) // the seasons' TheTVDB art is fetched with the show's
	wg.Wait()
	ss.pairTMDB(pairs, b.tmdbEps.posters)
	ss.fillSeasons(s)
	sourcesOK := ss.finish(s)
	return tmdbOK && titlesReady && crewReady && sourcesOK
}

// finishTVDB fills what no source gave: a title, and the first regular
// episode's air date as the show's.
func finishTVDB(s *Show, b *tvdbBuild) {
	if strings.TrimSpace(s.Title) == "" {
		s.Title = fmt.Sprintf("TVDB %d", b.tvdbID)
	}
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
