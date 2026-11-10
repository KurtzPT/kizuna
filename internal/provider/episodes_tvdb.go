package provider

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"kizuna/internal/mapping"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// tvdbPairing is what a /tvdb show's AniDB entries tell about each place on
// its episode list (TheTVDB's): AniDB's titles, and the place of the same
// AniDB episode on the other site, TheMovieDb, which often splits seasons
// differently (One Piece: TVDB S2E1 is AniDB episode 9, TMDB S2E1 is
// episode 62).
type tvdbPairing struct {
	titles  map[spot]anidbTitles
	other   map[spot]otherPlace
	loading []int // AniDB entries not cached yet, fetched in the background
}

// otherPlace is an episode's place on TheMovieDb. on is false when the
// mapping puts its AniDB entry on another show there, or a TMDB exception
// says the episode has no counterpart: it pairs with nothing.
type otherPlace struct {
	at spot
	on bool
}

type anidbTitles struct {
	title, any string
	aid        int    // the AniDB entry and episode it is, for the match report
	epNo       string // (formatted only when recording: see tvdbEpisodeTitles)
}

// atListed looks up an episode by its place on the list, else (regular
// episodes) by its absolute number, for entries mapped in absolute order.
func atListed[V any](m map[spot]V, ep *Ep) (V, bool) {
	v, ok := m[ep.listed]
	if !ok && ep.Season > 0 {
		v, ok = m[spot{-1, ep.Index}]
	}
	return v, ok
}

// otherAt: an episode's place on TheMovieDb, through AniDB. An episode
// whose AniDB entry is on another show there has none, nor has one a TMDB
// exception says has no counterpart. Without an AniDB counterpart (not
// cached yet, or a TheTVDB-only extra), or one the mapping doesn't place on
// TheMovieDb (no TMDB season or exception), a regular episode
// goes by its absolute number, which doesn't depend on how either site
// splits seasons (an absolute number the other site doesn't have pairs with
// nothing). A special has no absolute number: it keeps its number in season
// 0, as unlisted specials do everywhere else.
func (pr tvdbPairing) otherAt(ep *Ep) (spot, bool) {
	if o, ok := atListed(pr.other, ep); ok {
		return o.at, o.on
	}
	if ep.Season > 0 {
		return spot{-1, ep.Index}, true
	}
	return ep.listed, true
}

// tvdbBuild is what the steps of a /tvdb build share.
type tvdbBuild struct {
	tvdbID, tmdbID int                 // tmdbID 0 = no TheMovieDb show
	aid            int                 // the series' first AniDB entry (0 = none), looked up once per build
	first          *sources.AniDBAnime // its details (nil = not available)
	eps            []sources.Episode   // TheTVDB's episode list, absolute numbers filled in
	// tmdbEps: TheMovieDb's episode list, for the titles and the crew;
	// fetched by startTMDB (empty when the show isn't there).
	tmdbEps episodeIndex
	// startTMDB fetches TheMovieDb's episode list (into tmdbEps and tmdbOK,
	// done with tmdbWG), and start TheTVDB's crew records (into crew and
	// crewPending, done with crewWG).
	tmdbWG, crewWG sync.WaitGroup
	tmdbOK         bool
	crew           map[int]sources.Crew
	crewPending    bool
}

func newTVDBBuild(tvdbID, tmdbID, aid int, first *sources.AniDBAnime, eps []sources.Episode) *tvdbBuild {
	return &tvdbBuild{tvdbID: tvdbID, tmdbID: tmdbID, aid: aid, first: first, eps: eps}
}

// tmdbOn reports whether TheMovieDb's episode list can be had: only for a
// show found there.
func (b *tvdbBuild) tmdbOn(p *Provider) bool {
	return p.tmdb.Enabled() && b.tmdbID > 0
}

// startTMDB looks up TheMovieDb's show (tmdbShow: tmdbShowOf's) and
// fetches its episode list (loadTMDB), as soon as the build starts: it
// needs only the show's id, not TheTVDB's list, and both can take a while
// on a long show. It writes only tmdbEps and tmdbOK, read after tmdbWG.
func (b *tvdbBuild) startTMDB(ctx context.Context, p *Provider, tmdbShow func() (int, error)) {
	b.tmdbWG.Go(func() {
		if id, _ := tmdbShow(); id > 0 && p.tmdb.Enabled() { // tmdbOn, once the build has set tmdbID from the same lookup
			b.tmdbEps, b.tmdbOK = p.seriesEpisodes(ctx, "tmdb", id)
		}
	})
}

// start starts what the build needs whatever AniDB answers, once TheTVDB's
// list is in: TheTVDB's crew records for its episodes (tvdbEpisodeCrew).
// They are always used: wait waits for them.
func (b *tvdbBuild) start(ctx context.Context, p *Provider) {
	ids := make([]int, 0, len(b.eps))
	for _, e := range b.eps {
		if e.ID > 0 {
			ids = append(ids, e.ID)
		}
	}
	b.crewWG.Go(func() { b.crew, b.crewPending = p.tvdb.EpisodeCrew(ctx, ids) })
}

// wait waits for what startTMDB and start started.
func (b *tvdbBuild) wait() {
	b.tmdbWG.Wait()
	b.crewWG.Wait()
}

// loadTMDB waits for TheMovieDb's episode list in b.tmdbEps (startTMDB
// fetches it). Returns false when it failed or came back partial.
func (b *tvdbBuild) loadTMDB(p *Provider) bool {
	b.tmdbWG.Wait() // also when the show isn't there: tmdbEps is read after this
	if !b.tmdbOn(p) {
		return true
	}
	return b.tmdbOK
}

