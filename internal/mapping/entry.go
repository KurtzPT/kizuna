package mapping

// Entry is everything known about one AniDB id.
type Entry struct {
	AID int
	// TVDB: the entry's place on its TVDB series (ID 0 when unknown or not a
	// series: "movie", "OVA"...). Always Mapped.
	TVDB Placement
	// TMDB: the entry's place on its TheMovieDb TV show, whose seasons can
	// differ from TheTVDB's. Mapped only when the list gives a TMDB season.
	TMDB      Placement
	TMDBMovie int
	IMDB      string
	MAL       int
	AniList   int
	Kitsu     int
	ANN       int // Anime News Network

	// noTVDB: Anime-Lists says this entry has no TVDB series on purpose
	// (tvdbid="movie", "OVA"...), so a fallback list must not add one.
	noTVDB bool
}

// SpecialOnly reports whether all of the entry's episodes are among its TVDB
// series' specials: the list puts it in season 0 (defaulttvdbseason="0"),
// and no exception moves an episode into a regular season or the absolute
// order. Movies, OVAs and specials that Anime-Lists files this way (One
// Piece Film Red is S00E46 of One Piece) are their own show in every mode,
// not part of the series. An exception saying an episode has no TVDB
// counterpart ("2-0") doesn't count.
func (e Entry) SpecialOnly() bool { return e.TVDB.specialsOnly() }

// specialsOnly: every episode of the placement is in the series' specials
// (see SpecialOnly).
func (p Placement) specialsOnly() bool {
	if p.ID <= 0 || !p.Mapped || p.Season != 0 {
		return false
	}
	for _, m := range p.Episodes {
		if m.Season != 0 && m.Ep > 0 {
			return false
		}
	}
	for _, r := range p.Ranges {
		if r.Season != 0 {
			return false
		}
	}
	return true
}

// Placement is where an entry's episodes sit on one site's series: TheTVDB
// (defaulttvdbseason, episodeoffset, <mapping tvdbseason="…">) or TheMovieDb
// (tmdbseason, tmdboffset, <mapping tmdbseason="…">).
type Placement struct {
	ID     int // the series or show; 0 = none
	Season int // the default season; -1 = "a" (absolute). Only when Mapped
	Offset int // episode offset
	// Mapped: the list gives the season, so Season and Offset place the
	// episodes no exception lists. Without it only the exceptions are known
	// (the provider pairs TheMovieDb like TheTVDB then). TheTVDB placements
	// are always mapped.
	Mapped bool
	// Episodes: Anime-Lists' per-episode exceptions (<mapping-list>), which
	// win over Season/Offset. Only Anime-Lists has them.
	Episodes []EpisodeMap
	// Ranges: <mapping>s with no end ("start=1156 offset=-1155": that
	// episode and every later one) or no start and end at all (the offset
	// applies to the whole AniDB season). Checked after Episodes.
	Ranges []EpisodeRange
}

// On returns the entry's placement on a site: "tvdb" or "tmdb" (TheMovieDb's
// TV show). Zero for any other site.
func (e Entry) On(site string) Placement {
	switch site {
	case "tvdb":
		return e.TVDB
	case "tmdb":
		return e.TMDB
	}
	return Placement{}
}

// EpisodeMap places one AniDB episode on TheTVDB or TheMovieDb: AniDB season
// 1 (regular) or 0 (S specials) episode AniDBEp is Season (-1 = absolute
// order) episode Ep there. Ep 0 = the episode has no counterpart.
type EpisodeMap struct {
	AniDBSeason, AniDBEp, Season, Ep int
}

// EpisodeRange: AniDB episodes From..To (inclusive) of AniDBSeason are
// episode n + Offset of Season.
type EpisodeRange struct {
	AniDBSeason, From, To, Season, Offset int
}

func (r EpisodeRange) covers(anidbSeason, ep int) bool {
	return r.AniDBSeason == anidbSeason && ep >= r.From && ep <= r.To
}

// Episode returns where an AniDB episode of the entry sits on this series:
// season (-1 = the site's absolute order) and episode number. ep is in
// Anime-Lists' numbering (credits C1 = 101, trailers T1 = 201, other O1 =
// 401: see sources.AniDBEpisode.ListIndex). The entry's own exceptions win
// (single episodes, then open ranges); otherwise, when Mapped, a regular
// episode N is episode N + Offset of Season, and special SN is special N:
// Anime-Lists only lists episodes whose numbers don't match ("not necessary
// if episode numbers match up exactly within the season(s)"). Openings,
// endings, trailers and the like (index 100 and up) have no numbers of their
// own on TheTVDB or TheMovieDb, so they're only placed by an exception. ok =
// false when it has no position (no series, an unlisted credit or trailer, or
// an exception saying "none").
func (p Placement) Episode(anidbSeason, ep int) (season, number int, ok bool) {
	if s, n, listed := p.exception(anidbSeason, ep); listed {
		return s, n, n > 0
	}
	return p.defaultEpisode(anidbSeason, ep)
}

// HasException reports whether the placement lists this AniDB episode itself.
func (p Placement) HasException(anidbSeason, ep int) bool {
	_, _, listed := p.exception(anidbSeason, ep)
	return listed
}

// Starts reports whether the placement starts its series: season 1 (or
// absolute) with no offset.
func (p Placement) Starts() bool {
	return p.Mapped && (p.Season == 1 || p.Season == -1) && p.Offset == 0
}

// exception finds an AniDB episode in the exceptions: single episodes first,
// then open ranges.
func (p Placement) exception(anidbSeason, ep int) (season, number int, listed bool) {
	for _, m := range p.Episodes {
		if m.AniDBSeason == anidbSeason && m.AniDBEp == ep {
			return m.Season, m.Ep, true
		}
	}
	for _, r := range p.Ranges {
		if r.covers(anidbSeason, ep) {
			return r.Season, ep + r.Offset, true
		}
	}
	return 0, 0, false
}

// defaultEpisode places an episode no exception lists: regular N is N +
// offset of the default season, special SN is special N.
func (p Placement) defaultEpisode(anidbSeason, ep int) (season, number int, ok bool) {
	switch {
	case p.ID <= 0 || !p.Mapped:
		return 0, 0, false
	case anidbSeason == 0:
		return 0, ep, ep >= 1 && ep < 100
	case anidbSeason != 1 || ep+p.Offset <= 0:
		return 0, 0, false
	}
	return p.Season, ep + p.Offset, true
}
