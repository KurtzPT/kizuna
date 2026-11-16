package config

import (
	"errors"
	"io/fs"
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
	if errors.Is(err, fs.ErrNotExist) {
		// The image build runs the tests without the docs (.dockerignore
		// keeps them out, so a doc edit builds no image): ci's go job, in
		// the whole repository, is where this runs.
		t.Skip("no SETTINGS.md here (the image's build context)")
	}
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

// The settings file's own variable (envFileVar, which LoadEnvFile reads:
// TestExplicitEnvFile) has no table row: SETTINGS.md and .env.sample name it
// in their text. Neither sync test above sees it, so this one keeps the
// name in both: renamed in the code alone, the docs would point users at a
// variable Kizuna no longer reads.
func TestSettingsFileVarDocumented(t *testing.T) {
	word := regexp.MustCompile(`\b` + regexp.QuoteMeta(envFileVar) + `\b`)
	for _, name := range []string{".env.sample", "SETTINGS.md"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "..", name))
			if name == "SETTINGS.md" && errors.Is(err, fs.ErrNotExist) {
				t.Skip("no SETTINGS.md here (the image's build context)") // .env.sample is always there
			}
			if err != nil {
				t.Fatal(err)
			}
			if !word.Match(b) {
				t.Errorf("%s doesn't name %s, the variable LoadEnvFile reads", name, envFileVar)
			}
		})
	}
}
