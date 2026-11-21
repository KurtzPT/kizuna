package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // zone names without the machine's zone database, as in main
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
	path := filepath.Join(dir, envFileName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	unset(t, envFileVar)
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
	var texts []string
	for _, p := range problems {
		texts = append(texts, p.text(func(string) bool { return true }))
	}
	for _, p := range []string{"line 11: no '='", "line 12: no setting name before '='", "line 13: H: missing closing quote",
		"line 14: I: text after", "line 15: KIZUNA_CONFIG: only works in the environment"} {
		if !slices.ContainsFunc(texts, func(s string) bool { return strings.HasPrefix(s, p) }) {
			t.Errorf("no problem %q in %q", p, texts)
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
	unset(t, envFileVar)
	t.Setenv("DATA_DIR", t.TempDir())
	f, err := LoadEnvFile()
	if err != nil || f.Path != "" || f.NotFound != filepath.Join(os.Getenv("DATA_DIR"), envFileName) {
		t.Errorf("no default file: %+v, %v; want NotFound and no error", f, err)
	}
	if log := warnings(t, f.Log); !strings.Contains(log, "no settings file") || !strings.Contains(log, envFileName) {
		t.Errorf("no default file: log %q, want a line naming the file", log)
	}
	t.Setenv(envFileVar, filepath.Join(t.TempDir(), "missing.env"))
	if _, err := LoadEnvFile(); err == nil || !strings.Contains(err.Error(), envFileVar) {
		t.Errorf("missing %s file: %v, want an error naming it", envFileVar, err)
	}
}

// KIZUNA_CONFIG points somewhere else than the data folder.
func TestExplicitEnvFile(t *testing.T) {
	writeEnvFile(t, "MIN_SCORE=10\n") // in DATA_DIR: must not be read
	other := filepath.Join(t.TempDir(), "other.env")
	if err := os.WriteFile(other, []byte("MIN_SCORE=20\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envFileVar, other)
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
	for _, want := range []string{`unknown setting (a typo?); ignoring it" path=`, "line=2", "no '='", "settings=3"} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in %q", want, log)
		}
	}
	for _, bad := range []string{"secret-key", "TITEL_LANGS", "line=3", "line=1"} {
		if strings.Contains(log, bad) {
			t.Errorf("%q in %q", bad, log)
		}
	}
}

// A line that isn't NAME=value may hold a key, as in a YAML-style line. Its
// warning gives the line number and, at most, the setting name it starts
// with when Kizuna knows that setting; never any other text of the line.
func TestBrokenLineNeverShowsItsText(t *testing.T) {
	writeEnvFile(t, "TMDB_API_KEY: abc123secret=x\n"+ // YAML style, an '=' in the value
		"MAL_CLIENT_ID: def456secret\n"+ // YAML style, no '='
		"ABC123SECRET: more=x\n"+ // starts like a name, but no setting has it
		"ghi789secret\n"+
		"jkl012 secret=x\n"+
		"TVDB-PIN=mno345secret\n")
	unset(t, "TMDB_API_KEY", "MAL_CLIENT_ID")
	f, err := LoadEnvFile()
	if err != nil {
		t.Fatal(err)
	}
	log := warnings(t, func() { Load(); f.Log() })
	for _, want := range []string{
		`problem="line 1: TMDB_API_KEY: no setting name before '='`,
		`problem="line 2: MAL_CLIENT_ID: no '='`,
		`problem="line 3: no setting name before '='`,
		`problem="line 4: no '='`,
		`problem="line 5: no setting name before '='`,
		`problem="line 6: no setting name before '='`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in %q", want, log)
		}
	}
	for _, bad := range []string{"secret", "ABC123", "jkl", "TVDB-PIN"} {
		if strings.Contains(log, bad) {
			t.Errorf("%q in %q", bad, log)
		}
	}
}

// A pasted key that ends in '=' (base64) reads as NAME=value. Every
// warning about such a line gives its number, never the text before '=':
// an unknown name, a name not in upper case, a missing closing quote. A
// setting Kizuna knows is still named (TestLowerCaseKeyIgnored: also one
// written in lower case).
func TestNameLikeKeyNeverShown(t *testing.T) {
	writeEnvFile(t, "QUJDREVGR0VGR0Q=\n"+ // upper case: an unknown setting
		"c2VjcmV0LXNlY3JldA==\n"+ // lower case
		"ZGVmc2VjcmV0=\"unclosed\n"+ // missing closing quote
		"ZGVmc2VjcmV0Mg=\"x\" y\n"+ // text after the closing quote
		"TMDB_API_KEY=\"unclosed\n")
	unset(t, "QUJDREVGR0VGR0Q", "c2VjcmV0LXNlY3JldA", "TMDB_API_KEY")
	f, err := LoadEnvFile()
	if err != nil {
		t.Fatal(err)
	}
	log := warnings(t, func() { Load(); f.Log() })
	for _, want := range []string{
		`unknown setting (a typo?); ignoring it" path=`, "line=1",
		`unknown setting (names must be upper case); ignoring it" path=`, "line=2",
		`problem="line 3: missing closing quote"`,
		`problem="line 4: text after the closing quote"`,
		`problem="line 5: TMDB_API_KEY: missing closing quote"`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in %q", want, log)
		}
	}
	// Not in upper case either: a lower-case name is named only when its
	// upper-case form is a setting Kizuna knows.
	for _, bad := range []string{"QUJD", "c2Vj", "C2VJ", "ZGVm", "setting="} {
		if strings.Contains(log, bad) {
			t.Errorf("%q in %q", bad, log)
		}
	}
}

