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
// without the show or not set up). Plex's is found by the TheTVDB id: its
// episodes are in TheTVDB's order.
type episodeLists struct {
	tvdb, tmdb, plex func() (episodeIndex, bool) // plex nil: not to be read (plexEpisodesListed)
	// plexEarly: Plex's list is the main one (TheTVDB isn't set up), so it
	// starts with the others.
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
	l := episodeLists{tvdb: list("tvdb", entry.TVDB.ID, p.tvdb.Enabled()), tmdb: list("tmdb", entry.TMDB.ID, p.tmdb.Enabled())}
	if p.plexEpisodesListed(entry.TVDB.ID, true) { // the closure only when used: it allocates
		l.plex = list("plex", entry.TVDB.ID, true)
		l.plexEarly = !p.tvdb.Enabled()
	}
	return l
}

// start fetches the lists, in wg: Plex's only when it is the main one.
func (l episodeLists) start(wg *sync.WaitGroup) {
	wg.Go(func() { l.tvdb() })
	wg.Go(func() { l.tmdb() })
	if l.plexEarly {
		wg.Go(func() { l.plex() })
	}
}

// plexEpisodesListed: Plex's episode list may be read for a series (by its
// TheTVDB id): Plex is set up, and listed in EPISODE_TITLES or (fill, in
// /anidb) EPISODE_FILL_SOURCES.
func (p *Provider) plexEpisodesListed(tvdb int, fill bool) bool {
	return tvdb > 0 && p.plex.Enabled() && (slices.Contains(p.cfg.EpisodeTitles, "plex") ||
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
// real title, or (fill) no summary, picture, date or runtime.
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
// Plex) series (lists). TheTVDB and Plex give the thumbnail, which AniDB
// never has, plus the summary, air date and runtime where AniDB has none, in
// EPISODE_FILL_SOURCES' order. All give a title for EPISODE_TITLES, and
// directors and writers (AniDB has none per episode). Plex's list is read
// only when it can add something (plexListNeeded). Returns false when a list
// it needed failed or came back partial, or episode crew is still being
// fetched.
func (p *Provider) fillEpisodes(ctx context.Context, s *Show, entry mapping.Entry, lists episodeLists) bool {
	wantTVDB := p.tvdb.Enabled() && entry.TVDB.ID > 0
	wantTMDB := p.tmdb.Enabled() && entry.TMDB.ID > 0
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
			ep.Directors, ep.Writers, ep.tvdbEp = e.Directors, e.Writers, e.ID // episodeCrew completes them
			ep.src.directors, ep.src.writers = srcTVDB, srcTVDB
		}
	})
	// EPISODE_FILL_SOURCES in order: each fills what is still blank. Plex's
	// list is read, once, when its turn comes and it can add something.
	px := plexFill{get: lists.plex, tvdb: tvList, tvdbOK: wantTVDB && tvOK, record: record}
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
	crewOK := p.episodeCrew(ctx, s, tmList, px.list)
	return tvOK && tmOK && !px.failed && crewOK
}

// plexFill is Plex's episode list in fillEpisodes. A plain value, so with
// Plex off it costs nothing.
type plexFill struct {
	get    func() (episodeIndex, bool) // nil: not to be read (plexEpisodesListed)
	tvdb   pairedList                  // Plex's list is placed by its spots: its order is TheTVDB's
	tvdbOK bool                        // TheTVDB's list is there (plexListNeeded)
	record bool                        // pairs are for the match report only
	asked  bool
	list   pairedList // empty until read
	failed bool       // the read failed or came back partial
	paired int        // episodes it gave a title
}

// read reads the list the first time it is called, if it can add something
// to what the sources before it filled (plexListNeeded), and gives its
// titles. Later calls do nothing: the list is read at most once.
func (px *plexFill) read(p *Provider, s *Show) {
	if px.get == nil || px.asked {
		return
	}
	px.asked = true
	at := px.tvdb.at
	onTVDB := func(ep *Ep) bool {
		_, ok := at(ep)
		return ok
	}
	if !p.plexListNeeded(s, px.tvdbOK, true, onTVDB) {
		return
	}
	x, ok := px.get()
	px.list, px.failed = pairedList{x, at}, !ok
	eachAniDBEpisode(s, func(ep *Ep) {
		if _, ok := px.list.title(ep, "plex", px.record); ok {
			px.paired++
		}
	})
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
