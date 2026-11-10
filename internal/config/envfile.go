package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// envFileName is the settings file Kizuna looks for in the data folder.
const envFileName = "kizuna.env"

// envFileVar names a settings file to use instead of <DATA_DIR>/kizuna.env.
// It only works in the environment, not inside the file.
const envFileVar = "KIZUNA_CONFIG"

// EnvFile is what LoadEnvFile did, for the startup log. It never holds
// values: the file has API keys.
type EnvFile struct {
	Path       string   // the file read; "" = none
	NotFound   string   // the default file looked for, when it isn't there
	Keys       []string // settings taken from the file
	Overridden []string // in the file, but the environment sets them (the environment wins)
	Ignored    []string // names that aren't all upper case: never set, on any OS
	Problems   []string // lines that were skipped, and why
}

// LoadEnvFile reads the settings file (KIZUNA_CONFIG, else
// <DATA_DIR>/kizuna.env) and copies each setting into the environment unless
// the environment already has it, even as "": Docker and Compose variables
// always win over the file. A missing default file is fine; a missing
// KIZUNA_CONFIG file, or one that can't be read, is an error.
//
// Call it before anything reads a setting. DATA_DIR set in the file still
// moves the cache and logs, but not where this file is looked for.
func LoadEnvFile() (EnvFile, error) {
	path, explicit := env(envFileVar, ""), true
	if path == "" {
		path, explicit = filepath.Join(DataDir(), envFileName), false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !explicit && errors.Is(err, fs.ErrNotExist) {
			return EnvFile{NotFound: path}, nil
		}
		if explicit {
			return EnvFile{}, fmt.Errorf("can't read the settings file (%s): %w", envFileVar, err)
		}
		return EnvFile{}, fmt.Errorf("can't read the settings file: %w", err)
	}
	f := EnvFile{Path: path}
	settings, problems := parseEnvFile(data)
	f.Problems = problems
	for _, s := range settings {
		// Only exact upper-case names count. Windows env names ignore case,
		// so setting "port" there would quietly set PORT.
		if s.key != strings.ToUpper(s.key) {
			f.Ignored = append(f.Ignored, s.key)
			continue
		}
		if _, set := os.LookupEnv(s.key); set {
			f.Overridden = append(f.Overridden, s.key)
			continue
		}
		if err := os.Setenv(s.key, s.value); err != nil {
			f.Problems = append(f.Problems, fmt.Sprintf("line %d: %s: %v", s.line, s.key, err))
			continue
		}
		f.Keys = append(f.Keys, s.key)
	}
	return f, nil
}

// Log reports the file at startup: which file (or that there is none), the
// settings the environment overrode, skipped lines and names Kizuna doesn't
// know (typos). Call it after Load, which marks the names Kizuna reads.
func (f EnvFile) Log() {
	if f.NotFound != "" {
		slog.Info("no settings file; using environment variables and defaults", "path", f.NotFound)
		return
	}
	if f.Path == "" {
		return
	}
	slog.Info("settings file", "path", f.Path, "settings", len(f.Keys), "overridden_by_environment", f.Overridden)
	for _, p := range f.Problems {
		slog.Warn("settings file: skipped a line", "path", f.Path, "problem", p)
	}
	for _, k := range slices.Concat(f.Keys, f.Overridden) {
		if !wasRead(k) && !slices.Contains(externalVars, k) {
			slog.Warn("settings file: unknown setting (a typo?); ignoring it", "path", f.Path, "setting", k)
		}
	}
	for _, k := range f.Ignored {
		slog.Warn("settings file: unknown setting (names must be upper case); ignoring it", "path", f.Path, "setting", k)
	}
}

// externalVars are read outside Load: TZ by SetTimeZone (and the Go runtime).
var externalVars = []string{"TZ"}

// SetTimeZone makes TZ (e.g. "Europe/London") the local time zone, on every
// OS: Go reads TZ itself only on Linux and macOS, while on Windows the zone
// always comes from the system, so TZ there would do nothing. Call it after
// LoadEnvFile (TZ may be in the file) and before the log file is opened (its
// days follow local time). An unknown zone is an error to log, and time.Local
// is left alone: the system's zone on Windows, UTC on Linux and macOS (the Go
// runtime's fallback). Zone names need the zone database: main embeds it
// (time/tzdata), as Windows and the distroless image have none.
func SetTimeZone() error {
	tz := strings.TrimSpace(os.Getenv("TZ"))
	if tz == "" {
		return nil
	}
	if runtime.GOOS != "windows" && (strings.HasPrefix(tz, ":") || strings.HasPrefix(tz, "/")) {
		return nil // a file path or the ":name" form: the Go runtime reads those itself
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("unknown time zone in TZ (want a name such as Europe/London): %w", err)
	}
	time.Local = loc
	return nil
}

// read holds every setting name env was asked for, so Log can tell typos
// from real settings.
var read = struct {
	sync.Mutex
	names map[string]bool
}{names: map[string]bool{}}

func markRead(name string) {
	read.Lock()
	read.names[name] = true
	read.Unlock()
}

func wasRead(name string) bool {
	read.Lock()
	defer read.Unlock()
	return read.names[name]
}

type envSetting struct {
	key, value string
	line       int
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseEnvFile reads KEY=value lines, the way a shell sources them for the
// common cases: blank lines and # comments are skipped, "export " is allowed,
// values may be in single or double quotes (taken literally, no escapes),
// and an unquoted value ends at " #". A later line for the same key wins.
func parseEnvFile(data []byte) (out []envSetting, problems []string) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // BOM from Windows editors
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text()) // also drops the \r of CRLF files
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, raw, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("line %d: no '=' (want KEY=value)", n))
			continue
		case !envKey.MatchString(key):
			problems = append(problems, fmt.Sprintf("line %d: %q is not a setting name", n, key))
			continue
		case key == envFileVar:
			problems = append(problems, fmt.Sprintf("line %d: %s only works in the environment", n, envFileVar))
			continue
		}
		value, err := envValue(raw)
		if err != nil {
			problems = append(problems, fmt.Sprintf("line %d: %s: %v", n, key, err))
			continue
		}
		out = slices.DeleteFunc(out, func(s envSetting) bool { return s.key == key })
		out = append(out, envSetting{key: key, value: value, line: n})
	}
	if err := sc.Err(); err != nil {
		problems = append(problems, "stopped reading: "+err.Error())
	}
	return out, problems
}

// envValue reads the text after "=". An unquoted value ends at a # that
// follows a space or tab, so "KEY=   # note" is empty, like in a shell.
func envValue(raw string) (string, error) {
	if t := strings.TrimSpace(raw); t != "" && (t[0] == '"' || t[0] == '\'') {
		end := strings.IndexByte(t[1:], t[0])
		if end < 0 {
			return "", errors.New("missing closing quote")
		}
		if rest := strings.TrimSpace(t[end+2:]); rest != "" && !strings.HasPrefix(rest, "#") {
			return "", errors.New("text after the closing quote")
		}
		return t[1 : end+1], nil
	}
	if i := strings.Index(raw, " #"); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.Index(raw, "\t#"); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw), nil
}
