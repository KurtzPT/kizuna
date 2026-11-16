package config

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// Every setting config reads has a row in SETTINGS.md's tables, and every
// row names a setting config reads (TZ is read by the Go runtime and
// SetTimeZone). The twin of TestEnvSampleMatchesSettings, for the user's
// reference page.
func TestSettingsDocMatchesSettings(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "SETTINGS.md"))
	if err != nil {
		t.Fatal(err)
	}
	var doc []string // a table row: | `NAME` | default | what it does |
	for _, m := range regexp.MustCompile("(?m)^\\| `([A-Z][A-Z0-9_]*)` \\|").FindAllStringSubmatch(string(b), -1) {
		if !slices.Contains(doc, m[1]) {
			doc = append(doc, m[1])
		}
	}
	slices.Sort(doc)

	// What is read must not depend on this machine's environment.
	for _, n := range doc {
		t.Setenv(n, "")
	}
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

	for _, n := range settings {
		if !slices.Contains(doc, n) {
			t.Errorf("%s is read but has no row in SETTINGS.md", n)
		}
	}
	for _, n := range doc {
		if !slices.Contains(settings, n) {
			t.Errorf("%s has a row in SETTINGS.md but is never read", n)
		}
	}
}
