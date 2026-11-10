package config

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func TestSourceList(t *testing.T) {
	t.Setenv("X_SOURCES", " TVDB, foo, anidb, tvdb ")
	if got := sourceList("X_SOURCES", "anidb", "anidb", "tvdb"); !slices.Equal(got, []string{"tvdb", "anidb"}) {
		t.Errorf("got %v", got)
	}
	t.Setenv("X_SOURCES", "none")
	if got := sourceList("X_SOURCES", "anidb", "anidb"); got != nil {
		t.Errorf("none: %v", got)
	}
	if got := sourceList("UNSET_SOURCES", "tmdb,tvdb", "tvdb", "tmdb"); !slices.Equal(got, []string{"tmdb", "tvdb"}) {
		t.Errorf("default: %v", got)
	}
}

// Language codes are lowercased, like the source lists: "EN" is "en".
func TestLangsLowercase(t *testing.T) {
	t.Setenv("TITLE_LANGS", "Main, EN")
	t.Setenv("EPISODE_LANGS", "X-JAT,En")
	t.Setenv("ORIGINAL_TITLE_LANGS", "JA,zh-Hans")
	c := Load()
	if !slices.Equal(c.TitleLangs, []string{"main", "en"}) || !slices.Equal(c.EpisodeLangs, []string{"x-jat", "en"}) ||
		!slices.Equal(c.OriginalTitleLangs, []string{"ja", "zh-hans"}) {
		t.Errorf("title %v, episode %v, original %v", c.TitleLangs, c.EpisodeLangs, c.OriginalTitleLangs)
	}
	t.Setenv("ORIGINAL_TITLE_LANGS", "None")
	if got := Load().OriginalTitleLangs; got != nil {
		t.Errorf("None: %v", got)
	}
}

func TestSize(t *testing.T) {
	cases := map[string]uint64{"": 7, "500": 500 << 20, "500MB": 500 << 20, "2gb": 2 << 30, "1.5 GB": 3 << 29, "1TB": 1 << 40, "0": 0, "lots": 7,
		"inf": 7, "+Inf": 7, "NaN": 7, "-1": 7, "1e30GB": 7, "1048576TB": 1 << 60, "1048577TB": 7}
	for v, want := range cases {
		t.Setenv("X_SIZE", v)
		if got := size("X_SIZE", 7); got != want {
			t.Errorf("%q: got %d, want %d", v, got, want)
		}
	}
}

func TestListenAddr(t *testing.T) {
	cases := []struct{ listen, port, want string }{
		{"", "", ":32600"},                            // default
		{"", "32700", ":32700"},                       // PORT
		{"", "70000", ":32600"},                       // out of range: default
		{"", "abc", ":32600"},                         // not a number: default
		{"127.0.0.1:9000", "32700", "127.0.0.1:9000"}, // LISTEN wins
	}
	for _, c := range cases {
		t.Setenv("LISTEN", c.listen)
		t.Setenv("PORT", c.port)
		if got := listenAddr(); got != c.want {
			t.Errorf("LISTEN=%q PORT=%q: got %q, want %q", c.listen, c.port, got, c.want)
		}
	}
}

// warnings captures what the settings log while f runs.
func warnings(t *testing.T, f func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	f()
	return buf.String()
}

// A list with no known source left falls back to the default with a
// warning instead of turning the field off.
func TestSourceListWithNothingKnownUsesDefault(t *testing.T) {
	for _, v := range []string{"animelist", "foo,bar", ","} {
		t.Setenv("MAPPING_SOURCES", v)
		var got []string
		log := warnings(t, func() { got = Load().MappingSources })
		if !slices.Equal(got, []string{"animelists", "fribb"}) {
			t.Errorf("MAPPING_SOURCES=%q: %v, want the default", v, got)
		}
		if !strings.Contains(log, "no known source left") {
			t.Errorf("MAPPING_SOURCES=%q: no warning in %q", v, log)
		}
	}
}

