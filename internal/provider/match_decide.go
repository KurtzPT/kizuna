package provider

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"kizuna/internal/anidb"
	"kizuna/internal/config"
	"kizuna/internal/mapping"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

// peeker is the matcher's one read of AniDB's details: what is cached, never
// a download (*sources.AniDB).
type peeker interface {
	Peek(aid int) (a *sources.AniDBAnime, fresh bool)
}

// matcher decides which show a Plex match request names, from the mapping
// lists and the title index alone.
type matcher struct {
	lists           *mapping.Lists
	titles          *anidb.Titles
	minScore        int    // MIN_SCORE
	autoDefault     string // AUTO_DEFAULT
	anidbOn, tvdbOn bool   // which modes work (config.ModeOff)
	peek            peeker // entryYear's cached AniDB details; nil = none
}

// matcher is the provider's matcher as it is set up now. A plain value, made
// on every call: it costs no allocation.
func (p *Provider) matcher() matcher {
	return matcher{lists: p.lists, titles: p.titles, minScore: p.cfg.MinScore, autoDefault: p.cfg.AutoDefault,
		anidbOn: p.anidb.Enabled(), tvdbOn: p.tvdb.Enabled(), peek: p.anidb}
}

// modeOff says why a mode can't build shows ("" = it can), from the sources
// as set up: the same rule as the startup check (config.ModeOff).
func (m matcher) modeOff(mode Mode) string {
	return config.ModeOff(mode.Path, m.anidbOn, m.tvdbOn)
}

// fixMatchList: Plex's Fix Match for a show, which answers a list of
// candidates. A manual season or episode match is one answer, like an
// automatic one.
func (r MatchRequest) fixMatchList() bool { return r.Manual == 1 && r.Type <= 2 }

// showTitle is the title of the show the request is about: a season names
// it as its parent, an episode as its grandparent.
func (r MatchRequest) showTitle() string {
	switch r.Type {
	case 3:
		return r.ParentTitle
	case 4:
		return r.GrandparentTitle
	}
	return r.Title
}

// decision is what a match request came to: the candidates, and what the
// id and the title search said, from which how words the reason. The facts
// are kept in the report's own types, so the match report takes them as
// they are.
type decision struct {
	title string         // the show title the request named
	query string         // title without its id tags: what the searches look for
	cands []candidate    // ordered; empty = no match
	by    report.MatchBy // what the id and the title search said; its ByName stays nil
	// named is the entry picked by name inside a TheTVDB id (AID 0 = not
	// so). A value, not by.ByName's pointer: with the reports off a
	// pointer would be an allocation nobody reads.
	named report.ByName
	min   int                // MIN_SCORE, when the title search ran
	found []report.Candidate // the first title search results (only with note)
	warn  matchWarning       // at most one warning for the caller to log
}

type warningKind uint8

const (
	warnNone    warningKind = iota
	warnTags                // more than one id tag
	warnModeOff             // an id tag needs a mode that is off
	warnByName              // matched by name inside a TheTVDB id
)

// matchWarning is a warning the decision calls for. The matcher doesn't log
// it: the provider does, at most once per key every 10 minutes (warn).
type matchWarning struct {
	kind       warningKind
	title      string // the show title (warnTags, warnByName)
	id         string // the tags, the tag, or the tag or GUID that named the series
	path, need string // warnModeOff: the mode that is off and what to set
	aid, score int    // warnByName: the entry and its name score
	sure       string // warnByName: the id tags that name the entry exactly
}

// decide: an id tag in the show title or the file path (which includes the
// show folder) wins, then the GUID Plex already has, then title search. Only
// one id tag is allowed: with two different ones the show gets no match. In
// /auto, an AniDB or TVDB tag whose mode is off is no match.
// A manual show match (Fix Match) lists the id match, if any, first and then
// the title search results: a tag that gives no match there still leaves the
// typed title's results. A manual season or episode match isn't a list: an
// id match is the answer, and a tag that gives no match stays no match.
// note: keep the first title search results, for the match report.
// warn, if not nil, gets the decision's warning between the two steps,
// where it always went out: the title search's debug lines follow it.
func (m matcher) decide(mode Mode, r MatchRequest, note bool, warn func(matchWarning)) decision {
	d := m.byID(mode, r)
	if warn != nil {
		warn(d.warn)
	}
	return m.bySearch(mode, r, note, d)
}

