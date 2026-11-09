package provider

import (
	"cmp"
	"log/slog"
	"regexp"
	"slices"
	"strings"
)

// genericTitle: placeholder titles that don't count as a title, so the next
// source in EPISODE_TITLES gets a chance ("Episode 12", "TBA").
var genericTitle = regexp.MustCompile(`(?i)^\s*(?:(?:episode|ep\.?)\s*\d+|tba|tbd|tbc)\s*$`)

// pickEpisodeTitles sets every episode's title per EPISODE_TITLES and logs
// (debug) how many titles each source gave.
func (p *Provider) pickEpisodeTitles(s *Show) {
	used := map[string]int{}
	s.eachEpisode(func(ep *Ep) {
		var src string
		ep.Title, src = episodeTitle(ep, p.cfg.EpisodeTitles)
		ep.titleSrc = src
		used[cmp.Or(src, "-")]++
	})
	attrs := []any{"root", s.Root, "order", p.cfg.EpisodeTitles}
	for _, src := range append(slices.Clone(p.cfg.EpisodeTitles), "-") {
		attrs = append(attrs, src, used[src])
	}
	slog.Debug("episode titles: source used", attrs...)
}

// episodeTitle: the first real title in source order; else AniDB's title in
// any language (when anidb is listed); else a placeholder title from a
// listed source; else AniDB's title even when anidb isn't listed, since no
// listed source has any (openings and endings are only on AniDB); else ""
// (shown as "Episode N"). EPISODE_TITLES=none always gives "".
func episodeTitle(ep *Ep, order []string) (title, src string) {
	for _, src := range order {
		if t := ep.titles[src]; t != "" && !genericTitle.MatchString(t) {
			return t, src
		}
	}
	if slices.Contains(order, "anidb") && ep.anyTitle != "" && !genericTitle.MatchString(ep.anyTitle) {
		return ep.anyTitle, "anidb"
	}
	for _, src := range order {
		if t := ep.titles[src]; t != "" {
			return t, src
		}
	}
	if len(order) > 0 && ep.anyTitle != "" { // no listed source has any title (openings, endings…: only AniDB lists them)
		return ep.anyTitle, "anidb"
	}
	return "", ""
}

// setPair notes where the episode is on another source, for the match report.
func (e *Ep) setPair(src, where string) {
	if e.pairs == nil {
		e.pairs = map[string]string{}
	}
	e.pairs[src] = where
}

func (e *Ep) setTitle(src, title string) {
	if title = strings.TrimSpace(title); title == "" {
		return
	}
	if e.titles == nil {
		e.titles = map[string]string{}
	}
	e.titles[src] = title
}
