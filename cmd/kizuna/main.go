// Command kizuna runs the Plex metadata provider.
//
//	kizuna         run the provider (settings: env vars and the settings file, see SETTINGS.md)
//	kizuna health  exit 0 when the running provider is ready (else 1, saying why on stderr)
//
// Any other argument prints the usage and exits 2; -h prints it and exits 0.
package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // TZ works in the distroless image and on Windows (no zoneinfo there): log days follow it

	"kizuna/internal/anidb"
	"kizuna/internal/config"
	"kizuna/internal/fetch"
	"kizuna/internal/logfile"
	"kizuna/internal/mapping"
	"kizuna/internal/plex"
	"kizuna/internal/provider"
	"kizuna/internal/report"
	"kizuna/internal/sources"
)

func main() {
	// Before anything else: a typo must not start the server, nor create
	// the data folder's logs.
	healthCheck, exit, stop := command(os.Args[1:], os.Stdout, os.Stderr)
	if stop {
		os.Exit(exit)
	}
	// The settings file goes into the environment first, so every setting
	// below can come from it. Its problems are logged once logging is set.
	envFile, envErr := config.LoadEnvFile()
	// TZ (maybe from the file) before the log file opens: its days follow it.
	tzErr := config.SetTimeZone()

	level, bad := slog.LevelInfo, ""
	switch v := strings.ToLower(strings.TrimSpace(config.LogLevel())); v {
	case "", "info":
	case "debug":
		level = slog.LevelDebug
	default:
		bad = v
	}

	// A bad LOG_RETENTION_DAYS is reported before the log file is open, so
	// that warning is held back and written once the real logger is set.
	out, fileErr, archiveErr := io.Writer(os.Stdout), error(nil), error(nil)
	var early bytes.Buffer
	var file *logfile.File
	if !healthCheck {
		slog.SetDefault(slog.New(slog.NewTextHandler(&early, nil)))
		f, err := logfile.Open(filepath.Join(config.DataDir(), "logs"), "kizuna", config.LogRetentionDays())
		if f != nil { // err alone: yesterday's file couldn't be dated, but logging works
			defer f.Close()
			file, out, archiveErr = f, io.MultiWriter(os.Stdout, f), err
		} else {
			fileErr = err
		}
	}
	handler := newLogHandler(out, level)
	if file != nil {
		handler = file.WarnVia(handler) // a later day the file can't be dated: a warning
	}
	slog.SetDefault(slog.New(handler))
	_, _ = out.Write(early.Bytes())
	if fileErr != nil {
		slog.Warn("can't write the log file; logging to standard output only (check the data folder's permissions)", "err", fileErr)
	}
	if archiveErr != nil {
		slog.Warn("log file: another program may have it open", "err", archiveErr)
	}
	if envErr != nil {
		slog.Error("can't start: " + envErr.Error())
		os.Exit(1)
	}
	if bad != "" {
		slog.Warn("unknown LOG_LEVEL; using info", "LOG_LEVEL", bad, "allowed", "info,debug")
	}
	if tzErr != nil {
		slog.Warn("ignoring TZ; log times use the default time zone", "err", tzErr)
	}

	cfg := config.Load()
	if !healthCheck {
		envFile.Log()
		// A malformed key stops the start before any request could carry it.
		err := cfg.CheckKeys()
		if err == nil {
			err = cfg.CheckModes()
		}
		if err != nil {
			slog.Error("can't start: " + err.Error())
			os.Exit(1)
		}
		cfg.LogModes()
	}
	fetch.UserAgent = "kizuna/" + provider.Version + " (personal Plex metadata provider)"
	if healthCheck {
		os.Exit(health(cfg))
	}
	if err := serve(cfg); err != nil {
		slog.Error("can't run: " + err.Error())
		os.Exit(1)
	}
}

// usage is what kizuna -h prints, and what a wrong argument gets.
const usage = `usage: kizuna [health]
  kizuna         run the provider (settings: see SETTINGS.md)
  kizuna health  exit 0 when the running provider is ready, else 1
`

// command reads kizuna's arguments: none runs the provider, "health" the
// Docker health check (the image has no curl). -h, -help, --help or help
// print the usage (exit 0). Anything else prints the usage to stderr with
// exit 2, the usual code for a wrong command line. stop says main should
// exit with that code. The argument isn't echoed: it could be anything
// pasted, a key included.
func command(args []string, stdout, stderr io.Writer) (healthCheck bool, exit int, stop bool) {
	switch {
	case len(args) == 0:
		return false, 0, false
	case len(args) > 1:
	case args[0] == "health":
		return true, 0, false
	case slices.Contains([]string{"-h", "-help", "--help", "help"}, args[0]):
		_, _ = io.WriteString(stdout, usage)
		return false, 0, true
	}
	_, _ = io.WriteString(stderr, "kizuna: unknown argument\n"+usage)
	return false, 2, true
}