// onTMDBShow reports whether an entry has a place on the show on TheMovieDb.
// Every entry walked is on the TVDB series; an entry of another TMDB show
// has no place on this one.
func (b *tvdbBuild) onTMDBShow(e mapping.Entry) bool {
	return e.TMDB.ID == 0 || e.TMDB.ID == b.tmdbID
}

// tvdbPairing walks every AniDB entry of a /tvdb series. b.first (b.aid's)
// is already fetched: the first entry isn't looked up again, so a mapping
// reload during the build can't pair with another entry's details. Other entries come from the cache; missing ones are fetched in the
// background for the next build, and ready is false meanwhile (not for an
// entry in its 24 h block after an AniDB error: no build before then gets
// it). record notes those missing entries in pr.loading, for the match report.
func (p *Provider) tvdbPairing(b *tvdbBuild, record bool) (pr tvdbPairing, ready bool) {
	pr = tvdbPairing{titles: map[spot]anidbTitles{}, other: map[spot]otherPlace{}}
	ready = true
	for _, aid := range p.lists.AIDsForTVDB(b.tvdbID) {
		a := b.first
		if aid != b.aid {
			var fresh bool
			if a, fresh = p.anidb.Peek(aid); !fresh && p.anidb.Enabled() {
				p.anidb.Prefetch(aid)
				if _, blocked := p.anidb.BlockedUntil(aid); a == nil && !blocked {
					ready = false
					if record {
						pr.loading = append(pr.loading, aid)
					}
				}
			}
		}
		if a == nil {
			continue
		}
		e := p.lists.ByAID(aid)
		es, otherOK := p.spotsOf(e), b.onTMDBShow(e)
		for _, ae := range a.Episodes {
			sp, ok := es.at("tvdb", ae.Season, ae.ListIndex())
			if !ok {
				continue
			}
			if _, taken := pr.titles[sp]; !taken { // first entry wins
				title, anyTitle := p.anidbEpisodeTitle(ae.Titles)
				pr.titles[sp] = anidbTitles{title, anyTitle, aid, ae.EpNo}
			}
			if _, taken := pr.other[sp]; taken {
				continue
			}
			if !otherOK { // on another show there: no pairing, not the absolute-number fallback
				pr.other[sp] = otherPlace{}
				continue
			}
			if !es.tmdbPlaces(ae.Season, ae.ListIndex()) {
				// No TMDB season or exception (a TMDB id only Fribb has): TheTVDB's
				// spot says nothing about TheMovieDb's split, so otherAt pairs it
				// by absolute number, as before the entry was cached.
				continue
			}
			if o, ok := es.at("tmdb", ae.Season, ae.ListIndex()); ok {
				pr.other[sp] = otherPlace{o, true}
			} else if es.tmdb.HasException(ae.Season, ae.ListIndex()) { // the mapping says it has none there
				pr.other[sp] = otherPlace{}
			}
		}
	}
	return pr, ready
}

// tvdbEpisodeTitles adds every title EPISODE_TITLES may pick to a /tvdb show:
// TheMovieDb's (TheTVDB's, from the episode list, is already set), paired
// through AniDB (tvdbPairing), and AniDB's, through every AniDB entry of the
// series. TheMovieDb's list is b.tmdbEps (loadTMDB). Returns the pairing for
// tvdbEpisodeCrew, and false while some AniDB entries are still missing.
func (p *Provider) tvdbEpisodeTitles(ctx context.Context, s *Show, b *tvdbBuild) (tvdbPairing, bool) {
	order := p.cfg.EpisodeTitles
	tmdbOn := b.tmdbOn(p)
	rec := report.RecorderFrom(ctx)
	var pr tvdbPairing
	ready := true
	if tmdbOn || slices.Contains(order, "anidb") { // TheMovieDb's crew needs the pairing too
		pr, ready = p.tvdbPairing(b, rec != nil)
		if len(pr.loading) > 0 { // only when recording
			ids := make([]string, len(pr.loading))
			for i, aid := range pr.loading {
				ids[i] = strconv.Itoa(aid)
			}
			rec.Check(report.Check{Level: report.LevelWarn, Cause: true,
				Title: plural(len(pr.loading), "AniDB entry was", "AniDB entries were") + " still loading",
				Text: "AniDB " + joinShort(ids, 8) + " were not cached yet and are being fetched in the background (AniDB allows few requests), " +
					"so their episodes got titles from other sources for now. The next build uses them.", Section: report.SectionEpisodes})
		}
	}
	if tmdbOn && slices.Contains(order, "tmdb") {
		tm := pairedList{b.tmdbEps, pr.otherAt}
		s.eachEpisode(func(ep *Ep) { tm.title(ep, "tmdb", rec != nil) })
	}
	if slices.Contains(order, "anidb") {
		s.eachEpisode(func(ep *Ep) {
			if f, ok := atListed(pr.titles, ep); ok {
				ep.setTitle("anidb", f.title)
				ep.anyTitle = f.any
				if rec != nil {
					ep.setPair("anidb", fmt.Sprintf("%d · %s", f.aid, f.epNo))
				}
			}
		})
	}
	return pr, ready
}

// tvdbEpisodeCrew sets directors and writers on a /tvdb show's episodes from
// its episode list (TheTVDB's) and TheMovieDb's, paired through AniDB like
// the titles (pr). Returns false while episode crew is still being fetched.
func (p *Provider) tvdbEpisodeCrew(s *Show, b *tvdbBuild, pr tvdbPairing) bool {
	tm := pairedList{b.tmdbEps, pr.otherAt} // TheTVDB's list's crew is on the episodes already (episodesFromList)
	b.crewWG.Wait()
	applyCrew(s, b.crew, tm)
	return !b.crewPending
}