// MAPPING_SOURCES can't be turned off: without a list the mapping never
// becomes ready. "none" warns and uses the default.
func TestMappingSourcesNoneUsesDefault(t *testing.T) {
	for _, v := range []string{"none", "NONE"} {
		t.Setenv("MAPPING_SOURCES", v)
		var got []string
		log := warnings(t, func() { got = Load().MappingSources })
		if !slices.Equal(got, []string{"animelists", "fribb"}) || !strings.Contains(log, "setting=MAPPING_SOURCES") {
			t.Errorf("MAPPING_SOURCES=%s: %v (log %q), want the default with a warning", v, got, log)
		}
	}
	t.Setenv("MAPPING_SOURCES", "fribb")
	if got := Load().MappingSources; !slices.Equal(got, []string{"fribb"}) {
		t.Errorf("fribb: %v", got)
	}
}

// Numbers and booleans that can't be read, or are out of range, warn and
// use the default.
func TestBadValuesWarnAndUseDefault(t *testing.T) {
	cases := []struct {
		name, value string
		get         func(Config) any
		want        any
		warns       bool
	}{
		{"MIN_SCORE", "150", func(c Config) any { return c.MinScore }, 80, true},
		{"MIN_SCORE", "-1", func(c Config) any { return c.MinScore }, 80, true},
		{"MIN_SCORE", "high", func(c Config) any { return c.MinScore }, 80, true},
		{"MIN_SCORE", "0", func(c Config) any { return c.MinScore }, 0, false},
		{"MIN_SCORE", "100", func(c Config) any { return c.MinScore }, 100, false},
		{"TITLE_OFFICIAL_ONLY", "yes", func(c Config) any { return c.TitleOfficialOnly }, false, true},
		{"TITLE_OFFICIAL_ONLY", "true", func(c Config) any { return c.TitleOfficialOnly }, true, false},
		{"ART_ALL_SOURCES", "maybe", func(c Config) any { return c.ArtAllSources }, false, true},
		{"SEASON_ART_FALLBACK", "", func(c Config) any { return c.SeasonArtFallback }, true, false},
		{"SEASON_ART_FALLBACK", "false", func(c Config) any { return c.SeasonArtFallback }, false, false},
		{"SEASON_ART_FALLBACK", "no", func(c Config) any { return c.SeasonArtFallback }, true, true},
		{"MATCH_REPORTS", "true", func(c Config) any { return c.MatchReports }, true, false},
		{"MATCH_REPORTS", "on", func(c Config) any { return c.MatchReports }, false, true},
		{"PORT", "70000", func(c Config) any { return c.Listen }, ":32600", true},
	}
	t.Setenv("LISTEN", "") // one set on this machine would win over PORT
	for _, c := range cases {
		t.Setenv(c.name, c.value)
		var got any
		log := warnings(t, func() { got = c.get(Load()) })
		if got != c.want {
			t.Errorf("%s=%s: %v, want %v", c.name, c.value, got, c.want)
		}
		if warned := strings.Contains(log, "setting="+c.name); warned != c.warns {
			t.Errorf("%s=%s: warned %v, want %v (log %q)", c.name, c.value, warned, c.warns, log)
		}
		t.Setenv(c.name, "")
	}
	t.Setenv("LOG_RETENTION_DAYS", "-3")
	var days int
	if log := warnings(t, func() { days = LogRetentionDays() }); days != 7 || !strings.Contains(log, "LOG_RETENTION_DAYS") {
		t.Errorf("LOG_RETENTION_DAYS=-3: %d (log %q), want 7 with a warning", days, log)
	}
}

// A mode works when its episode source is usable: /anidb AniDB, /tvdb
// TheTVDB, /auto either. Kizuna starts when one mode works.
func TestModes(t *testing.T) {
	cases := []struct {
		name              string
		c                 Config
		anidb, tvdb, auto bool
	}{
		{"nothing", Config{}, false, false, false},
		{"TheMovieDb and MAL only add details", Config{TMDBKey: "x", MALClientID: "x"}, false, false, false},
		{"AniDB", Config{AniDBClient: "x"}, true, false, true},
		{"TheTVDB v4", Config{TVDBKey: "x"}, false, true, true},
		{"TheTVDB legacy", Config{TVDBLegacyKey: "x"}, false, true, true},
		{"both", Config{AniDBClient: "x", TVDBKey: "x"}, true, true, true},
	}
	for _, c := range cases {
		got := [3]bool{c.c.ModeOff("anidb") == "", c.c.ModeOff("tvdb") == "", c.c.ModeOff("auto") == ""}
		if got != [3]bool{c.anidb, c.tvdb, c.auto} {
			t.Errorf("%s: anidb, tvdb, auto ready = %v, want %v", c.name, got, [3]bool{c.anidb, c.tvdb, c.auto})
		}
		if err := c.c.CheckModes(); (err == nil) != c.auto {
			t.Errorf("%s: CheckModes %v, want an error only when no mode works", c.name, err)
		}
	}
}

