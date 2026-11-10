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

// modeOff says why a mode can't build shows ("" = it can): Provider.modeOff's
// rule.
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
// id and the title search said, from which how words the reason.
type decision struct {
	title  string        // the show title the request named
	query  string        // title without its id tags: what the searches look for
	cands  []candidate   // ordered; empty = no match
	id     idOutcome     // what an id tag or GUID said
	search searchOutcome // what the title search said, if it ran
	found  []foundTitle  // the first title search results (only with note)
	warn   matchWarning  // at most one warning for the caller to log
}

type idKind uint8

const (
	idNone     idKind = iota // no id tag or GUID, or none the mapping knows
	idTag                    // an id tag named the show
	idGUID                   // the GUID Plex already has named the show
	idManyTags               // more than one id tag: no match
	idModeOff                // an /auto id tag whose mode is off: no match
)

// idOutcome is what an id tag or GUID said.
type idOutcome struct {
	kind idKind
	text string // the tag, the GUID, or for idManyTags every tag
	// idModeOff: the mode the tag needs and the setting that turns it on
	path, need string
	// an entry picked by name inside a TheTVDB id (aid 0 = not so): the
	// entry and the id tags that name it exactly
	aid  int
	sure string
}

// stops: a tag rule says the show gets no match.
func (o idOutcome) stops() bool { return o.kind == idManyTags || o.kind == idModeOff }

type searchKind uint8

const (
	searchNotRun   searchKind = iota
	searchFound               // at least one candidate
	searchNothing             // no similar title
	searchBelowMin            // the best result is below MIN_SCORE
)

// searchOutcome is what the title search said.
type searchOutcome struct {
	kind      searchKind
	best, min int // the best result's score (0 = no result) and MIN_SCORE
}

// foundTitle is one title search result, for the match report.
type foundTitle struct {
	aid, score int
	title      string
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
func (m matcher) decide(mode Mode, r MatchRequest, note bool) decision {
	return m.bySearch(mode, r, note, m.byID(mode, r))
}

// decide is the matcher's decision, with its warning logged. The warning
// goes out between the two steps, where it always was: the title search's
// debug lines follow it.
func (p *Provider) decide(mode Mode, r MatchRequest, note bool) decision {
	m := p.matcher()
	d := m.byID(mode, r)
	p.warn(d.warn)
	return m.bySearch(mode, r, note, d)
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
		d.id = idOutcome{kind: idManyTags, text: list}
		d.warn = matchWarning{kind: warnTags, title: d.title, id: list}
		return d
	case len(tags) == 1:
		t := tags[0]
		rules, need := m.tagRules(mode, t[1])
		if need != "" { // the tag is the user's choice: no other mode, GUID or title search
			d.id = idOutcome{kind: idModeOff, text: t[0], path: rules.Path, need: need}
			d.warn = matchWarning{kind: warnModeOff, id: t[0], path: rules.Path, need: need}
			return d
		}
		if strings.EqualFold(t[1], "tvdb") {
			if b, ok := m.specialRoot(t[2], d.query, r.Filename, r.Year); ok {
				return d.namedBy(idTag, t[0], b)
			}
		}
		if root := m.rootForID(rules, t[1], t[2], d.query, r.Year); root != "" {
			d.id = idOutcome{kind: idTag, text: t[0]}
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
					return d.namedBy(idGUID, r.GUID, b)
				}
			}
			if root := m.rootForID(m.rules(mode, g[1]), g[1], g[2], d.query, r.Year); root != "" {
				d.id = idOutcome{kind: idGUID, text: r.GUID}
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
func (d decision) namedBy(kind idKind, src string, b byName) decision {
	d.id = idOutcome{kind: kind, text: src, aid: b.aid, sure: b.sure}
	d.cands = []candidate{{fmt.Sprintf("anidb-%d", b.aid), b.score}}
	d.warn = matchWarning{kind: warnByName, title: d.title, id: src, aid: b.aid, score: b.score, sure: b.sure}
	return d
}

// bySearch is decide's second step, the AniDB title search: the best match
// when the id gave none, or for a manual match up to 10 results (MIN_SCORE
// not applied), in a Fix Match list after the id match. An id match or a tag
// rule outside that list is final: no search.
func (m matcher) bySearch(mode Mode, r MatchRequest, note bool, d decision) decision {
	if !r.fixMatchList() && (len(d.cands) > 0 || d.id.stops()) {
		return d
	}
	debug := debugOn(context.Background())
	manual := r.Manual == 1
	limit := 1
	if manual {
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
				d.found = append(d.found, foundTitle{aid: res.AID, score: res.Score, title: t})
			}
		}
	}
	for _, res := range results {
		if len(out) >= limit {
			break
		}
		if !manual && res.Score < m.minScore {
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
	d.cands = out
	d.search = searchOutcome{kind: searchFound, min: m.minScore}
	switch {
	case len(results) == 0:
		d.search.kind = searchNothing
	case len(out) == 0:
		d.search.kind, d.search.best = searchBelowMin, results[0].Score
	default:
		d.search.best = results[0].Score
	}
	return d
}

// how says in words what decided. The wording of a match's reason is made
// here and nowhere else: the log, the request's note and the match report
// all show it.
func (d decision) how() string {
	switch {
	case d.id.kind == idNone:
		return d.search.how()
	case d.search.kind == searchNotRun:
		return d.id.how()
	}
	return d.id.how() + "; " + d.search.how() // a Fix Match list: both
}

// how is the id's part of decision.how ("" for none).
func (o idOutcome) how() string {
	var s string
	switch o.kind {
	case idManyTags:
		return "id tags " + o.text + ": only one is allowed"
	case idModeOff:
		return fmt.Sprintf("tag %s: /%s is off, set %s", o.text, o.path, o.need)
	case idTag:
		s = "tag " + o.text
	case idGUID:
		s = "guid " + o.text
	default:
		return ""
	}
	if o.aid > 0 {
		s += ", by name: " + o.nameNote()
	}
	return s
}

// nameNote says which entry was picked by name and which tag makes it sure.
func (o idOutcome) nameNote() string {
	return report.ByName{AID: o.aid, Sure: o.sure}.Note()
}

// how is the title search's part of decision.how ("" when it didn't run).
func (o searchOutcome) how() string {
	switch o.kind {
	case searchFound:
		return "title search"
	case searchNothing:
		return "title search: no similar title"
	case searchBelowMin:
		return fmt.Sprintf("title search: best score %d below MIN_SCORE %d", o.best, o.min)
	}
	return ""
}