// decide is the matcher's decision, with its warning logged.
func (p *Provider) decide(mode Mode, r MatchRequest, note bool) decision {
	return p.matcher().decide(mode, r, note, p.warn)
}

// warn logs a decision's warning, unless its key had one in the last 10
// minutes.
func (p *Provider) warn(w matchWarning) {
	switch w.kind {
	case warnTags:
		if p.tagsLogged.due(w.title) {
			slog.Warn("match: more than one id tag; only one is allowed, the show gets no match", "title", w.title, "tags", w.id)
		}
	case warnModeOff:
		if p.tagOffLogged.due(w.path) {
			slog.Warn("match: id tag needs /"+w.path+", which is off", "tag", w.id, "set", w.need)
		}
	case warnByName:
		if p.nameLogged.due(w.title) {
			slog.Warn("match: matched to an AniDB entry by name inside a TheTVDB id; may be wrong", "title", w.title, "id", w.id,
				"aid", w.aid, "score", w.score, "to_be_sure", "use "+w.sure)
		}
	}
}

// byID is decide's first step: the show an id tag or Plex's GUID names, or
// the tag rule that says the show gets no match (two tags, or a strict tag
// whose mode is off).
func (m matcher) byID(mode Mode, r MatchRequest) decision {
	d := decision{title: r.showTitle()}
	d.query = tagRe.ReplaceAllString(d.title, "")
	debug := debugOn(context.Background())
	switch tags := idTags(d.title, r.Filename); {
	case len(tags) > 1: // which one the user meant is a guess: no tag, GUID or title search
		names := make([]string, len(tags))
		for i, t := range tags {
			names[i] = t[0]
		}
		list := strings.Join(names, " ")
		d.by = report.MatchBy{ID: report.MatchManyTags, Text: list}
		d.warn = matchWarning{kind: warnTags, title: d.title, id: list}
		return d
	case len(tags) == 1:
		t := tags[0]
		rules, need := m.tagRules(mode, t[1])
		if need != "" { // the tag is the user's choice: no other mode, GUID or title search
			d.by = report.MatchBy{ID: report.MatchTagOff, Text: t[0], Off: rules.Path, Need: need}
			d.warn = matchWarning{kind: warnModeOff, id: t[0], path: rules.Path, need: need}
			return d
		}
		if strings.EqualFold(t[1], "tvdb") {
			if b, ok := m.specialRoot(t[2], d.query, r.Filename, r.Year); ok {
				return d.namedBy(report.MatchTag, t[0], b)
			}
		}
		if root := m.rootForID(rules, t[1], t[2], d.query, r.Year); root != "" {
			d.by = report.MatchBy{ID: report.MatchTag, Text: t[0]}
			d.cands = []candidate{{root, 100}}
			return d
		}
		if debug {
			slog.Debug("match: id tag not in the mapping", "tag", t[0])
		}
	}
	for _, re := range []*regexp.Regexp{guidRe, otherGUIDRe} {
		if g := re.FindStringSubmatch(r.GUID); g != nil {
			// Another agent's TheTVDB GUID names the series like a tag; Kizuna's
			// own (and HAMA's) GUID is an earlier match or a Fix Match: kept.
			if re == otherGUIDRe && (strings.EqualFold(g[1], "thetvdb") || strings.EqualFold(g[1], "tvdb")) {
				if b, ok := m.specialRoot(g[2], d.query, r.Filename, r.Year); ok {
					return d.namedBy(report.MatchGUID, r.GUID, b)
				}
			}
			if root := m.rootForID(m.rules(mode, g[1]), g[1], g[2], d.query, r.Year); root != "" {
				d.by = report.MatchBy{ID: report.MatchGUID, Text: r.GUID}
				d.cands = []candidate{{root, 100}}
				return d
			}
			if debug {
				slog.Debug("match: GUID id not in the mapping", "guid", r.GUID)
			}
		}
	}
	return d
}