// setup creates every source. keepLoaded loads the two lists matching
// depends on, calls firstDone after the first load, and keeps them fresh
// until ctx ends. It returns TheTVDB too, for the ready line.
func setup(ctx context.Context, cfg config.Config) (p *provider.Provider, tvdb *sources.TVDB, keepLoaded func(firstDone func(titles bool))) {
	f := fetch.New(filepath.Join(cfg.DataDir, "cache"))
	go f.KeepFree(ctx, cfg.CacheMinFree, cfg.CacheFreeTarget) //lint:allow goroutine: lives as long as the process: a panic should end it, not stop the cache cleanup unseen
	titles, lists := &anidb.Titles{}, &mapping.Lists{}
	ad := sources.NewAniDB(f, cfg.DataDir, cfg.AniDBClient, cfg.AniDBClientVer)
	tvdb = sources.NewTVDB(f, cfg.TVDBKey, cfg.TVDBPin, cfg.TVDBLegacyKey)
	p = provider.New(cfg, titles, ad, lists, provider.Sources{TVDB: tvdb,
		TMDB: sources.NewTMDB(f, cfg.TMDBKey), MAL: sources.NewMAL(f, cfg.MALClientID), ANN: sources.NewANN(f),
		Plex: sources.NewPlex(f, cfg.PlexToken)})
	load := func() {
		loadErr(ctx, "AniDB title list", titles.Load(ctx, f))
		loadErr(ctx, "anime mapping", lists.Load(ctx, f, cfg.DataDir))
	}
	keepLoaded = func(firstDone func(titles bool)) {
		keepFresh(ctx, load, func() (bool, []string) { return titles.Ready(), lists.Missing() }, firstDone)
	}
	return p, tvdb, keepLoaded
}

// loadErr logs a list that failed to load, unless Kizuna is stopping (the
// load was cancelled, nothing is wrong).
func loadErr(ctx context.Context, list string, err error) {
	if err != nil && ctx.Err() == nil {
		slog.Error(list, "err", err)
	}
}

// keepFresh loads the title list and the mapping lists now and then daily.
// Until each of them has loaded once (first start without a network: no
// cached copy to fall back on) they are retried every 5 minutes instead:
// without titles nothing matches by name, and a mapping list that never loaded leaves
// its ids and episode exceptions out, even when the other one loaded. ready
// says whether the titles loaded and which mapping lists never did.
// firstDone runs after the first load, told whether the titles loaded,
// unless Kizuna started stopping during it: a load cut short isn't "ready".
// Returns when ctx ends.
func keepFresh(ctx context.Context, load func(), ready func() (titles bool, missingLists []string), firstDone func(titles bool)) {
	load()
	if ctx.Err() != nil {
		return
	}
	titles, _ := ready()
	firstDone(titles)
	for {
		wait := 24 * time.Hour
		if titles, missing := ready(); !titles || len(missing) > 0 {
			wait = retryMissing
			slog.Warn("a list is missing (download failed, nothing cached yet); retrying", "in", wait,
				"titles", titles, "mapping_missing", missing)
		}
		select {
		case <-ctx.Done():
			return
		case <-after(wait):
			load()
		}
	}
}

// retryMissing is how soon keepFresh tries again a list that never loaded.
const retryMissing = 5 * time.Minute

// after is time.After; tests replace it to see keepFresh's wait.
var after = time.After

// drainTimeout is how long stopping waits for the requests still running,
// and the show builds they started (see drain). A Plex request gets one
// budget of provider's requestTimeout (25 s), the wait for the first list
// load included; 30 s covers it with a margin (compose.yml sets
// stop_grace_period to 35s to match). Without requests or builds running,
// stopping is immediate.
const drainTimeout = 30 * time.Second

