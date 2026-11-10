// Package config reads all settings from environment variables, which may
// come from a settings file (see LoadEnvFile). Every setting has a sensible
// default except the sources' keys and the AniDB client name.
package config

import (
	"fmt"
	"log/slog"
	"math"
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
	AniDBClientVer string // the version registered with that client

	TVDBKey       string // TheTVDB v4 API key (optional, enables TVDB episodes and art)
	TVDBPin       string // only for subscriber keys
	TVDBLegacyKey string // legacy TheTVDB key (v3 API): when TVDBKey is empty or rejected
	TMDBKey       string // TheMovieDb v3 API key (optional)
	MALClientID   string // MyAnimeList API v2 client ID (optional, public data only)

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
	// rest are fallbacks in order. Each comment lists the allowed sources in
	// their default order (see Load).
	TitleSources      []string // anidb, tvdb, tmdb, ann
	SummarySources    []string // anidb, tvdb, tmdb, mal, ann
	PosterSources     []string // tvdb, tmdb, anidb, mal, ann
	BackgroundSources []string // tvdb, tmdb
	LogoSources       []string // tvdb, tmdb
	// EpisodeFillSources: in /anidb mode, where missing episode thumbnails,
	// summaries, dates and runtimes come from, matched through the
	// mapping's TVDB season and episode offset. tvdb. (Titles follow
	// EpisodeTitles.)
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
	// SeasonArtFallback: a TheTVDB/TheMovieDb season without art of a kind
	// gives that site's series art of the kind instead. false = nothing from
	// that site: the next source in the order.
	SeasonArtFallback bool

	// CacheMinFree / CacheFreeTarget (bytes): when the cache disk has less
	// than CacheMinFree free, the oldest cache files are deleted until
	// CacheFreeTarget is free. 0 = never clean up.
	CacheMinFree, CacheFreeTarget uint64
	// MatchReports writes an HTML report per show to <DataDir>/reports.
	MatchReports bool

	// PlexURL and PlexToken: the Plex server's address and an account token.
	// With both, the opening and ending extras Plex makes from local files
	// get readable titles through Plex's API (internal/plex).
	PlexURL, PlexToken string
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
		TVDBLegacyKey:      env("TVDB_LEGACY_API_KEY", ""),
		TMDBKey:            env("TMDB_API_KEY", ""),
		MALClientID:        env("MAL_CLIENT_ID", ""),
		MappingSources:     mappingSources(),
		TitleLangs:         lowerList(env("TITLE_LANGS", "main,x-jat,en")),
		TitleOfficialOnly:  flag("TITLE_OFFICIAL_ONLY", false),
		OriginalTitleLangs: langList("ORIGINAL_TITLE_LANGS", "ja"),
		EpisodeLangs:       lowerList(env("EPISODE_LANGS", "en,x-jat")),
		MinScore:           num("MIN_SCORE", 80, 0, 100),

		TitleSources:       sourceList("TITLE_SOURCES", "anidb,tvdb,tmdb,ann", "anidb", "tvdb", "tmdb", "ann"),
		SummarySources:     sourceList("SUMMARY_SOURCES", "anidb,tvdb,tmdb,mal,ann", "anidb", "tvdb", "tmdb", "mal", "ann"),
		PosterSources:      sourceList("POSTER_SOURCES", "tvdb,tmdb,anidb,mal,ann", "anidb", "tvdb", "tmdb", "mal", "ann"),
		BackgroundSources:  sourceList("BACKGROUND_SOURCES", "tvdb,tmdb", "tvdb", "tmdb"),
		LogoSources:        sourceList("LOGO_SOURCES", "tvdb,tmdb", "tvdb", "tmdb"),
		EpisodeFillSources: sourceList("EPISODE_FILL_SOURCES", "tvdb", "tvdb"),
		GenreSources:       sourceList("GENRE_SOURCES", "anidb,tvdb,tmdb,mal,ann", "anidb", "tvdb", "tmdb", "mal", "ann"),
		CastSources:        sourceList("CAST_SOURCES", "anidb,tvdb,tmdb,ann", "anidb", "tvdb", "tmdb", "ann"),
		StudioSources:      sourceList("STUDIO_SOURCES", "anidb,tvdb,tmdb,mal,ann", "anidb", "tvdb", "tmdb", "mal", "ann"),
		CrewSources:        sourceList("CREW_SOURCES", "anidb,tvdb,tmdb,ann", "anidb", "tvdb", "tmdb", "ann"),
		RatingSources:      sourceList("RATING_SOURCES", "anidb,tvdb,tmdb,mal,ann", "anidb", "tvdb", "tmdb", "mal", "ann"),
		RatingCountries:    upperList(env("RATING_COUNTRY", "US")),
		DateSources:        sourceList("DATE_SOURCES", "anidb,tvdb,tmdb,mal,ann", "anidb", "tvdb", "tmdb", "mal", "ann"),
		ArtAllSources:      flag("ART_ALL_SOURCES", false),
		SeasonArtFallback:  flag("SEASON_ART_FALLBACK", true),
		AutoDefault:        oneOf("AUTO_DEFAULT", "anidb", "tvdb"),
		EpisodeTitles:      sourceList("EPISODE_TITLES", "anidb,tvdb,tmdb", "anidb", "tvdb", "tmdb"),
		CacheMinFree:       size("CACHE_MIN_FREE", 1<<30),
		CacheFreeTarget:    size("CACHE_FREE_TARGET", 2<<30),
		MatchReports:       flag("MATCH_REPORTS", false),
		PlexURL:            env("PLEX_URL", ""),
		PlexToken:          env("PLEX_TOKEN", ""),
	}
}

