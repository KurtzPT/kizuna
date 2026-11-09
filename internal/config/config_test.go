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
// warning instead of turning the field off; "none" still turns it off.
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
	t.Setenv("MAPPING_SOURCES", "none")
	if got := Load().MappingSources; got != nil {
		t.Errorf("none: %v", got)
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

// Kizuna needs AniDB or TheTVDB; TheMovieDb and MAL only add details,
// unless EPISODE_SOURCES puts tmdb first.
func TestCheckSources(t *testing.T) {
	if err := (Config{}).CheckSources(); err == nil || !strings.Contains(err.Error(), "ANIDB_CLIENT") {
		t.Errorf("nothing set: %v, want an error naming ANIDB_CLIENT", err)
	}
	for _, c := range []Config{{AniDBClient: "x"}, {TVDBKey: "x"}, {TVDBLegacyKey: "x"}} {
		if err := c.CheckSources(); err != nil {
			t.Errorf("%+v: %v, want nil", c, err)
		}
	}
	for _, c := range []Config{
		{TMDBKey: "x", MALClientID: "x", EpisodeSources: []string{"tvdb", "tmdb"}}, // TMDB only a fallback
		{TMDBKey: "x", EpisodeSources: nil},                                        // EPISODE_SOURCES=none
		{EpisodeSources: []string{"tmdb"}},                                         // tmdb first, but no key
	} {
		if err := c.CheckSources(); err == nil {
			t.Errorf("%+v: nil, want an error", c)
		}
	}
	// The user chose TheMovieDb's episode order: its key is enough.
	for _, eps := range [][]string{{"tmdb"}, {"tmdb", "tvdb"}} {
		if err := (Config{TMDBKey: "x", EpisodeSources: eps}).CheckSources(); err != nil {
			t.Errorf("TMDB key, EPISODE_SOURCES %v: %v, want nil", eps, err)
		}
	}
}