func serve(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	start := time.Now()
	// Open the port before anything else: if it's taken, stop now with a clear
	// message instead of after loading the lists (and never log "ready").
	ln, err := listen(cfg.Listen)
	if err != nil {
		return err
	}
	p, tvdb, keepLoaded := setup(ctx, cfg)
	defer p.Flush() // after the drain below: the builds it waits for may replace crew photos
	if cfg.MatchReports {
		if reports, err := report.Open(filepath.Join(cfg.DataDir, "reports"), provider.Version); err == nil {
			p.SetReports(reports)
			go reports.Run(ctx) //lint:allow goroutine: lives as long as the process, but Store.flush writes each show's page with no recover yet, so one show's panic ends it: not fixed yet
			defer reports.Flush()
		} else {
			slog.Warn("can't write match reports (check the data folder's permissions)", "err", err)
		}
	}
	renamer := extraTitles(cfg)
	if renamer != nil {
		p.OnExtrasRequest(renamer.Queue)
		go renamer.Run(ctx) //lint:allow goroutine: lives as long as the process, but (*Renamer).look has no per-show recover yet, so one show's panic ends it: not fixed yet
	}
	// Serve while the lists load, so /health answers "starting" (and Plex's
	// requests wait for the load) instead of the port hanging until then.
	loaded := p.Loading()
	go keepLoaded(func(titles bool) { //lint:allow goroutine: lives as long as the process: a panic should end it, not leave the lists unloaded unseen
		loaded()
		logReady(cfg, ln, tvdb, start, renamer != nil, titles)
	})

	srv := &http.Server{Handler: p.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: time.Minute, IdleTimeout: 2 * time.Minute}
	closed := make(chan struct{})
	go func() { //lint:allow goroutine: the shutdown: a panic should end the process, not leave it waiting on closed
		<-ctx.Done()
		p.Stop()
		stop() // a second Ctrl+C (or SIGTERM) during the drain stops at once
		drain(srv, p.WaitBuilds, drainTimeout)
		close(closed)
	}()
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	// Serve returns as soon as Shutdown starts: wait for the requests and
	// builds still running, so they finish and the reports' last Flush
	// includes them.
	<-closed
	return nil
}

// extraTitles returns what renames Plex's opening and ending extras, or nil
// when PLEX_URL and PLEX_TOKEN aren't both set (a half or bad setup is
// logged: it would look like the feature doing nothing). PLEX_TOKEN alone
// is a setup of its own: the plex source uses it, so that is an info line.
// PLEX_TLS_NAME is only for the titles: without PLEX_URL it does nothing,
// which is a warning (without PLEX_TOKEN, PLEX_URL's warning covers it).
// A PLEX_TLS_NAME that can't be used turns the titles off too: going on
// without it would fail on every call, or send the token over plain http to
// a user who asked for a certificate check.
func extraTitles(cfg config.Config) *plex.Renamer {
	switch tlsName := cfg.PlexTLSName != ""; {
	case cfg.PlexURL == "" && cfg.PlexToken == "":
		if tlsName {
			slog.Warn("PLEX_TLS_NAME is set without PLEX_URL and PLEX_TOKEN: setting the opening and ending titles in Plex needs both, so PLEX_TLS_NAME is ignored")
		}
		return nil
	case cfg.PlexURL == "" && tlsName:
		slog.Warn("PLEX_TOKEN and PLEX_TLS_NAME are set without PLEX_URL: the plex source uses the token; setting the opening and ending titles in Plex also needs PLEX_URL, so PLEX_TLS_NAME is ignored and they stay as Plex names them")
		return nil
	case cfg.PlexURL == "":
		slog.Info("PLEX_TOKEN is set without PLEX_URL: the plex source uses it; the opening and ending titles also need PLEX_URL, so they stay as Plex names them")
		return nil
	case cfg.PlexToken == "":
		slog.Warn("PLEX_URL is set without PLEX_TOKEN: setting the opening and ending titles in Plex needs both, so they stay as Plex names them")
		return nil
	}
	if err := plex.CheckURL(cfg.PlexURL); err != nil {
		slog.Warn("PLEX_URL can't be used, so the opening and ending titles stay as Plex names them", "why", err)
		return nil
	}
	if cfg.PlexTLSName != "" {
		if err := plex.CheckTLSName(cfg.PlexTLSName, cfg.PlexURL); err != nil {
			slog.Warn("PLEX_TLS_NAME can't be used, so the opening and ending titles stay as Plex names them", "why", err)
			return nil
		}
	}
	return plex.New(cfg.PlexURL, cfg.PlexToken, cfg.PlexTLSName)
}

// drain stops the server within timeout in all: it waits for the requests
// still running, then for the show builds that outlived theirs (Plex gave
// up on them). A build queues its match report only when it ends, and
// serve's last report flush comes after the drain.
func drain(srv *http.Server, waitBuilds func(context.Context) bool, timeout time.Duration) {
	slog.Info("stopping: waiting for running requests", "at_most", timeout)
	shutdown, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	if !waitBuilds(shutdown) {
		slog.Warn("stopping with a show build still running; it is lost, and so is its match report", "waited", timeout)
	}
}

