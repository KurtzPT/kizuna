package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"kizuna/internal/config"
	"kizuna/internal/fetch"
	"kizuna/internal/report"
)

// Version is set at build time (-ldflags "-X kizuna/internal/provider.Version=1.2.3").
var Version = "dev"

// requestTimeout bounds one Plex request, the wait for the first list load
// included. A slow AniDB download that runs past it keeps going in the
// background and is used on the next refresh.
const requestTimeout = 25 * time.Second

// Handler returns the HTTP routes, behind the ALLOW_FROM / ALLOWED_HOSTS
// check (access.go):
//
//	GET  /health
//	GET  /{mode}                                      provider description
//	POST /{mode}/library/metadata/matches             match
//	GET  /{mode}/library/metadata/{key}               show / season / episode
//	GET  /{mode}/library/metadata/{key}/children      seasons or episodes
//	GET  /{mode}/library/metadata/{key}/grandchildren all episodes of a show
//	GET  /{mode}/library/metadata/{key}/images        artwork
func (p *Provider) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", p.health)
	mux.HandleFunc("GET /{mode}", p.withMode(p.describe))
	mux.HandleFunc("POST /{mode}/library/metadata/matches", p.withMode(p.waitLoaded(p.match)))
	mux.HandleFunc("GET /{mode}/library/metadata/{key}", p.withMode(p.waitLoaded(p.metadata)))
	mux.HandleFunc("GET /{mode}/library/metadata/{key}/{sub}", p.withMode(p.waitLoaded(p.metadata)))
	return p.guard(mux)
}

type modeHandler func(w http.ResponseWriter, r *http.Request, mode Mode)

func (p *Provider) withMode(h modeHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mode, ok := Modes[r.PathValue("mode")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		// A mode without its source answers 503 (Plex keeps what it has)
		// with the reason. Its description still answers, so Plex can add
		// the provider before the key is set.
		if why := p.modeOff(mode); why != "" && r.URL.Path != "/"+mode.Path {
			if p.offLogged.due(mode.Path) {
				slog.Warn("a request for a mode that is off; set its key or use another mode", "mode", "/"+mode.Path, "why", why)
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "/" + mode.Path + " is off: " + why})
			return
		}
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		debug := slog.Default().Enabled(r.Context(), slog.LevelDebug)
		var url string // only for the debug log and the match report
		if debug || p.reports != nil {
			url = fetch.Redact(r.URL.RequestURI())
		}
		var rq *report.Request // filled in by the handler, for the match report
		if p.reports != nil {
			rq = &report.Request{Time: start, Mode: mode.Path, Method: r.Method, URL: url}
			r = r.WithContext(report.WithRequest(r.Context(), rq))
		}
		h(rec, r, mode)
		took := time.Since(start)
		if debug {
			slog.Debug("plex request", "method", r.Method, "url", url, "status", rec.status,
				"bytes", rec.bytes, "took", took.Round(time.Millisecond))
		}
		if rq != nil {
			rq.Status, rq.Bytes, rq.TookMS = rec.status, rec.bytes, float64(took.Microseconds())/1000
			p.reports.AddRequest(*rq)
		}
	}
}

// modeOff says why a mode can't build shows ("" = it can), from the sources
// as set up: the same rule as the startup check (config.ModeOff).
func (p *Provider) modeOff(m Mode) string {
	return config.ModeOff(m.Path, p.anidb.Enabled(), p.tvdb.Enabled())
}

// everyTenMinutes reports a key as due at most once every 10 minutes, so a
// Plex library scan doesn't repeat one warning per show.
type everyTenMinutes struct {
	mu  sync.Mutex
	at  map[string]time.Time
	max int // past this many keys all are forgotten (0 = no limit): an internet scan's addresses can't fill memory
}

func (e *everyTenMinutes) due(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if t, ok := e.at[key]; ok && time.Since(t) < 10*time.Minute {
		return false
	}
	if e.at == nil || (e.max > 0 && len(e.at) >= e.max) {
		e.at = map[string]time.Time{}
	}
	e.at[key] = time.Now()
	return true
}

// recorder notes the status and size of an answer for the request log.
type recorder struct {
	http.ResponseWriter
	status, bytes int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// waitLoaded holds a request until the first list load is over: before it,
// nothing matches and shows are built without their ids, and Plex would keep
// what it got. A load that outlasts requestTimeout gets a 503 "starting",
// which Plex treats as a failed request, not as an answer; so does one
// waiting when Kizuna stops (Stop), at once. It also starts the request's
// requestTimeout: h gets what the wait left of it.
func (p *Provider) waitLoaded(h modeHandler) modeHandler {
	return func(w http.ResponseWriter, r *http.Request, mode Mode) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		select {
		case <-p.loaded: // loaded: served, even while stopping (the drain lets it finish)
			h(w, r.WithContext(ctx), mode)
			return
		default:
		}
		select {
		case <-p.loaded:
			h(w, r.WithContext(ctx), mode)
		case <-p.stopping:
			writeJSON(w, http.StatusServiceUnavailable, healthAnswer{Status: "stopping", Version: Version})
		case <-ctx.Done():
			writeJSON(w, http.StatusServiceUnavailable, healthAnswer{Status: "starting", Version: Version})
		}
	}
}

