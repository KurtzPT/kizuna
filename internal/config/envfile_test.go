package config

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// unset clears names for this test and restores them afterwards, so values
// LoadEnvFile copies into the environment don't leak into other tests.
func unset(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		t.Setenv(n, "")
		if err := os.Unsetenv(n); err != nil {
			t.Fatal(err)
		}
	}
}

// writeEnvFile writes <dir>/kizuna.env and points DATA_DIR at dir.
func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, EnvFileName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	unset(t, EnvFileVar)
	t.Setenv("DATA_DIR", dir)
	return path
}

func TestParseEnvFile(t *testing.T) {
	in := "\xef\xbb\xbf# comment\r\n" +
		"\n" +
		"A=plain\r\n" +
		"export B = spaced value \n" +
		"C=\"double # not a comment\"  # comment\n" +
		"D='single'\n" +
		"E=value # comment\n" +
		"F=\n" +
		"G=a=b\n" +
		"A=later wins\n" +
		"no equals sign\n" +
		"1BAD=x\n" +
		"H=\"unclosed\n" +
		"I=\"x\" y\n" +
		"KIZUNA_CONFIG=/elsewhere\n" +
		"J=   # only a comment\n" +
		"K=\t# only a comment\n" +
		"L=value\t# tab comment\n" +
		"M='single # not a comment' # comment\n" +
		"N=#not a comment\n" +
		"O= \"quoted\"\t# comment\n"
	got, problems := parseEnvFile([]byte(in))
	want := map[string]string{"A": "later wins", "B": "spaced value", "C": "double # not a comment", "D": "single",
		"E": "value", "F": "", "G": "a=b",
		"J": "", "K": "", "L": "value", "M": "single # not a comment", "N": "#not a comment", "O": "quoted"}
	if len(got) != len(want) {
		t.Errorf("got %d settings %v, want %d", len(got), got, len(want))
	}
	for _, s := range got {
		if w, ok := want[s.key]; !ok || s.value != w {
			t.Errorf("%s=%q, want %q (known %v)", s.key, s.value, w, ok)
		}
	}
	for _, p := range []string{"line 11: no '='", `line 12: "1BAD"`, "line 13: H: missing closing quote",
		"line 14: I: text after", "line 15: KIZUNA_CONFIG only works in the environment"} {
		if !slices.ContainsFunc(problems, func(s string) bool { return strings.HasPrefix(s, p) }) {
			t.Errorf("no problem %q in %q", p, problems)
		}
	}
}

// The environment wins over the file, even when it sets a variable to "":
// in Compose, TVDB_API_KEY: "" turns TVDB off whatever the file says.
func TestEnvironmentWinsOverFile(t *testing.T) {
	writeEnvFile(t, "TVDB_API_KEY=from-file\nTMDB_API_KEY=from-file\nMIN_SCORE=60\n")
	unset(t, "MIN_SCORE")
	t.Setenv("TVDB_API_KEY", "")
	t.Setenv("TMDB_API_KEY", "from-env")
	f, err := LoadEnvFile()
	if err != nil {
		t.Fatal(err)
	}
	c := Load()
	if c.TVDBKey != "" || c.TMDBKey != "from-env" || c.MinScore != 60 {
		t.Errorf("tvdb %q tmdb %q min score %d", c.TVDBKey, c.TMDBKey, c.MinScore)
	}
	if !slices.Equal(f.Keys, []string{"MIN_SCORE"}) || !slices.Equal(f.Overridden, []string{"TVDB_API_KEY", "TMDB_API_KEY"}) {
		t.Errorf("keys %v overridden %v", f.Keys, f.Overridden)
	}
}

// Settings read before Load, like LOG_LEVEL and LOG_RETENTION_DAYS, work
// from the file too.
func TestEarlySettingsFromFile(t *testing.T) {
	writeEnvFile(t, "LOG_LEVEL=debug\nLOG_RETENTION_DAYS=3\n")
	unset(t, "LOG_LEVEL", "LOG_RETENTION_DAYS")
	if _, err := LoadEnvFile(); err != nil {
		t.Fatal(err)
	}
	if LogLevel() != "debug" || LogRetentionDays() != 3 {
		t.Errorf("LOG_LEVEL %q, LOG_RETENTION_DAYS %d", LogLevel(), LogRetentionDays())
	}
}