// namedBy is d with the entry picked by name inside the TheTVDB id that src
// (a tag or GUID) names: the entry's show, and a warning that it may be
// wrong.
func (d decision) namedBy(id report.MatchID, src string, b byName) decision {
	d.by = report.MatchBy{ID: id, Text: src}
	d.named = report.ByName{AID: b.aid, Sure: b.sure}
	d.cands = []candidate{{fmt.Sprintf("anidb-%d", b.aid), b.score}}
	d.warn = matchWarning{kind: warnByName, title: d.title, id: src, aid: b.aid, score: b.score, sure: b.sure}
	return d
}

// bySearch is decide's second step, the AniDB title search: the best match
// when the id gave none, or for a Fix Match list up to 10 results (MIN_SCORE
// not applied) after the id match. An id match or a tag rule outside that
// list is final: no search. A manual season or episode match is one answer:
// MIN_SCORE holds, as for an automatic one.
func (m matcher) bySearch(mode Mode, r MatchRequest, note bool, d decision) decision {
	list := r.fixMatchList()
	if !list && (len(d.cands) > 0 || d.by.TagRule()) { // no search has run yet, so TagRule is the tag rule alone
		return d
	}
	debug := debugOn(context.Background())
	limit := 1
	if list {
		limit = 10
	}
	out := d.cands // the id match, if any, stays first
	seen := map[string]bool{}
	for _, c := range out {
		seen[c.root] = true
	}
	titleRules := m.rules(mode, "")
	results := m.titles.Search(d.query, r.Year, 20)
	if debug || note {
		// The best five, whichever of them the match below takes or turns
		// down: the log and the report show what the search found.
		for i, res := range results[:min(5, len(results))] {
			t := mainTitle(m.titles, res.AID)
			if debug {
				slog.Debug("match: title search result", "query", d.query, "year", r.Year, "rank", i+1, "aid", res.AID, "score", res.Score, "title", t)
			}
			if note {
				d.found = append(d.found, report.Candidate{AID: res.AID, Title: t, Score: res.Score})
			}
		}
	}
	for _, res := range results {
		if len(out) >= limit {
			break
		}
		if !list && res.Score < m.minScore {
			if debug {
				slog.Debug("match: below MIN_SCORE", "aid", res.AID, "score", res.Score, "min", m.minScore)
			}
			break
		}
		root := m.rootFor(titleRules, "anidb", res.AID) // never "" for an AniDB id
		if !seen[root] {
			seen[root] = true
			out = append(out, candidate{root, res.Score})
		}
	}
	d.cands, d.min = out, m.minScore
	switch {
	case len(results) == 0:
		d.by.Search = report.SearchNone
	case len(out) == 0:
		d.by.Search, d.by.Best = report.SearchLow, results[0].Score
	default:
		d.by.Search, d.by.Best = report.SearchFound, results[0].Score
	}
	return d
}

// how says in words what decided. The wording of a match's reason is made
// here and nowhere else: the log, the request's note and the match report
// all show it.
func (d decision) how() string {
	id, search := d.idHow(), d.searchHow()
	switch {
	case id == "":
		return search
	case search == "":
		return id
	}
	return id + "; " + search // a Fix Match list: both
}

// idHow is the id's part of how ("" for none).
func (d decision) idHow() string {
	var s string
	switch by := d.by; by.ID {
	case report.MatchManyTags:
		return "id tags " + by.Text + ": only one is allowed"
	case report.MatchTagOff:
		return fmt.Sprintf("tag %s: /%s is off, set %s", by.Text, by.Off, by.Need)
	case report.MatchTag:
		s = "tag " + by.Text
	case report.MatchGUID:
		s = "guid " + by.Text
	default:
		return ""
	}
	if d.named.AID > 0 {
		s += ", by name: " + d.named.Note()
	}
	return s
}

// searchHow is the title search's part of how ("" when it didn't run).
func (d decision) searchHow() string {
	switch d.by.Search {
	case report.SearchFound:
		return "title search"
	case report.SearchNone:
		return "title search: no similar title"
	case report.SearchLow:
		return fmt.Sprintf("title search: best score %d below MIN_SCORE %d", d.by.Best, d.min)
	}
	return ""
}