// Usable reports whether a source has what it needs to answer: AniDB a
// client name, TheTVDB a v4 or legacy key, TheMovieDb a key, MyAnimeList a
// client ID. Anime News Network needs nothing. Each source list uses the
// first usable source that has a value; the others are skipped.
func (c Config) Usable(source string) bool {
	switch source {
	case "anidb":
		return c.AniDBClient != ""
	case "tvdb":
		return c.TVDBKey != "" || c.TVDBLegacyKey != ""
	case "tmdb":
		return c.TMDBKey != ""
	case "mal":
		return c.MALClientID != ""
	}
	return source == "ann"
}

// UsableOf keeps the usable sources of a list, in order.
func (c Config) UsableOf(list []string) []string {
	return slices.DeleteFunc(slices.Clone(list), func(s string) bool { return !c.Usable(s) })
}

// What a mode is missing: /anidb takes its episodes from AniDB, /tvdb from
// TheTVDB, /auto from either (per show).
const (
	noAniDB = "no ANIDB_CLIENT"
	noTVDB  = "no TVDB_API_KEY or TVDB_LEGACY_API_KEY"
)

// ModeOff says why a mode ("anidb", "tvdb", "auto") can't build shows; ""
// when it can. /auto works with either source, for the shows that use it.
func (c Config) ModeOff(mode string) string {
	return ModeOff(mode, c.Usable("anidb"), c.Usable("tvdb"))
}

// ModeOff is the rule behind Config.ModeOff, for callers that know which
// sources are set up (the provider asks its sources).
func ModeOff(mode string, anidb, tvdb bool) string {
	switch {
	case mode == "anidb" && !anidb:
		return noAniDB
	case mode == "tvdb" && !tvdb:
		return noTVDB
	case mode == "auto" && !anidb && !tvdb:
		return noAniDB + " and " + noTVDB
	}
	return ""
}

// CheckModes fails when no mode can build shows. One working mode is enough:
// Kizuna can't know which ones Plex uses.
func (c Config) CheckModes() error {
	if why := c.ModeOff("auto"); why != "" {
		return fmt.Errorf("no mode can build shows (%s): set ANIDB_CLIENT for /anidb, "+
			"TVDB_API_KEY or TVDB_LEGACY_API_KEY for /tvdb, or both for /auto. See SETUP.md", why)
	}
	return nil
}

// LogModes logs, at startup, which modes are ready, and the source lists
// left without a usable source (their field stays empty).
func (c Config) LogModes() {
	state := func(mode string) string {
		if why := c.ModeOff(mode); why != "" {
			return "off: " + why
		}
		return "ready"
	}
	auto := "ready"
	switch {
	case c.ModeOff("anidb") != "":
		auto = "only shows with TVDB rules (" + noAniDB + ")"
	case c.ModeOff("tvdb") != "":
		auto = "only shows with AniDB rules (" + noTVDB + ")"
	}
	slog.Info("modes", "anidb", state("anidb"), "tvdb", state("tvdb"), "auto", auto)
	set, defaults, keys := c.emptyFields()
	if len(set) > 0 {
		slog.Warn("no usable source in these lists, so their fields stay empty; add a key, or set them to none",
			"settings", set)
	}
	// Lists left at their default are no mistake (the defaults name sources
	// that need a key, such as backgrounds from TheTVDB): no warning at every
	// start, just which key would fill them.
	if len(defaults) > 0 {
		slog.Info("these fields stay empty: their default sources need a key", "settings", defaults, "keys", keys)
	}
}

// sourceKeys: the setting that makes each source usable (see Usable).
var sourceKeys = map[string]string{"anidb": "ANIDB_CLIENT", "tvdb": "TVDB_API_KEY", "tmdb": "TMDB_API_KEY", "mal": "MAL_CLIENT_ID"}