type healthAnswer struct {
	Status  string   `json:"status"` // ok, starting, degraded; stopping (waitLoaded only)
	Missing []string `json:"missing,omitempty"`
	Version string   `json:"version"`
}

// health answers 503 until matching can work: "starting" during the first
// list load and while the title list isn't loaded, "degraded" while the id
// mapping isn't (titles still match, but TVDB tags and /tvdb mode find
// nothing), so Docker marks the container unhealthy instead of hiding a
// half-working provider.
func (p *Provider) health(w http.ResponseWriter, _ *http.Request) {
	loading := true
	select {
	case <-p.loaded:
		loading = false
	default:
	}
	switch {
	case loading || !p.titles.Ready():
		writeJSON(w, http.StatusServiceUnavailable, healthAnswer{Status: "starting", Version: Version})
	case !p.lists.Ready():
		writeJSON(w, http.StatusServiceUnavailable, healthAnswer{Status: "degraded", Missing: []string{"mapping"}, Version: Version})
	default:
		writeJSON(w, http.StatusOK, healthAnswer{Status: "ok", Version: Version})
	}
}

func (p *Provider) describe(w http.ResponseWriter, _ *http.Request, mode Mode) {
	var d MediaProvider
	mp := &d.MediaProvider
	mp.Identifier, mp.Title, mp.Version = mode.Identifier(), mode.Title, Version
	for _, t := range []int{2, 3, 4} {
		mp.Types = append(mp.Types, Type{Type: t, Scheme: []Scheme{{mode.Identifier()}}})
	}
	mp.Feature = []Feature{{"metadata", metadataPath}, {"match", metadataPath + "/matches"}}
	writeJSON(w, http.StatusOK, d)
}