// sourceSettings are the source lists emptyFields checks.
var sourceSettings = []string{"TITLE_SOURCES", "SUMMARY_SOURCES", "POSTER_SOURCES", "BACKGROUND_SOURCES", "LOGO_SOURCES",
	"EPISODE_FILL_SOURCES", "EPISODE_TITLES", "GENRE_SOURCES", "CAST_SOURCES", "STUDIO_SOURCES", "CREW_SOURCES",
	"RATING_SOURCES", "DATE_SOURCES"}

// Lists keep their order; sources without a key are skipped, ANN always
// counts. A list with nothing usable left is reported, apart by whether it
// was set or left at its default; "none" isn't.
func TestUsableSources(t *testing.T) {
	unset(t, sourceSettings...)
	t.Setenv("CAST_SOURCES", "anidb,tvdb")
	c := Config{TMDBKey: "x", PosterSources: []string{"tvdb", "ann", "tmdb", "anidb"},
		BackgroundSources: []string{"tvdb"}, LogoSources: nil, CastSources: []string{"anidb", "tvdb"},
		EpisodeFillSources: []string{"tvdb", "mal"}}
	if got := c.UsableOf(c.PosterSources); !slices.Equal(got, []string{"ann", "tmdb"}) {
		t.Errorf("posters: %v", got)
	}
	set, defaults, keys := c.emptyFields()
	if !slices.Equal(set, []string{"CAST_SOURCES"}) || !slices.Equal(defaults, []string{"BACKGROUND_SOURCES", "EPISODE_FILL_SOURCES"}) ||
		!slices.Equal(keys, []string{"TVDB_API_KEY", "MAL_CLIENT_ID"}) {
		t.Errorf("empty fields: set %v, defaults %v, keys %v", set, defaults, keys)
	}
	log := warnings(t, func() { Config{AniDBClient: "x"}.LogModes() })
	for _, want := range []string{"anidb=ready", `tvdb="off: no TVDB_API_KEY`, "only shows with AniDB rules"} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in %q", want, log)
		}
	}
}

// With only ANIDB_CLIENT (a setup CheckModes allows) and every list at its
// default, the lists whose defaults need TheTVDB or TheMovieDb are no
// warning, just one info line naming the keys that would fill them. A list
// the user set to sources without a key is a warning.
func TestLogModesDefaultListsNoWarning(t *testing.T) {
	unset(t, sourceSettings...)
	unset(t, "TVDB_API_KEY", "TVDB_LEGACY_API_KEY", "TMDB_API_KEY", "MAL_CLIENT_ID")
	t.Setenv("ANIDB_CLIENT", "x")
	c := Load()
	log := warnings(t, c.LogModes)
	if strings.Contains(log, "level=WARN") {
		t.Errorf("warning for lists left at their default: %q", log)
	}
	want := `level=INFO msg="these fields stay empty: their default sources need a key" ` +
		`settings="[BACKGROUND_SOURCES LOGO_SOURCES EPISODE_FILL_SOURCES]" keys="[TVDB_API_KEY TMDB_API_KEY]"`
	if strings.Count(log, "these fields stay empty") != 1 || !strings.Contains(log, want) {
		t.Errorf("no %q (once) in %q", want, log)
	}

	t.Setenv("LOGO_SOURCES", "tmdb")
	log = warnings(t, Load().LogModes)
	if !strings.Contains(log, `level=WARN msg="no usable source in these lists`) || !strings.Contains(log, "settings=[LOGO_SOURCES]") {
		t.Errorf("LOGO_SOURCES set without a TMDB key: no warning naming it in %q", log)
	}
	if !strings.Contains(log, `settings="[BACKGROUND_SOURCES EPISODE_FILL_SOURCES]" keys="[TVDB_API_KEY TMDB_API_KEY]"`) {
		t.Errorf("the defaults' info line: %q", log)
	}
}
