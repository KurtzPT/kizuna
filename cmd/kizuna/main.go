// Command kizuna runs the Plex metadata provider.
//
//	kizuna         run the provider (settings: env vars, see SETUP.md)
//	kizuna health  exit 0 when the running provider is ready (else 1, saying why on stderr)
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // TZ works in the distroless image (no zoneinfo there): log days follow it

	"kizuna/internal/anidb"
	"kizuna/internal/config"
	"kizuna/internal/fetch"
	"kizuna/internal/logfile"
	"kizuna/internal/mapping"
	"kizuna/internal/provider"
	"kizuna/internal/sources"
)

func main() {
	// Two levels: info (what happened) and debug (everything needed to see why).
	level, bad := slog.LevelInfo, ""
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))); v {
	case "", "info":
	case "debug":
		level = slog.LevelDebug
	default:
		bad = v
	}
	healthCheck := len(os.Args) > 1 && os.Args[1] == "health" // Docker healthcheck (the image has no curl)

	// Logs go to standard output (docker logs) and, for the server, to
	// <DATA_DIR>/logs/kizuna.log: one file per day, older days renamed
	// kizuna-YYYY-MM-DD.log and deleted after LOG_RETENTION_DAYS.
	out, fileErr := io.Writer(os.Stdout), error(nil)
	if !healthCheck {
		if f, err := logfile.Open(filepath.Join(config.DataDir(), "logs"), "kizuna", config.LogRetentionDays()); err == nil {
			defer f.Close()
			out = io.MultiWriter(os.Stdout, f)
		} else {
			fileErr = err
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})))
	if fileErr != nil {
		slog.Warn("can't write the log file; logging to standard output only (check the data folder's permissions)", "err", fileErr)
	}
	if bad != "" {
		slog.Warn("unknown LOG_LEVEL; using info", "LOG_LEVEL", bad, "allowed", "info,debug")
	}

	cfg := config.Load()
	fetch.UserAgent = "kizuna/" + provider.Version + " (personal Plex metadata provider)"
	if healthCheck {
		os.Exit(health(cfg))
	}
	if err := serve(cfg); err != nil {
		slog.Error("can't run: " + err.Error())
		os.Exit(1)
	}
}

// setup creates every source and loads the two lists matching depends on.
// It returns TheTVDB too, for the ready line.
func setup(ctx context.Context, cfg config.Config) (*provider.Provider, *sources.TVDB) {
	f := fetch.New(filepath.Join(cfg.DataDir, "cache"))
	go f.KeepFree(ctx, cfg.CacheMinFree, cfg.CacheFreeTarget)
	titles, lists := &anidb.Titles{}, &mapping.Lists{}
	ad := sources.NewAniDB(f, cfg.DataDir, cfg.AniDBClient, cfg.AniDBClientVer)
	tvdb := sources.NewTVDB(f, cfg.TVDBKey, cfg.TVDBPin, cfg.TVDBLegacyKey)
	if cfg.TVDBKey != "" && cfg.TVDBLegacyKey != "" {
		slog.Info("TVDB_API_KEY and TVDB_LEGACY_API_KEY are both set: using the v4 key; the legacy key takes over (with a warning) if TheTVDB rejects it")
	}
	p := provider.New(cfg, titles, ad, lists, tvdb,
		sources.NewTMDB(f, cfg.TMDBKey), sources.NewMAL(f, cfg.MALClientID), sources.NewANN(f))
	load := func() {
		if err := titles.Load(ctx, f); err != nil {
			slog.Error("AniDB title list", "err", err)
		}
		if err := lists.Load(ctx, f, cfg.DataDir, cfg.MappingSources); err != nil {
			slog.Error("anime mapping", "err", err)
		}
	}
	load()
	// Both lists refresh daily. Until both have loaded once (first start
	// without a network: no cached copy to fall back on, so nothing matches)
	// they are retried every 5 minutes instead.
	go func() {
		for {
			wait := 24 * time.Hour
			if !titles.Ready() || !lists.Ready() {
				wait = 5 * time.Minute
				slog.Warn("a list is missing (download failed, nothing cached yet); retrying", "in", wait,
					"titles", titles.Ready(), "mapping", lists.Ready())
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
				load()
			}
		}
	}()
	return p, tvdb
}

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
	p, tvdb := setup(ctx, cfg)
	slog.Info("ready", "version", provider.Version, "listen", ln.Addr().String(), "allow_from", cfg.AllowFrom, "allowed_hosts", cfg.AllowedHosts, "startup", time.Since(start).Round(time.Millisecond),
		"tvdb", cmp.Or(tvdb.API(), "off"), "tmdb", cfg.TMDBKey != "", "mal", cfg.MALClientID != "", "anidb_client", cfg.AniDBClient != "",
		"titles", cfg.TitleSources, "summaries", cfg.SummarySources, "posters", cfg.PosterSources,
		"backgrounds", cfg.BackgroundSources, "logos", cfg.LogoSources, "episodes", cfg.EpisodeSources, "art_all", cfg.ArtAllSources,
		"genres", cfg.GenreSources, "cast", cfg.CastSources, "studio", cfg.StudioSources, "crew", cfg.CrewSources,
		"rating", cfg.RatingSources, "rating_country", cfg.RatingCountries, "mapping", cfg.MappingSources, "date", cfg.DateSources,
		"episode_fill", cfg.EpisodeFillSources, "episode_titles", cfg.EpisodeTitles, "auto_default", cfg.AutoDefault, "cache_min_free_mb", cfg.CacheMinFree>>20, "cache_free_target_mb", cfg.CacheFreeTarget>>20)

	srv := &http.Server{Handler: p.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: time.Minute, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
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

func health(cfg config.Config) int {
	addr := cfg.Listen
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Get("http://" + addr + "/health")
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