func (p *Provider) match(w http.ResponseWriter, r *http.Request, mode Mode) {
	req, err := readMatch(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	items, err := p.Match(r.Context(), mode, req)
	if err != nil && !noShow(err) {
		writeUnavailable(w, err)
		return
	}
	writeJSON(w, http.StatusOK, container(mode, items, 0, len(items)))
}

// timedOut: the show wasn't ready before the request's time ran out.
func timedOut(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// writeUnavailable answers a show that couldn't be built, but may exist,
// with a 503, like a request that outwaited the first list load: Plex keeps
// what it has (no "no match", no lost item) and asks again. A show that
// wasn't ready in time is still being built and is cached; for any other
// failure the answer names the reason (a source down, a key not set).
func writeUnavailable(w http.ResponseWriter, err error) {
	why := "the show wasn't ready in time; it is still being built, try again"
	if !timedOut(err) {
		why = "the show can't be built now, try again later: " + err.Error()
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": why})
}

// keyRe is a rating key: a show key (rootPattern), plus a season and an episode.
var keyRe = regexp.MustCompile(`^(` + rootPattern + `)(?:-s(\d+)(?:-e(\d+))?)?$`)

func (p *Provider) metadata(w http.ResponseWriter, r *http.Request, mode Mode) {
	m := keyRe.FindStringSubmatch(r.PathValue("key"))
	sub := r.PathValue("sub")
	if m == nil || (sub != "" && sub != "children" && sub != "grandchildren" && sub != "images") {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	if rq := report.RequestFrom(ctx); rq != nil {
		rq.Key = p.reportKey(m[1])
	}
	s, err := p.show(ctx, m[1])
	if err != nil {
		slog.Debug("metadata: show not available", "key", r.PathValue("key"), "err", err)
		if !noShow(err) {
			writeUnavailable(w, err)
			return
		}
		http.NotFound(w, r)
		return
	}
	withChildren := param(r, "includeChildren") == "1"

	var item *Item
	switch {
	case m[2] == "": // show
		switch sub {
		case "children":
			seasons := s.Seasons()
			writePaged(w, r, mode, len(seasons), func(i int) Item { return *p.seasonItem(mode, s, seasons[i], false) })
			return
		case "grandchildren":
			seasons, total := s.Seasons(), 0
			for _, n := range seasons {
				total += len(s.Episodes[n])
			}
			// writePaged asks for the page's items in order: walk the seasons
			// along with it (si: the season, before: episodes ahead of it).
			si, before := 0, 0
			writePaged(w, r, mode, total, func(i int) Item {
				for i-before >= len(s.Episodes[seasons[si]]) {
					before += len(s.Episodes[seasons[si]])
					si++
				}
				return p.episodeItem(mode, s, &s.Episodes[seasons[si]][i-before])
			})
			return
		}
		it := p.showItem(mode, s, withChildren)
		item = &it
	case m[3] == "": // season
		n, _ := strconv.Atoi(m[2])
		item = p.seasonItem(mode, s, n, withChildren && sub != "children")
		if item != nil && sub == "children" {
			eps := s.Episodes[n]
			writePaged(w, r, mode, len(eps), func(i int) Item { return p.episodeItem(mode, s, &eps[i]) })
			return
		}
	default: // episode
		n, _ := strconv.Atoi(m[2])
		i, _ := strconv.Atoi(m[3])
		if ep := s.Episode(n, i); ep != nil {
			it := p.episodeItem(mode, s, ep)
			item = &it
		}
	}
	if item == nil || sub == "grandchildren" || (sub == "children" && m[3] != "") {
		http.NotFound(w, r)
		return
	}
	if sub == "images" {
		c := container(mode, nil, 0, len(item.Image))
		c.MediaContainer.Image = item.Image
		c.MediaContainer.Size = len(item.Image)
		writeJSON(w, http.StatusOK, c)
		return
	}
	writeJSON(w, http.StatusOK, container(mode, []Item{*item}, 0, 1))
}

// --- helpers ---------------------------------------------------------------

func container(mode Mode, items []Item, offset, total int) Container {
	return Container{MediaContainer{Offset: offset, TotalSize: total, Size: len(items),
		Identifier: mode.Identifier(), Metadata: items}}
}

// writePaged applies X-Plex-Container-Start/-Size (header or query) to n
// items and builds only the ones on the page, in order (a long show's
// episodes are asked for 20 at a time). Without them the spec asks for the
// first 20.
func writePaged(w http.ResponseWriter, r *http.Request, mode Mode, n int, item func(i int) Item) {
	start, _ := strconv.Atoi(param(r, "X-Plex-Container-Start"))
	size, err := strconv.Atoi(param(r, "X-Plex-Container-Size"))
	if err != nil || size < 0 {
		size = 20
	}
	start = min(max(start, 0), n)
	end := start + min(size, n-start) // no start+size: a huge size would overflow
	items := make([]Item, 0, end-start)
	for i := start; i < end; i++ {
		items = append(items, item(i))
	}
	w.Header().Set("X-Plex-Container-Start", strconv.Itoa(start))
	w.Header().Set("X-Plex-Container-Total-Size", strconv.Itoa(n))
	writeJSON(w, http.StatusOK, container(mode, items, start, n))
}

// param reads a Plex parameter from the headers or the query string.
func param(r *http.Request, name string) string {
	if v := r.Header.Get(name); v != "" {
		return v
	}
	return r.URL.Query().Get(name)
}

// readMatch accepts JSON or form bodies, with numbers as numbers or strings,
// plus hints in the query string (Plex clients differ).
func readMatch(r *http.Request) (MatchRequest, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return MatchRequest{}, err
	}
	if slog.Default().Enabled(r.Context(), slog.LevelDebug) { // the body string only for the log
		slog.Debug("plex match body", "body", string(raw[:min(len(raw), 4096)]))
	}
	fields := map[string]string{}
	if body := strings.TrimSpace(string(raw)); strings.HasPrefix(body, "{") {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return MatchRequest{}, err
		}
		for k, v := range m {
			switch v := v.(type) {
			case string:
				fields[k] = v
			case float64:
				fields[k] = strconv.FormatFloat(v, 'f', -1, 64)
			case bool:
				fields[k] = map[bool]string{true: "1", false: "0"}[v]
			}
		}
	} else if form, err := url.ParseQuery(body); err == nil {
		for k := range form {
			fields[k] = form.Get(k)
		}
	}
	for k := range r.URL.Query() {
		if _, ok := fields[k]; !ok {
			fields[k] = r.URL.Query().Get(k)
		}
	}
	num := func(k string) *int {
		if n, err := strconv.Atoi(fields[k]); err == nil {
			return &n
		}
		return nil
	}
	req := MatchRequest{Title: fields["title"], ParentTitle: fields["parentTitle"], GrandparentTitle: fields["grandparentTitle"],
		GUID: fields["guid"], Filename: fields["filename"], Index: num("index"), ParentIndex: num("parentIndex")}
	req.Type = deref(num("type"), 2)
	req.Year = deref(num("year"), 0)
	flag := func(k string) int {
		if fields[k] == "1" || fields[k] == "true" {
			return 1
		}
		return 0
	}
	req.Manual, req.IncludeChildren = flag("manual"), flag("includeChildren")
	return req, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
