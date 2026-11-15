package provider

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"kizuna/internal/mapping"
	"kizuna/internal/report"
)

// episodeLists: an /anidb show's TheTVDB, TheMovieDb and Plex episode
// lists, each fetched once, on first use (empty, without a fetch, for a site
// without the show, not set up, or that no episode setting lists:
// tvdbEpisodesListed, tmdbEpisodesListed). Plex's is found by the TheTVDB
// id: its episodes are in TheTVDB's order.
type episodeLists struct {
	tvdb, tmdb, plex func() (episodeIndex, bool) // plex nil: not to be read (plexEpisodesListed)
	// plexEarly: Plex's list is read anyway, so it starts with the others:
	// for episode crew (plexCrewRead), or as the main one for titles or fill
	// values (TheTVDB's isn't fetched).
	plexEarly bool
}

func (p *Provider) episodeLists(ctx context.Context, entry mapping.Entry) episodeLists {
	list := func(src string, id int, on bool) func() (episodeIndex, bool) {
		return sync.OnceValues(func() (episodeIndex, bool) {
			if !on || id <= 0 {
				return episodeIndex{}, true
			}
			return p.seriesEpisodes(ctx, src, id)
		})
	}
	tvdbOn := p.tvdbEpisodesListed(entry)
	l := episodeLists{tvdb: list("tvdb", entry.TVDB.ID, tvdbOn), tmdb: list("tmdb", entry.TMDB.ID, p.tmdbEpisodesListed(false))}
	if p.plexEpisodesListed(entry.TVDB.ID, true) { // the closure only when used: it allocates
		l.plex = list("plex", entry.TVDB.ID, true)
		l.plexEarly = p.plexCrewRead(entry.TVDB.ID) ||
			!tvdbOn && (slices.Contains(p.cfg.EpisodeTitles, "plex") || slices.Contains(p.cfg.EpisodeFillSources, "plex"))
	}
	return l
}

// tvdbEpisodesListed: an /anidb show's TheTVDB episode list is fetched:
// TheTVDB is set up, and an episode setting lists tvdb (EPISODE_TITLES,
// EPISODE_FILL_SOURCES, EPISODE_CREW_SOURCES), or the entry is special-only
// (its episodes are TheTVDB's specials). Otherwise nothing would be taken
// from it: Plex's list is then the main one, as without TheTVDB.
func (p *Provider) tvdbEpisodesListed(entry mapping.Entry) bool {
	cfg := p.cfg
	return p.tvdb.Enabled() && (entry.SpecialOnly() || slices.Contains(cfg.EpisodeTitles, "tvdb") ||
		slices.Contains(cfg.EpisodeFillSources, "tvdb") || slices.Contains(cfg.EpisodeCrewSources, "tvdb"))
}

// tmdbEpisodesListed: TheMovieDb's episode list is fetched: TheMovieDb is
// set up, and a setting asks the list for something: its titles
// (EPISODE_TITLES), its crew (EPISODE_CREW_SOURCES), or in /tvdb (tvdbMode)
// the season posters that come with it (POSTER_SOURCES, pairTMDB).
func (p *Provider) tmdbEpisodesListed(tvdbMode bool) bool {
	cfg := p.cfg
	return p.tmdb.Enabled() && (slices.Contains(cfg.EpisodeTitles, "tmdb") || slices.Contains(cfg.EpisodeCrewSources, "tmdb") ||
		tvdbMode && slices.Contains(cfg.PosterSources, "tmdb"))
}

// start fetches the lists, in wg: Plex's only when it is read anyway
// (plexEarly).
func (l episodeLists) start(wg *sync.WaitGroup) {
	wg.Go(func() { l.tvdb() })
	wg.Go(func() { l.tmdb() })
	if l.plexEarly {
		wg.Go(func() { l.plex() })
	}
}

// plexEpisodesListed: Plex's episode list may be read for a series (by its
// TheTVDB id): Plex is set up, and listed in EPISODE_TITLES or (fill, in
// /anidb) EPISODE_FILL_SOURCES, or read for episode crew (plexCrewRead).
func (p *Provider) plexEpisodesListed(tvdb int, fill bool) bool {
	return p.plexCrewRead(tvdb) || tvdb > 0 && p.plex.Enabled() && (slices.Contains(p.cfg.EpisodeTitles, "plex") ||
		fill && slices.Contains(p.cfg.EpisodeFillSources, "plex"))
}

// before: a and b are both in order, a first.
func before(order []string, a, b string) bool {
	i, j := slices.Index(order, a), slices.Index(order, b)
	return i >= 0 && j >= 0 && i < j
}

