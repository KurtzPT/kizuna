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

func TestSize(t *testing.T) {
	cases := map[string]uint64{"": 7, "500": 500 << 20, "500MB": 500 << 20, "2gb": 2 << 30, "1.5 GB": 3 << 29, "1TB": 1 << 40, "0": 0, "lots": 7}
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
		{"MATCH_REPORTS", "true", func(c Config) any { return c.MatchReports }, true, false},
		{"MATCH_REPORTS", "on", func(c Config) any { return c.MatchReports }, false, true},
		{"PORT", "70000", func(c Config) any { return c.Listen }, ":32600", true},
	}
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

// Lists keep their order; sources without a key are skipped, ANN always
// counts. A list with nothing usable left is reported; "none" isn't.
func TestUsableSources(t *testing.T) {
	c := Config{TMDBKey: "x", PosterSources: []string{"tvdb", "ann", "tmdb", "anidb"},
		BackgroundSources: []string{"tvdb"}, LogoSources: nil, CastSources: []string{"anidb", "tvdb"}}
	if got := c.UsableOf(c.PosterSources); !slices.Equal(got, []string{"ann", "tmdb"}) {
		t.Errorf("posters: %v", got)
	}
	if got := c.emptyFields(); !slices.Equal(got, []string{"BACKGROUND_SOURCES", "CAST_SOURCES"}) {
		t.Errorf("empty fields: %v", got)
	}
	log := warnings(t, func() { Config{AniDBClient: "x"}.LogModes() })
	for _, want := range []string{"anidb=ready", `tvdb="off: no TVDB_API_KEY`, "only shows with AniDB rules"} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in %q", want, log)
		}
	}
}
