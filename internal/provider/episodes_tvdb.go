package provider

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"kizuna/internal/anidb"
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
// exception says has no counterpart. Without an AniDB
// counterpart (not cached yet, or a TheTVDB-only extra), a regular episode
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
	first          *sources.AniDBAnime // the series' first AniDB entry (nil = not available)
	eps            []sources.Episode   // TheTVDB's episode list, absolute numbers filled in
	// tmdbEps: TheMovieDb's episode list, for the titles and the crew;
	// fetched once by loadTMDB (empty when the show isn't there).
	tmdbEps episodeIndex
}

func newTVDBBuild(tvdbID, tmdbID int, first *sources.AniDBAnime, eps []sources.Episode) *tvdbBuild {
	return &tvdbBuild{tvdbID: tvdbID, tmdbID: tmdbID, first: first, eps: eps}
}

// tmdbOn reports whether TheMovieDb's episode list can be had: only for a
// show found there.
func (b *tvdbBuild) tmdbOn(p *Provider) bool {
	return p.tmdb.Enabled() && b.tmdbID > 0
}

// loadTMDB fetches TheMovieDb's episode list into b.tmdbEps. Returns false
// when it failed or came back partial.
func (b *tvdbBuild) loadTMDB(ctx context.Context, p *Provider) bool {
	if !b.tmdbOn(p) {
		return true
	}
	var ok bool
	b.tmdbEps, ok = p.seriesEpisodes(ctx, "tmdb", b.tmdbID)
	return ok
}

// onTMDBShow reports whether an entry has a place on the show on TheMovieDb.
// Every entry walked is on the TVDB series; an entry of another TMDB show
// has no place on this one.
func (b *tvdbBuild) onTMDBShow(e mapping.Entry) bool {
	return e.TMDB.ID == 0 || e.TMDB.ID == b.tmdbID
}

// tvdbPairing walks every AniDB entry of a /tvdb series. b.first is already
// fetched. Other entries come from the cache; missing ones are fetched in the
// background for the next build, and ready is false meanwhile. record notes
// those missing entries in pr.loading, for the match report.
func (p *Provider) tvdbPairing(b *tvdbBuild, record bool) (pr tvdbPairing, ready bool) {
	pr = tvdbPairing{titles: map[spot]anidbTitles{}, other: map[spot]otherPlace{}}
	ready = true
	firstAID := p.lists.FirstAID(b.tvdbID)
	for _, aid := range p.lists.AIDsForTVDB(b.tvdbID) {
		a := b.first
		if aid != firstAID {
			var fresh bool
			if a, fresh = p.anidb.Peek(aid); !fresh && p.anidb.Enabled() {
				p.anidb.Prefetch(aid)
				ready = ready && a != nil
				if a == nil && record {
					pr.loading = append(pr.loading, aid)
				}
			}
		}
		e := p.lists.ByAID(aid)
		if a == nil {
			continue
		}
		es, otherOK := p.spotsOf(e), b.onTMDBShow(e)
		for _, ae := range a.Episodes {
			sp, ok := es.at("tvdb", ae.Season, ae.ListIndex())
			if !ok {
				continue
			}
			if _, taken := pr.titles[sp]; !taken { // first entry wins
				pr.titles[sp] = anidbTitles{anidb.FindTitle(ae.Titles, p.cfg.EpisodeLangs, false), anidb.PickTitle(ae.Titles, p.cfg.EpisodeLangs, false),
					aid, ae.EpNo}
			}
			if _, taken := pr.other[sp]; taken {
				continue
			}
			if !otherOK { // on another show there: no pairing, not the absolute-number fallback
				pr.other[sp] = otherPlace{}
			} else if o, ok := es.at("tmdb", ae.Season, ae.ListIndex()); ok {
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
func (p *Provider) tvdbEpisodeCrew(ctx context.Context, s *Show, b *tvdbBuild, pr tvdbPairing) bool {
	tv := pairedList{indexEpisodes(b.eps), func(ep *Ep) (spot, bool) { return ep.listed, true }}
	tm := pairedList{b.tmdbEps, pr.otherAt}
	return p.episodeCrew(ctx, s, tv, tm)
}