func TestMissingEnvFile(t *testing.T) {
	unset(t, EnvFileVar)
	t.Setenv("DATA_DIR", t.TempDir())
	f, err := LoadEnvFile()
	if err != nil || f.Path != "" || f.NotFound != filepath.Join(os.Getenv("DATA_DIR"), EnvFileName) {
		t.Errorf("no default file: %+v, %v; want NotFound and no error", f, err)
	}
	if log := warnings(t, f.Log); !strings.Contains(log, "no settings file") || !strings.Contains(log, EnvFileName) {
		t.Errorf("no default file: log %q, want a line naming the file", log)
	}
	t.Setenv(EnvFileVar, filepath.Join(t.TempDir(), "missing.env"))
	if _, err := LoadEnvFile(); err == nil || !strings.Contains(err.Error(), EnvFileVar) {
		t.Errorf("missing %s file: %v, want an error naming it", EnvFileVar, err)
	}
}

// KIZUNA_CONFIG points somewhere else than the data folder.
func TestExplicitEnvFile(t *testing.T) {
	writeEnvFile(t, "MIN_SCORE=10\n") // in DATA_DIR: must not be read
	other := filepath.Join(t.TempDir(), "other.env")
	if err := os.WriteFile(other, []byte("MIN_SCORE=20\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvFileVar, other)
	unset(t, "MIN_SCORE")
	f, err := LoadEnvFile()
	if err != nil || f.Path != other || Load().MinScore != 20 {
		t.Errorf("%+v, %v, MIN_SCORE %d; want %s with 20", f, err, Load().MinScore, other)
	}
}

// Log names typos and skipped lines, never values.
func TestEnvFileLog(t *testing.T) {
	writeEnvFile(t, "TVDB_API_KEY=secret-key\nTITEL_LANGS=en\nTZ=Europe/London\nbroken\n")
	unset(t, "TVDB_API_KEY", "TITEL_LANGS", "TZ")
	f, err := LoadEnvFile()
	if err != nil {
		t.Fatal(err)
	}
	log := warnings(t, func() { Load(); f.Log() })
	for _, want := range []string{"setting=TITEL_LANGS", "no '='", "settings=3"} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in %q", want, log)
		}
	}
	for _, bad := range []string{"secret-key", "setting=TZ", "setting=TVDB_API_KEY"} {
		if strings.Contains(log, bad) {
			t.Errorf("%q in %q", bad, log)
		}
	}
}

// Only exact upper-case names count, on every OS: "min_score" must not set
// MIN_SCORE (Windows env names ignore case) and is reported as unknown.
func TestLowerCaseKeyIgnored(t *testing.T) {
	writeEnvFile(t, "min_score=55\nMax_Episodes=9\nTVDB_API_KEY=secret-key\n")
	unset(t, "MIN_SCORE", "min_score", "Max_Episodes", "TVDB_API_KEY")
	f, err := LoadEnvFile()
	if err != nil {
		t.Fatal(err)
	}
	if _, set := os.LookupEnv("MIN_SCORE"); set {
		t.Errorf("min_score set MIN_SCORE to %q", os.Getenv("MIN_SCORE"))
	}
	if !slices.Equal(f.Keys, []string{"TVDB_API_KEY"}) || !slices.Equal(f.Ignored, []string{"min_score", "Max_Episodes"}) {
		t.Errorf("keys %v ignored %v", f.Keys, f.Ignored)
	}
	log := warnings(t, func() { Load(); f.Log() })
	for _, want := range []string{"setting=min_score", "setting=Max_Episodes", "settings=1"} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in %q", want, log)
		}
	}
	for _, bad := range []string{"55", "secret-key"} {
		if strings.Contains(log, bad) {
			t.Errorf("%q in %q", bad, log)
		}
	}
}

// Every setting config reads is in .env.sample, and the sample lists no
// setting config doesn't read (TZ is read by the Go runtime).
func TestEnvSampleMatchesSettings(t *testing.T) {
	read.Lock()
	saved := read.names
	read.names = map[string]bool{}
	read.Unlock()
	t.Cleanup(func() {
		read.Lock()
		read.names = saved
		read.Unlock()
	})
	Load()
	DataDir()
	LogLevel()
	LogRetentionDays()
	read.Lock()
	var settings []string
	for n := range read.names {
		settings = append(settings, n)
	}
	read.Unlock()
	settings = append(settings, externalVars...)
	slices.Sort(settings)

	b, err := os.ReadFile(filepath.Join("..", "..", ".env.sample"))
	if err != nil {
		t.Fatal(err)
	}
	var sample []string
	for _, m := range regexp.MustCompile(`(?m)^(?:# ?)?([A-Z][A-Z0-9_]*)=`).FindAllStringSubmatch(string(b), -1) {
		if !slices.Contains(sample, m[1]) {
			sample = append(sample, m[1])
		}
	}
	slices.Sort(sample)
	for _, n := range settings {
		if !slices.Contains(sample, n) {
			t.Errorf("%s is read but not in .env.sample", n)
		}
	}
	for _, n := range sample {
		if !slices.Contains(settings, n) {
			t.Errorf("%s is in .env.sample but never read", n)
		}
	}
}
