// Package config reads all settings from environment variables.
// Every setting has a sensible default except the AniDB client name.
package config

import (
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Name is the project name. It is part of the Plex provider identifiers
// (tv.plex.agents.custom.<Name>.anidb), so changing it after libraries are
// matched makes Plex re-match everything.
const Name = "kizuna"

type Config struct {
	Listen string // address to listen on: LISTEN, else ":" + PORT (default ":32600")

	// AllowFrom: client networks that may use Kizuna ("private", CIDRs, IPs,
	// "any"). AllowedHosts: extra host names requests may be addressed to
	// ("any" turns the check off). See provider/access.go.
	AllowFrom, AllowedHosts []string
	DataDir                 string // cache and state

	AniDBClient    string // registered AniDB HTTP client name (required for AniDB details)
	AniDBClientVer string //

	TVDBKey     string // TheTVDB v4 API key (optional, enables TVDB episodes and art)
	TVDBPin     string // only for subscriber keys
	TMDBKey     string // TheMovieDb v3 API key (optional)
	MALClientID string // MyAnimeList API v2 client ID (optional, public data only)

	// MappingSources: AniDB id mapping lists, in priority order; later ones
	// only fill missing ids. animelists, fribb.
	MappingSources []string

	TitleLangs []string // show title priority; "main" = AniDB main (romaji) title
	// TitleOfficialOnly skips AniDB synonyms for TitleLangs, so a language
	// without an official title falls through to the next one.
	TitleOfficialOnly  bool
	OriginalTitleLangs []string // originalTitle priority (official titles only); none = no original title
	EpisodeLangs       []string // episode title priority
	MinScore           int      // lowest title-search score accepted automatically (0-100)

	// Source order per field: the first source that has a value wins, the
	// rest are fallbacks in order.
	TitleSources      []string // anidb, tvdb, tmdb, ann
	SummarySources    []string // anidb, tvdb, tmdb, mal, ann
	PosterSources     []string // tmdb, tvdb, anidb, mal, ann
	BackgroundSources []string // tmdb, tvdb
	LogoSources       []string // tmdb, tvdb
	EpisodeSources    []string // tvdb, tmdb (episode list of the /tvdb mode)
	// EpisodeFillSources: in /anidb mode, where missing episode thumbnails,
	// summaries, titles, dates and runtimes come from, matched through the
	// mapping's TVDB season and episode offset. tvdb.
	EpisodeFillSources []string
	// EpisodeTitles: where episode titles come from, in both modes (first
	// real title wins). anidb (per EpisodeLangs), tvdb, tmdb.
	EpisodeTitles []string
	// AutoDefault: the rules /auto uses for a show without an AniDB or TVDB
	// tag or GUID. anidb or tvdb.
	AutoDefault string
	// In /anidb mode, TVDB and TMDB describe the whole series, so for these
	// fields they are only used on the series' first AniDB entry.
	GenreSources  []string // anidb, tvdb, tmdb, mal, ann
	CastSources   []string // anidb, tvdb, tmdb, ann
	StudioSources []string // anidb, tvdb, tmdb, mal, ann
	CrewSources   []string // anidb, tvdb, tmdb, ann (directors and writers)
	RatingSources []string // anidb, tvdb, tmdb, mal, ann (content rating)
	// RatingCountries: TVDB/TMDB rating country order (ISO 3166-1 alpha-2, e.g. "DE,US").
	RatingCountries []string
	DateSources     []string // anidb, tvdb, tmdb, mal, ann
	// ArtAllSources offers posters/backgrounds/logos from every listed source
	// (the first source's best image is the default). false = only the first
	// source that has any.
	ArtAllSources bool

	// CacheMinFree / CacheFreeTarget (bytes): when the cache disk has less
	// than CacheMinFree free, the oldest cache files are deleted until
	// CacheFreeTarget is free. 0 = never clean up.
	CacheMinFree, CacheFreeTarget uint64
}

func Load() Config {
	return Config{
		Listen:             listenAddr(),
		AllowFrom:          list(env("ALLOW_FROM", "private")),
		AllowedHosts:       list(env("ALLOWED_HOSTS", "")),
		DataDir:            DataDir(),
		AniDBClient:        env("ANIDB_CLIENT", ""),
		AniDBClientVer:     env("ANIDB_CLIENTVER", "1"),
		TVDBKey:            env("TVDB_API_KEY", ""),
		TVDBPin:            env("TVDB_PIN", ""),
		TMDBKey:            env("TMDB_API_KEY", ""),
		MALClientID:        env("MAL_CLIENT_ID", ""),
		MappingSources:     sourceList("MAPPING_SOURCES", "animelists,fribb", "animelists", "fribb"),
		TitleLangs:         list(env("TITLE_LANGS", "main,en")),
		TitleOfficialOnly:  flag("TITLE_OFFICIAL_ONLY", false),
		OriginalTitleLangs: langList("ORIGINAL_TITLE_LANGS", "ja"),
		EpisodeLangs:       list(env("EPISODE_LANGS", "en,x-jat")),
		MinScore:           num("MIN_SCORE", 60),

		TitleSources:       sourceList("TITLE_SOURCES", "anidb,tvdb,tmdb", "anidb", "tvdb", "tmdb", "ann"),
		SummarySources:     sourceList("SUMMARY_SOURCES", "anidb,tvdb,tmdb,mal,ann", "anidb", "tvdb", "tmdb", "mal", "ann"),
		PosterSources:      sourceList("POSTER_SOURCES", "tmdb,tvdb,anidb,mal", "anidb", "tvdb", "tmdb", "mal", "ann"),
		BackgroundSources:  sourceList("BACKGROUND_SOURCES", "tmdb,tvdb", "tvdb", "tmdb"),
		LogoSources:        sourceList("LOGO_SOURCES", "tmdb,tvdb", "tvdb", "tmdb"),
		EpisodeSources:     sourceList("EPISODE_SOURCES", "tvdb,tmdb", "tvdb", "tmdb"),
		EpisodeFillSources: sourceList("EPISODE_FILL_SOURCES", "tvdb", "tvdb"),
		GenreSources:       sourceList("GENRE_SOURCES", "anidb,mal,tmdb,tvdb,ann", "anidb", "tvdb", "tmdb", "mal", "ann"),
		CastSources:        sourceList("CAST_SOURCES", "anidb,tmdb,tvdb,ann", "anidb", "tvdb", "tmdb", "ann"),
		StudioSources:      sourceList("STUDIO_SOURCES", "anidb,mal,tmdb,tvdb,ann", "anidb", "tvdb", "tmdb", "mal", "ann"),
		CrewSources:        sourceList("CREW_SOURCES", "anidb,tmdb,tvdb,ann", "anidb", "tvdb", "tmdb", "ann"),
		RatingSources:      sourceList("RATING_SOURCES", "anidb,mal,tmdb,tvdb", "anidb", "tvdb", "tmdb", "mal", "ann"),
		RatingCountries:    upperList(env("RATING_COUNTRY", "US")),
		DateSources:        sourceList("DATE_SOURCES", "anidb,mal,tmdb,tvdb,ann", "anidb", "tvdb", "tmdb", "mal", "ann"),
		ArtAllSources:      flag("ART_ALL_SOURCES", true),
		AutoDefault:        oneOf("AUTO_DEFAULT", "anidb", "tvdb"),
		EpisodeTitles:      sourceList("EPISODE_TITLES", "anidb,tvdb,tmdb", "anidb", "tvdb", "tmdb"),
		CacheMinFree:       size("CACHE_MIN_FREE", 1<<30),
		CacheFreeTarget:    size("CACHE_FREE_TARGET", 2<<30),
	}
}

// DefaultPort is Kizuna's port unless PORT or LISTEN says otherwise.
const DefaultPort = 32600

// listenAddr: LISTEN (a full address such as "127.0.0.1:32600") wins; else
// PORT on every network interface.
func listenAddr() string {
	if v := env("LISTEN", ""); v != "" {
		return v
	}
	return ":" + strconv.Itoa(port())
}

// port reads PORT (1-65535); anything else warns and uses DefaultPort.
func port() int {
	raw := env("PORT", "")
	if raw == "" {
		return DefaultPort
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 65535 {
		slog.Warn("ignoring bad PORT; using the default", "PORT", raw, "allowed", "1-65535", "default", DefaultPort)
		return DefaultPort
	}
	return n
}

// DataDir is the folder for cache, state and logs (DATA_DIR). Separate from
// Load so logging can start before the settings are read.
func DataDir() string { return env("DATA_DIR", "./data") }

// LogRetentionDays is how many past days of logs are kept (LOG_RETENTION_DAYS,
// default 7; 0 = keep forever). Read before Load, like DataDir.
func LogRetentionDays() int {
	if n := num("LOG_RETENTION_DAYS", 7); n >= 0 {
		return n
	}
	return 7
}

// sourceList reads a comma-separated source order. "none" turns the field
// off; unknown or unsupported names are dropped with a warning.
func sourceList(name, def string, allowed ...string) []string {
	raw := strings.ToLower(env(name, def))
	if raw == "none" {
		return nil
	}
	var out []string
	for _, s := range list(raw) {
		if !slices.Contains(allowed, s) {
			slog.Warn("ignoring unsupported source", "setting", name, "source", s, "allowed", strings.Join(allowed, ","))
			continue
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func upperList(s string) []string {
	out := list(s)
	for i := range out {
		out[i] = strings.ToUpper(out[i])
	}
	return out
}

// langList is a comma-separated language list; "none" means empty.
func langList(name, def string) []string {
	if v := env(name, def); !strings.EqualFold(v, "none") {
		return list(v)
	}
	return nil
}

func flag(name string, def bool) bool {
	b, err := strconv.ParseBool(env(name, ""))
	if err != nil {
		return def
	}
	return b
}

// oneOf reads a setting that must be one of the allowed values (the first
// is the default); anything else warns and uses the default.
func oneOf(name string, allowed ...string) string {
	v := strings.ToLower(env(name, allowed[0]))
	if !slices.Contains(allowed, v) {
		slog.Warn("ignoring unknown value; using the default", "setting", name, "value", v,
			"allowed", strings.Join(allowed, ","), "default", allowed[0])
		return allowed[0]
	}
	return v
}

// size reads a byte size: "500MB", "2GB", "1.5TB" (plain numbers are MB,
// 1 GB = 1024 MB). Bad values fall back to def with a warning.
func size(name string, def uint64) uint64 {
	raw := strings.ToUpper(strings.ReplaceAll(env(name, ""), " ", ""))
	if raw == "" {
		return def
	}
	mult := float64(1 << 20)
	for _, u := range []struct {
		suffix string
		mult   float64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}} {
		if strings.HasSuffix(raw, u.suffix) {
			raw, mult = strings.TrimSuffix(raw, u.suffix), u.mult
			break
		}
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f < 0 {
		slog.Warn("ignoring bad size; using the default", "setting", name, "value", env(name, ""), "default_mb", def>>20)
		return def
	}
	return uint64(f * mult)
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func num(name string, def int) int {
	n, err := strconv.Atoi(env(name, ""))
	if err != nil {
		return def
	}
	return n
}

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
