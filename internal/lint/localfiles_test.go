package lint

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// noLocalFile is the failure for a committed file that cites a file the
// repository doesn't ship. The maintainer's notes and specs stay on their
// machine, so a reader of the repository can't open them.
const noLocalFile = " cites a file that is not in the repository, so a reader can't open it. " +
	"Say the rule itself in a short sentence, or link a committed document or a pull request. " +
	"A deliberate mention: `//lint:allow localfiles: <why>` on this line"

// localOnly are the maintainer's four files, kept out of the repository.
var localOnly = []string{"TODO.md", "DECISIONS.md", "CLAUDE.md", "RELEASING.md"}

// localMention matches one of localOnly, or a Markdown file under docs/, or the
// folder itself (also as ./docs/), not inside a longer path or URL.
var localMention = regexp.MustCompile(`(?:^|[^\w/.-])((?:TODO|DECISIONS|CLAUDE|RELEASING)\.md\b|(?:\./)?docs/(?:[\w.-]+/)*[\w.-]+\.md\b|(?:\./)?docs/(?:[^\w.-]|$))`)

// localAllow is `//lint:allow localfiles: <why>` with a reason.
var localAllow = regexp.MustCompile(`lint:allow localfiles:\s*\S`)

// localFileMentions reads the text files a reader of the repository can open
// and returns one message per mention of a local-only file. It looks in cmd,
// internal, .github, scripts and model, and at the root's Markdown files;
// the image build holds only cmd and internal, so a folder that is absent is
// skipped. It reads text, not Go: a comment, a workflow, a script and a model
// are all checked the same way. This file is skipped: it names the files.
// It doesn't read .gitignore: nothing under these folders is generated.
func localFileMentions(fsys fs.FS) []string {
	var out []string
	scan := func(name string) {
		src, err := fs.ReadFile(fsys, name)
		if err != nil || bytes.IndexByte(src, 0) >= 0 {
			return // unreadable or binary
		}
		for i, line := range strings.Split(string(src), "\n") {
			if m := localMention.FindStringSubmatch(line); m != nil && !localAllow.MatchString(line) {
				out = append(out, fmt.Sprintf("%s:%d: %s%s", name, i+1, m[1], noLocalFile))
			}
		}
	}
	for _, top := range []string{"cmd", "internal", ".github", "scripts", "model"} {
		_ = fs.WalkDir(fsys, top, func(name string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return nil // absent
			case d.IsDir():
				if d.Name() == ".git" {
					return fs.SkipDir
				}
			case name != "internal/lint/localfiles_test.go":
				scan(name)
			}
			return nil
		})
	}
	root, _ := fs.Glob(fsys, "*.md")
	for _, name := range root {
		if !slices.Contains(localOnly, path.Base(name)) {
			scan(name)
		}
	}
	return out
}

// TestLocalFiles: no committed file cites a local-only file.
func TestLocalFiles(t *testing.T) {
	for _, msg := range localFileMentions(os.DirFS("../..")) {
		t.Error(msg)
	}
}

// TestRulesCatchLocalFiles runs the rule on each kind of file it reads. It
// catches the five citations on dev at 11c957a: two workflow comments, one
// Go comment and two Go test comments, all of RELEASING.md or DECISIONS.md.
func TestRulesCatchLocalFiles(t *testing.T) {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	for _, c := range []struct {
		name string
		want []string // file:line of each violation
		fs   fstest.MapFS
	}{
		{"a workflow", []string{".github/workflows/v.yml:2"},
			fstest.MapFS{".github/workflows/v.yml": file("# a\n# patch (RELEASING.md, hotfix)\n")}},
		{"a Go source", []string{"internal/a/a.go:3"},
			fstest.MapFS{"internal/a/a.go": file("package a\n\n// (DECISIONS.md, \"x\")\n")}},
		{"a Go test", []string{"internal/a/a_test.go:1"},
			fstest.MapFS{"internal/a/a_test.go": file("// see TODO.md\n")}},
		{"a command", []string{"cmd/k/main.go:1"},
			fstest.MapFS{"cmd/k/main.go": file("// CLAUDE.md says so\n")}},
		{"a script", []string{"scripts/run.sh:2"},
			fstest.MapFS{"scripts/run.sh": file("#!/bin/sh\n# see docs/plan.md\n")}},
		{"a model", []string{"model/m.tla:1"},
			fstest.MapFS{"model/m.tla": file("(* DECISIONS.md *)\n")}},
		{"a root document", []string{"GUIDE.md:1"},
			fstest.MapFS{"GUIDE.md": file("Read RELEASING.md first.\n")}},
		{"two on one file", []string{"internal/a/a.go:1", "internal/a/a.go:2"},
			fstest.MapFS{"internal/a/a.go": file("// TODO.md\n// docs/x.md\n")}},
		{"a dot-slash path", []string{"scripts/run.sh:1"},
			fstest.MapFS{"scripts/run.sh": file("# see ./docs/x.md\n")}},
		{"a nested path", []string{"scripts/run.sh:1"},
			fstest.MapFS{"scripts/run.sh": file("# see docs/sub/x.md\n")}},
		{"a bare folder", []string{"scripts/run.sh:1"},
			fstest.MapFS{"scripts/run.sh": file("# kept in docs/\n")}},
		{"a local file itself", nil,
			fstest.MapFS{"CLAUDE.md": file("TODO.md DECISIONS.md\n"), "docs/x.md": file("TODO.md\n")}},
		{"a binary", nil,
			fstest.MapFS{"internal/a/b.bin": file("TODO.md\x00")}},
		{"a longer path", nil,
			fstest.MapFS{"internal/a/a.go": file("// https://x.io/a/docs/b.md and https://x.io/TODO.md and MYTODO.md\n")}},
		{"allowed", nil,
			fstest.MapFS{"internal/a/a.go": file("// TODO.md //lint:allow localfiles: it is the file's name\n")}},
		{"allowed without a reason", []string{"internal/a/a.go:1"},
			fstest.MapFS{"internal/a/a.go": file("// TODO.md //lint:allow localfiles:\n")}},
		{"a folder that is absent", nil, fstest.MapFS{"cmd/k/main.go": file("package main\n")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, msg := range localFileMentions(c.fs) {
				got = append(got, strings.SplitN(msg, ": ", 2)[0])
				if !strings.Contains(msg, "link a committed document or a pull request") {
					t.Errorf("the failure doesn't name the fix: %q", msg)
				}
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
