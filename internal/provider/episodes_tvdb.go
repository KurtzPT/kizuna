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
// its episode list (TheTVDB's, or TheMovieDb's when the list came from
// there): AniDB's titles, and the place of the same AniDB episode on the
// other site, which often splits seasons differently (One Piece: TVDB S2E1
// is AniDB episode 9, TMDB S2E1 is episode 62).
type tvdbPairing struct {
	titles  map[spot]anidbTitles
	other   map[spot]otherPlace
	loading []int // AniDB entries not cached yet, fetched in the background
}

// otherPlace is an episode's place on the other site. on is false when the
// mapping puts its AniDB entry on another show there: it pairs with nothing.
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

// otherAt: an episode's place on the other site, through AniDB. An episode
// whose AniDB entry is on another show there has none. Without an AniDB
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
	list, other    string              // the episode list's site (always "tvdb") and the other one ("tmdb")
	first          *sources.AniDBAnime // the series' first AniDB entry (nil = not available)
	eps            []sources.Episode   // the episode list, absolute numbers filled in
}

func newTVDBBuild(tvdbID, tmdbID int, first *sources.AniDBAnime, eps []sources.Episode) *tvdbBuild {
	return &tvdbBuild{tvdbID: tvdbID, tmdbID: tmdbID, list: "tvdb", other: "tmdb", first: first, eps: eps}
}

// id is the show's id on a site.
func (b *tvdbBuild) id(site string) int {
	if site == "tmdb" {
		return b.tmdbID
	}
	return b.tvdbID
}

// fetchable reports whether a site's episode list can be had: TheTVDB has
// every /tvdb show, TheMovieDb only one it was found on.
func (b *tvdbBuild) fetchable(p *Provider, site string) bool {
	if site == "tmdb" {
		return p.tmdb.Enabled() && b.tmdbID > 0
	}
	return p.tvdb.Enabled()
}

// onShow reports whether an entry has a place on the show on a site. Every
// entry walked is on the TVDB series; an entry of another TMDB show has no
// place on this one. So with the list from TheMovieDb, such an entry has no
// episodes on the list, and the other site (TheTVDB) always has it.
func (b *tvdbBuild) onShow(site string, e mapping.Entry) bool {
	return site != "tmdb" || e.TMDB.ID == 0 || e.TMDB.ID == b.tmdbID
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
		if a == nil || !b.onShow(b.list, e) {
			continue
		}
		es, otherOK := p.spotsOf(e), b.onShow(b.other, e)
		for _, ae := range a.Episodes {
			sp, ok := es.at(b.list, ae.Season, ae.ListIndex())
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
			} else if o, ok := es.at(b.other, ae.Season, ae.ListIndex()); ok {
				pr.other[sp] = otherPlace{o, true}
			}
		}
	}
	return pr, ready
}

// tvdbEpisodeTitles adds every title EPISODE_TITLES may pick to a /tvdb show:
// the other of TVDB/TMDB (the episode list's own source is already set),
// paired through AniDB (tvdbPairing), and AniDB's, through every AniDB entry
// of the series. Returns the pairing for tvdbEpisodeCrew, and false while
// some AniDB entries are still missing, or when the other list failed.
func (p *Provider) tvdbEpisodeTitles(ctx context.Context, s *Show, b *tvdbBuild) (tvdbPairing, bool) {
	order := p.cfg.EpisodeTitles
	otherOn := b.fetchable(p, b.other)
	rec := report.RecorderFrom(ctx)
	var pr tvdbPairing
	ready := true
	if otherOn || slices.Contains(order, "anidb") { // the other site's crew needs the pairing too
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
	if otherOn && slices.Contains(order, b.other) {
		x, ok := p.seriesEpisodes(ctx, b.other, b.id(b.other))
		ready = ready && ok
		other := pairedList{x, pr.otherAt}
		s.eachEpisode(func(ep *Ep) {
			if e, ok := other.of(ep); ok {
				ep.setTitle(b.other, e.Title)
				if rec != nil {
					ep.setPair(b.other, spotName(e))
				}
			}
		})
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
// its episode list and the other of TVDB/TMDB, paired through AniDB like the
// titles (pr). Returns false when the other list failed or episode crew is
// still being fetched.
func (p *Provider) tvdbEpisodeCrew(ctx context.Context, s *Show, b *tvdbBuild, pr tvdbPairing) bool {
	ready := true
	paired := func(site string) pairedList {
		if site == b.list {
			return pairedList{indexEpisodes(b.eps), func(ep *Ep) (spot, bool) { return ep.listed, true }}
		}
		var x episodeIndex
		if b.fetchable(p, site) {
			var ok bool
			x, ok = p.seriesEpisodes(ctx, site, b.id(site))
			ready = ready && ok
		}
		return pairedList{x, pr.otherAt}
	}
	tv := paired("tvdb")
	tm := paired("tmdb")
	crewOK := p.episodeCrew(ctx, s, tv, tm)
	return ready && crewOK
}