// Only exact upper-case names count, on every OS: "min_score" must not set
// MIN_SCORE (Windows env names ignore case). Its warning names MIN_SCORE, the
// name to write, as Kizuna knows that setting; "Max_Episodes" is no setting
// in any case, so its warning gives the line alone. Neither shows the name as
// written, nor a value. "mapping_sources", a retired setting, is silent, as
// MAPPING_SOURCES is: written in upper case it would do nothing either.
func TestLowerCaseKeyIgnored(t *testing.T) {
	writeEnvFile(t, "min_score=score-value\nMax_Episodes=9\nTVDB_API_KEY=secret-key\nmapping_sources=x\n")
	unset(t, "MIN_SCORE", "min_score", "Max_Episodes", "TVDB_API_KEY", "mapping_sources", "MAPPING_SOURCES")
	f, err := LoadEnvFile()
	if err != nil {
		t.Fatal(err)
	}
	if _, set := os.LookupEnv("MIN_SCORE"); set {
		t.Errorf("min_score set MIN_SCORE to %q", os.Getenv("MIN_SCORE"))
	}
	if !slices.Equal(f.Keys, []string{"TVDB_API_KEY"}) || !slices.Equal(f.Ignored, []string{"min_score", "Max_Episodes", "mapping_sources"}) {
		t.Errorf("keys %v ignored %v", f.Keys, f.Ignored)
	}
	log := warnings(t, func() { Load(); f.Log() })
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)setting not in upper case; ignoring it \(write the name in upper case\)" path=.+ line=1 setting=MIN_SCORE\r?$`),
		regexp.MustCompile(`(?m)unknown setting \(names must be upper case\); ignoring it" path=.+ line=2\r?$`),
		regexp.MustCompile(`settings=1`),
	} {
		if !want.MatchString(log) {
			t.Errorf("no %s in %q", want, log)
		}
	}
	for _, bad := range []string{"score-value", "secret-key", "min_score", "Max_Episodes", "MAX_EPISODES", "mapping_sources", "MAPPING_SOURCES", "line=4"} {
		if strings.Contains(log, bad) {
			t.Errorf("%q in %q", bad, log)
		}
	}
}

// forgetRead starts the test with no setting names read (restored after).
func forgetRead(t *testing.T) {
	read.Lock()
	saved := read.names
	read.names = map[string]bool{}
	read.Unlock()
	t.Cleanup(func() {
		read.Lock()
		read.names = saved
		read.Unlock()
	})
}

// PORT in the file is a real setting, not a typo, also when LISTEN in the
// environment wins over it.
func TestPortUnderListenNotUnknown(t *testing.T) {
	writeEnvFile(t, "PORT=32700\n")
	unset(t, "PORT")
	t.Setenv("LISTEN", "127.0.0.1:32600")
	forgetRead(t)
	f, err := LoadEnvFile()
	if err != nil {
		t.Fatal(err)
	}
	if log := warnings(t, func() { Load(); f.Log() }); strings.Contains(log, "unknown setting") { // PORT is the file's only line
		t.Errorf("PORT reported as unknown: %q", log)
	}
}