// plexListNeeded: Plex's episode list can add something to s, so it is
// read (whole, like the other sources' lists): plex comes before tvdb in
// EPISODE_TITLES or (fill) EPISODE_FILL_SOURCES; TheTVDB's list isn't there
// (tvdbOK false: not set up, or it failed); or an episode Plex has a place
// for (onList) still has a gap after every source listed before plex: no
// real title, or (fill) no summary, picture, date or runtime. Episode crew
// reads the list without a gap test (plexCrewRead), and takes nothing but
// crew from it.
func (p *Provider) plexListNeeded(s *Show, tvdbOK, fill bool, onList func(*Ep) bool) bool {
	titles, fills := p.cfg.EpisodeTitles, p.cfg.EpisodeFillSources
	if !fill {
		fills = nil
	}
	inTitles, inFill := slices.Contains(titles, "plex"), slices.Contains(fills, "plex")
	switch {
	case !inTitles && !inFill:
		return false
	case !tvdbOK || before(titles, "plex", "tvdb") || before(fills, "plex", "tvdb"):
		return true
	}
	gap := false
	s.eachEpisode(func(ep *Ep) {
		if gap || !onList(ep) {
			return
		}
		gap = inTitles && !titledBefore(ep, titles, "plex") ||
			inFill && (blank(ep.Summary) || blank(ep.Thumb) || blank(ep.Date) || ep.Minutes == 0)
	})
	return gap
}

// titledBefore: a source listed before src in order gave ep a real title.
func titledBefore(ep *Ep, order []string, src string) bool {
	for _, o := range order {
		if o == src {
			return false
		}
		if t := ep.titles[o]; t != "" && !genericTitle.MatchString(t) {
			return true
		}
	}
	return false
}

func blank(v string) bool { return strings.TrimSpace(v) == "" }

// listsEarly: an /anidb show's episode lists are worth fetching before
// AniDB answers, which can take seconds (its fair-use gap): AniDB may be
// asked, and the lists will likely be paired with its episodes (any entry
// but a movie known by its TMDB movie id alone; a special-only one is
// paired too, as TheTVDB's special). With AniDB's answer at hand (fresh in
// the cache, or the cached copy while AniDB can't be asked), its type
// decides: a movie isn't paired. With no answer to come (the anime's 24 h
// block, a backoff, the daily limit, a ban...: Anime fails at once), the
// show is built without details, and only a special-only one is paired
// (finishSpecial).
func (p *Provider) listsEarly(aid int, entry mapping.Entry) bool {
	if !p.anidb.Enabled() {
		return false // the answer is at hand: no details
	}
	a, fresh := p.anidb.Peek(aid)
	if a != nil && fresh {
		return !a.IsMovie() || entry.SpecialOnly()
	}
	if !p.anidb.Askable(aid) {
		if a != nil { // Anime gives this copy
			return !a.IsMovie() || entry.SpecialOnly()
		}
		return entry.SpecialOnly()
	}
	return entry.SpecialOnly() || entry.TMDB.ID > 0 || entry.TMDBMovie == 0
}