// emptyFields: the source lists that name sources, none of them usable
// ("none" turns a list off on purpose and isn't reported). set are the ones
// set in the environment or the settings file, defaults the ones left at
// their default, and keys the settings that would make the defaults' sources
// usable.
func (c Config) emptyFields() (set, defaults, keys []string) {
	for _, f := range []struct {
		name string
		list []string
	}{
		{"TITLE_SOURCES", c.TitleSources}, {"SUMMARY_SOURCES", c.SummarySources}, {"POSTER_SOURCES", c.PosterSources},
		{"BACKGROUND_SOURCES", c.BackgroundSources}, {"LOGO_SOURCES", c.LogoSources},
		{"EPISODE_FILL_SOURCES", c.EpisodeFillSources}, {"EPISODE_TITLES", c.EpisodeTitles},
		{"GENRE_SOURCES", c.GenreSources}, {"CAST_SOURCES", c.CastSources}, {"STUDIO_SOURCES", c.StudioSources},
		{"CREW_SOURCES", c.CrewSources}, {"RATING_SOURCES", c.RatingSources}, {"DATE_SOURCES", c.DateSources},
	} {
		if len(f.list) == 0 || len(c.UsableOf(f.list)) > 0 {
			continue
		}
		if isSet(f.name) {
			set = append(set, f.name)
			continue
		}
		defaults = append(defaults, f.name)
		for _, s := range f.list {
			if k := sourceKeys[s]; k != "" && !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	return set, defaults, keys
}

// isSet reports whether a setting has a value of its own (env gives it
// its default when it's unset or blank). The settings file is in the
// environment by now (LoadEnvFile).
func isSet(name string) bool { return strings.TrimSpace(os.Getenv(name)) != "" }

// defaultPort is Kizuna's port unless PORT or LISTEN says otherwise.
const defaultPort = 32600

// listenAddr: LISTEN (a full address such as "127.0.0.1:32600") wins; else
// PORT on every network interface.
func listenAddr() string {
	markRead("PORT") // a real setting even when LISTEN hides it: not a typo in the settings file
	if v := env("LISTEN", ""); v != "" {
		return v
	}
	return ":" + strconv.Itoa(port())
}

// port reads PORT (1-65535); anything else warns and uses defaultPort.
func port() int { return num("PORT", defaultPort, 1, 65535) }

// DataDir is the folder for cache, state and logs (DATA_DIR). Separate from
// Load so logging can start before the settings are read.
func DataDir() string { return env("DATA_DIR", "./data") }

// LogRetentionDays is how many past days of logs are kept (LOG_RETENTION_DAYS,
// default 7; 0, or 100000 and up, = keep forever). Read before Load, like
// DataDir.
func LogRetentionDays() int { return num("LOG_RETENTION_DAYS", 7, 0, math.MaxInt) }

// LogLevel is LOG_LEVEL as given (trimmed); main checks it. Read before
// Load, like DataDir.
func LogLevel() string { return env("LOG_LEVEL", "") }

// sourceList reads a comma-separated source order. "none" turns the field
// off; unknown or unsupported names are dropped with a warning. A list with
// no known source left uses the default, with a warning: a typo must not
// turn a field off (an empty MAPPING_SOURCES would never load a mapping).
func sourceList(name, def string, allowed ...string) []string {
	raw := strings.ToLower(env(name, def))
	if raw == "none" {
		return nil
	}
	out := knownSources(name, raw, allowed)
	if len(out) == 0 && raw != def {
		slog.Warn("no known source left; using the default (\"none\" turns the field off)", "setting", name,
			"value", raw, "default", def)
		return knownSources(name, def, allowed)
	}
	return out
}

// mappingSources reads MAPPING_SOURCES. Unlike the other lists it can't be
// "none": without a list the mapping never loads and /health stays down.
func mappingSources() []string {
	const name, def = "MAPPING_SOURCES", "animelists,fribb"
	if raw := env(name, def); strings.EqualFold(raw, "none") {
		slog.Warn("a mapping list is required; using the default", "setting", name, "value", raw, "default", def)
		return list(def)
	}
	return sourceList(name, def, "animelists", "fribb")
}

// knownSources keeps the allowed names of a source list, once each.
func knownSources(name, raw string, allowed []string) []string {
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

// lowerList is a comma-separated list in lowercase (language codes: "EN" is "en").
func lowerList(s string) []string { return list(strings.ToLower(s)) }

// langList is a comma-separated language list; "none" means empty.
func langList(name, def string) []string {
	if v := env(name, def); !strings.EqualFold(v, "none") {
		return lowerList(v)
	}
	return nil
}

// flag reads a boolean (true/false, 1/0); anything else warns and uses def.
func flag(name string, def bool) bool {
	raw := env(name, "")
	if raw == "" {
		return def
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		slog.Warn("ignoring bad value; using the default", "setting", name, "value", raw, "allowed", "true,false", "default", def)
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
	// !(f >= 0) also catches NaN; past 1 EB the number is a typo (and
	// would wrap the uint64).
	if err != nil || !(f >= 0) || f*mult > 1<<60 {
		slog.Warn("ignoring bad size; using the default", "setting", name, "value", env(name, ""), "default_mb", def>>20)
		return def
	}
	return uint64(f * mult)
}

func env(name, def string) string {
	markRead(name)
	if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// num reads a whole number from lo to hi; anything else warns and uses def.
func num(name string, def, lo, hi int) int {
	raw := env(name, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		allowed := fmt.Sprintf("%d-%d", lo, hi)
		if hi == math.MaxInt {
			allowed = fmt.Sprintf("%d or more", lo)
		}
		slog.Warn("ignoring bad value; using the default", "setting", name, "value", raw, "allowed", allowed, "default", def)
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