// MAPPING_SOURCES is no setting any more (the two lists have one order). One
// left in the environment, in the settings file, or in both changes nothing
// and is never named in a warning: not as a bad value, not as a typo.
func TestLeftoverMappingSourcesIgnored(t *testing.T) {
	writeEnvFile(t, "MIN_SCORE=70\n")
	unset(t, "MIN_SCORE", "MAPPING_SOURCES")
	forgetRead(t)
	if _, err := LoadEnvFile(); err != nil {
		t.Fatal(err)
	}
	want := Load() // without it

	for _, c := range []struct{ name, env, file string }{
		{"environment", "fribb", ""},
		{"environment, no list", "none", ""},
		{"settings file", "", "MAPPING_SOURCES=fribb,animelists\n"},
		{"both", "fribb", "MAPPING_SOURCES=nonsense\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			writeEnvFile(t, "MIN_SCORE=70\n"+c.file)
			unset(t, "MIN_SCORE", "MAPPING_SOURCES")
			if c.env != "" {
				t.Setenv("MAPPING_SOURCES", c.env)
			}
			forgetRead(t)
			f, err := LoadEnvFile()
			if err != nil {
				t.Fatal(err)
			}
			var got Config
			log := warnings(t, func() { got = Load(); f.Log() })
			want := want
			want.DataDir = got.DataDir // each case has its own folder
			if !reflect.DeepEqual(got, want) || got.MinScore != 70 {
				t.Errorf("settings changed:\n got %+v\nwant %+v", got, want)
			}
			for _, line := range strings.Split(log, "\n") {
				// The typo warning gives a line, not the name: any of them is about it.
				if strings.Contains(line, "level=WARN") &&
					(strings.Contains(line, "MAPPING_SOURCES") || strings.Contains(line, "unknown setting")) {
					t.Errorf("warning about it: %s", line)
				}
			}
			if len(f.Problems) > 0 || len(f.Ignored) > 0 {
				t.Errorf("problems %v, ignored %v", f.Problems, f.Ignored)
			}
		})
	}
}

// Every setting config reads is in .env.sample, and the sample lists no
// setting config doesn't read (TZ is read by the Go runtime).
func TestEnvSampleMatchesSettings(t *testing.T) {
	forgetRead(t)
	t.Setenv("LISTEN", "127.0.0.1:32600") // PORT counts as read all the same
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

// keepLocal restores time.Local after the test.
func keepLocal(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })
}

// TZ sets the local zone on every OS (Go itself ignores it on Windows),
// also from the settings file. An unknown zone is an error and changes
// nothing; so does an empty TZ.
func TestSetTimeZone(t *testing.T) {
	keepLocal(t)
	writeEnvFile(t, "TZ=Asia/Tokyo\n")
	unset(t, "TZ")
	if _, err := LoadEnvFile(); err != nil {
		t.Fatal(err)
	}
	if err := SetTimeZone(); err != nil {
		t.Fatal(err)
	}
	noon := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if got := noon.In(time.Local); time.Local.String() != "Asia/Tokyo" || got.Hour() != 21 {
		t.Errorf("TZ=Asia/Tokyo: local zone %s, 12:00 UTC is %s", time.Local, got)
	}

	before := time.Local
	for _, tz := range []string{"Mars/Olympus_Mons", "   "} {
		t.Setenv("TZ", tz)
		err := SetTimeZone()
		if (err != nil) != (tz != "   ") || time.Local != before {
			t.Errorf("TZ=%q: %v, local zone %s; want it unchanged (an error only for the unknown zone)", tz, err, time.Local)
		}
		if err != nil && !strings.Contains(err.Error(), "TZ") {
			t.Errorf("unclear error: %v", err)
		}
	}

	// A path: Go reads it itself on Linux and macOS; Windows can't use it.
	t.Setenv("TZ", "/etc/localtime")
	if err := SetTimeZone(); (err != nil) != (runtime.GOOS == "windows") || time.Local != before {
		t.Errorf("TZ=/etc/localtime on %s: %v, local zone %s", runtime.GOOS, err, time.Local)
	}
}