// logReady logs the settings in effect once the first list load is over.
// extraTitles says whether the opening and ending titles are really on
// (extraTitles returned a Renamer), not just set. Without the title list
// (titles false: first start without a network, nothing cached) the line is
// a warning instead of "ready": no show matches by name until it loads, and
// /health answers "starting" until then too.
func logReady(cfg config.Config, ln net.Listener, tvdb *sources.TVDB, start time.Time, extraTitles, titles bool) {
	level, msg := slog.LevelInfo, "ready"
	if !titles {
		level, msg = slog.LevelWarn, "started without AniDB's title list: shows match only by id, not by name, until it loads (retried every "+retryMissing.String()+")"
	}
	u := cfg.UsableOf // each list as it works: sources without a key are skipped
	// The cache cleanup frees at least CACHE_MIN_FREE (fetch.KeepFree raises a
	// lower target to it): log the target it works with.
	slog.Log(context.Background(), level, msg, "version", provider.Version, "listen", ln.Addr().String(), "allow_from", cfg.AllowFrom, "allowed_hosts", cfg.AllowedHosts, "startup", time.Since(start).Round(time.Millisecond),
		"tvdb", cmp.Or(tvdb.API(), "off"), "tmdb", cfg.TMDBKey != "", "mal", cfg.MALClientID != "", "plex", cfg.PlexToken != "", "anidb_client", cfg.AniDBClient != "",
		"titles", u(cfg.TitleSources), "summaries", u(cfg.SummarySources), "posters", u(cfg.PosterSources),
		"backgrounds", u(cfg.BackgroundSources), "logos", u(cfg.LogoSources), "art_all", cfg.ArtAllSources, "season_art_fallback", cfg.SeasonArtFallback,
		"genres", u(cfg.GenreSources), "cast", u(cfg.CastSources), "cast_photos_from_plex", cfg.CastPhotosFromPlex, "studio", u(cfg.StudioSources), "crew", u(cfg.CrewSources),
		"rating", u(cfg.RatingSources), "rating_country", cfg.RatingCountries, "date", u(cfg.DateSources),
		"episode_fill", u(cfg.EpisodeFillSources), "episode_crew", u(cfg.EpisodeCrewSources), "crew_photos_from_plex", cfg.CrewPhotosFromPlex, "episode_titles", u(cfg.EpisodeTitles), "auto_default", cfg.AutoDefault, "cache_min_free_mb", cfg.CacheMinFree>>20, "cache_free_target_mb", max(cfg.CacheFreeTarget, cfg.CacheMinFree)>>20,
		"match_reports", cfg.MatchReports, "extra_titles", extraTitles)
}

// listen opens the address Kizuna serves on. A port that another program
// already uses gets an error that says so and how to fix it.
func listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil
	}
	_, port, _ := net.SplitHostPort(addr)
	if portInUse(err) {
		return nil, fmt.Errorf("port %s is already in use by another program (another Kizuna?): stop that program, "+
			"or set PORT to a free port (in Docker: change the host side of the port mapping instead): %w", port, err)
	}
	return nil, fmt.Errorf("can't listen on %q (check PORT / LISTEN): %w", addr, err)
}

// portInUse recognises "address already in use" on Linux/macOS and its
// Windows wording ("Only one usage of each socket address...").
func portInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "address already in use") || strings.Contains(s, "only one usage of each socket address")
}

// healthURL is where the health check asks: 127.0.0.1 when Kizuna listens
// on every address (":32600", "0.0.0.0", or "[::]", which takes IPv4 too;
// a container may have no IPv6 loopback), else the one address it listens
// on (LISTEN=<ip>), which /health answers from the machine itself whatever
// ALLOW_FROM says.
func healthURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen + "/health"
	}
	if ip, err := netip.ParseAddr(host); host == "" || (err == nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	// url.URL escapes an IPv6 zone ("%eth0" becomes "%25eth0").
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: "/health"}).String()
}

// healthClient never uses a proxy (HTTP_PROXY): the check always asks this
// machine, and through a proxy it would come from the proxy's address.
var healthClient = &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{Proxy: nil}}

func health(cfg config.Config) int {
	resp, err := healthClient.Get(healthURL(cfg.Listen))
	if err != nil {
		fmt.Fprintln(os.Stderr, "kizuna health:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The answer says why ({"status":"degraded","missing":["mapping"]...}):
		// Docker keeps it, so `docker inspect` shows it next to "unhealthy".
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		fmt.Fprintln(os.Stderr, "kizuna health: HTTP", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}
	return 0
}

// newLogHandler is the format of every log line: slog's text lines. It
// quotes a message, key or value that holds a line break, so text from a
// request can't end its line and start one that reads like another entry
// (TestLogLinesCantBeForged). CodeQL's go/log-injection is off because of it
// (.github/codeql/codeql-config.yml): a handler that doesn't quote needs it on.
func newLogHandler(out io.Writer, level slog.Leveler) slog.Handler {
	return slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})
}