// fillEpisodes pairs an /anidb show's episodes with its TVDB (and TMDB, and
// Plex) series (lists; each only when a setting takes something from it).
// TheTVDB and Plex give the thumbnail, which AniDB
// never has, plus the summary, air date and runtime where AniDB has none, in
// EPISODE_FILL_SOURCES' order. All give a title for EPISODE_TITLES, and
// directors, writers and producers in EPISODE_CREW_SOURCES' order
// (episodeCrew, which adds their photos to photos; AniDB has none per
// episode). For titles and fill values, Plex's list is read only when it can
// add something (plexListNeeded); for crew, always (plexCrewRead). Returns
// false when a list it needed failed or came back partial, or episode crew
// is still being fetched.
func (p *Provider) fillEpisodes(ctx context.Context, s *Show, entry mapping.Entry, lists episodeLists, photos *crewPhotos) bool {
	wantTVDB := p.tvdbEpisodesListed(entry) && entry.TVDB.ID > 0
	wantTMDB := p.tmdbEpisodesListed(false) && entry.TMDB.ID > 0
	wantPlex := lists.plex != nil
	if !wantTVDB && !wantTMDB && !wantPlex {
		return true
	}
	var tv, tm episodeIndex
	tvOK, tmOK := true, true
	var wg sync.WaitGroup
	if wantTVDB {
		wg.Go(func() { tv, tvOK = lists.tvdb() })
	}
	if wantTMDB {
		wg.Go(func() { tm, tmOK = lists.tmdb() })
	}
	wg.Wait()

	es := p.spotsOf(entry)
	on := func(site string, x episodeIndex) pairedList {
		return pairedList{x, func(ep *Ep) (spot, bool) {
			if ep.Season != 0 && ep.Season != 1 {
				return spot{}, false
			}
			return es.at(site, ep.Season, ep.mapIndex())
		}}
	}
	tvList, tmList := on("tvdb", tv), on("tmdb", tm)
	record := report.RecorderFrom(ctx) != nil // pairs are for the match report only
	paired, pairedTMDB := 0, 0
	eachAniDBEpisode(s, func(ep *Ep) {
		if _, ok := tmList.title(ep, "tmdb", record); ok {
			pairedTMDB++
		}
		if e, ok := tvList.title(ep, "tvdb", record); ok {
			paired++
			ep.Directors, ep.Writers, ep.Producers, ep.tvdbEp = e.Directors, e.Writers, e.Producers, e.ID // TheTVDB's, for episodeCrew to place in the order
		}
	})
	// EPISODE_FILL_SOURCES in order: each fills what is still blank. Plex's
	// list is read, once, when its turn comes and it can add something.
	px := plexFill{get: lists.plex, tvdb: tvList, tvdbOK: wantTVDB && tvOK, record: record, crew: p.plexCrewRead(entry.TVDB.ID)}
	fillFrom := func(l pairedList, code srcCode) {
		eachAniDBEpisode(s, func(ep *Ep) {
			if e, ok := l.of(ep); ok {
				fillValue(&ep.Thumb, e.Thumb, &ep.src.thumb, code)
				fillValue(&ep.Summary, e.Summary, &ep.src.summary, code)
				fillValue(&ep.Date, e.Aired, &ep.src.date, code)
				if ep.Minutes == 0 && e.Minutes > 0 {
					ep.Minutes, ep.src.minutes = e.Minutes, code
				}
			}
		})
	}
	for _, src := range p.cfg.EpisodeFillSources {
		switch src {
		case "tvdb":
			fillFrom(tvList, srcTVDB)
		case "plex":
			px.read(p, s)
			fillFrom(px.list, srcPlex)
		}
	}
	px.read(p, s) // for its titles, when plex isn't in EPISODE_FILL_SOURCES
	if debugOn(ctx) {
		slog.Debug("episode pairing", "root", s.Root, "tvdb", entry.TVDB.ID, "season", entry.TVDB.Season, "offset", entry.TVDB.Offset,
			"exceptions", len(entry.TVDB.Episodes), "episodes", len(s.Episodes[1])+len(s.Episodes[0]), "paired_tvdb", paired,
			"tmdb", entry.TMDB.ID, "tmdb_mapped", entry.TMDB.Mapped, "paired_tmdb", pairedTMDB, "paired_plex", px.paired)
	}
	crewOK := p.episodeCrew(ctx, s, tmList, &px, photos)
	return tvOK && tmOK && !px.failed && crewOK
}

// plexFill is Plex's episode list in fillEpisodes. A plain value, so with
// Plex off it costs nothing.
type plexFill struct {
	get    func() (episodeIndex, bool) // nil: not to be read (plexEpisodesListed)
	tvdb   pairedList                  // Plex's list is placed by its spots: its order is TheTVDB's
	tvdbOK bool                        // TheTVDB's list is there (plexListNeeded)
	record bool                        // pairs are for the match report only
	crew   bool                        // read for episode crew, whatever the gaps (plexCrewRead)
	asked  bool                        // read decided, for titles and fill values
	got    bool                        // the list was read
	list   pairedList                  // empty until read
	failed bool                        // the read failed or came back partial
	paired int                         // episodes it gave a title
}

// read reads the list the first time it is called, if it can add something
// to what the sources before it filled (plexListNeeded), and gives its
// titles. Later calls do nothing: the decision is made once.
func (px *plexFill) read(p *Provider, s *Show) {
	if px.get == nil || px.asked {
		return
	}
	px.asked = true
	if !p.plexListNeeded(s, px.tvdbOK, true, px.onList) {
		return
	}
	px.fetch()
	eachAniDBEpisode(s, func(ep *Ep) {
		if _, ok := px.list.title(ep, "plex", px.record); ok {
			px.paired++
		}
	})
}

// fetch reads the list, once (the titles' read and the crew's share it).
// Read for crew, it gives no titles and no fill values: those were decided
// before (read).
func (px *plexFill) fetch() {
	if px.got {
		return
	}
	px.got = true
	x, ok := px.get()
	px.list, px.failed = pairedList{x, px.tvdb.at}, !ok
}

// onList: Plex's list has a place for ep (its order is TheTVDB's).
func (px *plexFill) onList(ep *Ep) bool {
	_, ok := px.tvdb.at(ep)
	return ok
}

// eachAniDBEpisode: an /anidb show's regular episodes, then its specials:
// the seasons pairing places.
func eachAniDBEpisode(s *Show, f func(*Ep)) {
	for _, season := range [...]int{1, 0} {
		for i := range s.Episodes[season] {
			f(&s.Episodes[season][i])
		}
	}
}

// fillValue gives a blank *v a source's value, and notes in src which source
// it was (code) when it took one: the episode's report says which source
// each value came from.
func fillValue(v *string, value string, src *srcCode, code srcCode) {
	if strings.TrimSpace(*v) != "" {
		return
	}
	if *v = firstOf(value); *v != "" {
		*src = code
	}
}
