// Package lint holds the repository's own rules: tests that type-check the
// Go files under cmd/ and internal/ and fail with a message that names the
// fix. They run with `go test ./...`, in CI and in the image build. Test
// files are parsed but not type-checked: only a syntactic rule reads them.
//
// To add a rule: write a func(*pkg) []string that returns one message per
// violation (use p.at for the "file:line:" prefix), in a file of its own
// beside its message and a TestRulesCatch<Rule> that shows the faulty shape
// failing, and add it to rules.
// A deliberate exception is a comment on the offending line:
// `//lint:allow <rule>: <why>`.
// One rule reads text, not Go: localfiles (localfiles_test.go) checks the
// committed text files for a citation of a file the repository doesn't ship.
package lint

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// module is a module's packages, type-checked together: a call resolves to
// the function it calls, in whichever package that is declared.
type module struct {
	goos  string // the build: linux, or windows for the _windows.go files
	fset  *token.FileSet
	info  *types.Info // of every package
	funcs map[*types.Func]*ast.FuncDecl
	pkgs  map[string]*pkg // by import path
}

// pkg is one package: the files one GOOS's build compiles, type-checked,
// and its test files, parsed only (type-checking them would build every
// test-only import).
type pkg struct {
	*module
	files []*ast.File
	tests []*ast.File
	types *types.Package
}

const modulePath = "kizuna"

// goosList are the builds the rules run on: the image's, and Windows, where
// the _windows.go files (the rename retry, the disk space) are compiled.
var goosList = []string{"linux", "windows"}

// setBuild sets c to goos/amd64 without cgo, so a rule's verdict doesn't
// depend on where the test runs.
func setBuild(c *build.Context, goos string) {
	c.GOOS, c.GOARCH, c.CgoEnabled = goos, "amd64", false
}

// std imports the standard library from source, one importer per GOOS. The
// source importer reads build.Default and isn't safe for concurrent use, so
// this package's tests don't call t.Parallel.
var std = func() map[string]types.Importer {
	out := map[string]types.Importer{}
	for _, goos := range goosList {
		setBuild(&build.Default, goos)
		src := importer.ForCompiler(token.NewFileSet(), "source", nil)
		out[goos] = importerFunc(func(path string) (*types.Package, error) {
			setBuild(&build.Default, goos)
			return src.Import(path)
		})
	}
	return out
}()

func newModule(goos string) *module {
	return &module{
		goos: goos,
		fset: token.NewFileSet(),
		info: &types.Info{
			Defs:       map[*ast.Ident]types.Object{},
			Uses:       map[*ast.Ident]types.Object{},
			Selections: map[*ast.SelectorExpr]*types.Selection{},
			Types:      map[ast.Expr]types.TypeAndValue{},
		},
		funcs: map[*types.Func]*ast.FuncDecl{},
		pkgs:  map[string]*pkg{},
	}
}

// parse adds the file name (slash-separated, relative to the module root)
// to its directory's package.
func (m *module) parse(name string, src []byte) error {
	f, err := parser.ParseFile(m.fset, name, src, parser.ParseComments)
	if err != nil {
		return err
	}
	dir := path.Dir(name)
	p := m.pkgs[modulePath+"/"+dir]
	if p == nil {
		p = &pkg{module: m}
		m.pkgs[modulePath+"/"+dir] = p
	}
	ctx := build.Default
	setBuild(&ctx, m.goos)
	ctx.OpenFile = func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(src)), nil }
	match, err := ctx.MatchFile(dir, path.Base(name))
	switch {
	case !match:
	case strings.HasSuffix(name, "_test.go"):
		p.tests = append(p.tests, f)
	default:
		p.files = append(p.files, f)
	}
	return err
}

// check type-checks every package, each after the ones it imports, and
// fails on a type error: a rule can't trust what it then resolves.
func (m *module) check(t *testing.T) {
	t.Helper()
	var errs []error
	conf := types.Config{Error: func(err error) { errs = append(errs, err) }}
	conf.Importer = importerFunc(func(path string) (*types.Package, error) {
		p := m.pkgs[path]
		if p == nil {
			return std[m.goos].Import(path)
		}
		if p.types == nil {
			p.types, _ = conf.Check(path, m.fset, p.files, m.info)
		}
		return p.types, nil
	})
	for _, path := range slices.Sorted(maps.Keys(m.pkgs)) {
		if len(m.pkgs[path].files) > 0 {
			_, _ = conf.Importer.Import(path)
		}
	}
	if len(errs) > 0 {
		t.Fatalf("type-checking failed (%d errors), first: %v", len(errs), errs[0])
	}
	for _, p := range m.pkgs {
		for _, f := range p.files {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
					m.funcs[m.info.Defs[fd.Name].(*types.Func)] = fd
				}
			}
		}
	}
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// repo is the repository's own module, per GOOS, read once for all tests.
var repo = map[string]*module{}

// loadRepo returns the repository's packages for goos.
func loadRepo(t *testing.T, goos string) *module {
	t.Helper()
	if repo[goos] == nil {
		repo[goos] = load(t, filepath.Join("..", ".."), goos)
	}
	return repo[goos]
}

// load reads the packages under cmd/ and internal/ of the module at root,
// the files goos's build compiles.
func load(t *testing.T, root, goos string) *module {
	t.Helper()
	m := newModule(goos)
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(file string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(file) != ".go" {
				return err
			}
			src, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, file)
			if err != nil {
				return err
			}
			return m.parse(filepath.ToSlash(rel), src)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	m.check(t)
	return m
}

func (p *pkg) at(n ast.Node) string {
	pos := p.fset.Position(n.Pos())
	return fmt.Sprintf("%s:%d:", pos.Filename, pos.Line)
}

// allowed reports whether the line of n carries `//lint:allow <rule>: <why>`.
func (p *pkg) allowed(rule string, n ast.Node) bool {
	pos := p.fset.Position(n.Pos())
	for _, f := range slices.Concat(p.files, p.tests) {
		if p.fset.Position(f.Pos()).Filename != pos.Filename {
			continue
		}
		for _, g := range f.Comments {
			for _, c := range g.List {
				if p.fset.Position(c.Pos()).Line != pos.Line {
					continue
				}
				if _, why, ok := strings.Cut(c.Text, "//lint:allow "+rule+":"); ok && strings.TrimSpace(why) != "" {
					return true
				}
			}
		}
	}
	return false
}

// rules maps a rule's name (the one `//lint:allow` takes) to its check.
var rules = map[string]func(*pkg) []string{
	"complete":  completeWrites,
	"goroutine": bareGoroutines,
	"sleep":     sleeps,
	"writes":    rawWrites,
}

// TestRepo runs every rule over cmd/ and internal/, on each of goosList's
// builds. A file both compile is reported once.
func TestRepo(t *testing.T) {
	var mods []*module
	for _, goos := range goosList {
		m := loadRepo(t, goos)
		if m.pkgs[modulePath+"/cmd/kizuna"] == nil || len(m.pkgs) < 10 {
			t.Fatalf("%s: found %d packages: the walk is not reaching the code", goos, len(m.pkgs))
		}
		mods = append(mods, m)
	}
	for _, name := range slices.Sorted(maps.Keys(rules)) {
		t.Run(name, func(t *testing.T) {
			seen := map[string]bool{}
			for _, m := range mods {
				for _, path := range slices.Sorted(maps.Keys(m.pkgs)) {
					for _, msg := range rules[name](m.pkgs[path]) {
						if !seen[msg] {
							seen[msg] = true
							t.Error(msg)
						}
					}
				}
			}
		})
	}
}

// TestHelpersExist: the failures name functions to use, so a rename must
// not leave one naming a function that is gone.
func TestHelpersExist(t *testing.T) {
	m := loadRepo(t, "linux")
	have := map[string]bool{}
	for fn := range m.funcs {
		have[fn.FullName()] = true
	}
	for _, name := range slices.Sorted(maps.Keys(helpers)) {
		if !strings.Contains(noRecover, name) {
			t.Errorf("the failure doesn't name %s", name)
		}
		if !have[helpers[name]] {
			t.Errorf("%s: no such function in the module", helpers[name])
		}
	}
	for _, name := range []string{"WriteFile", "Rename"} {
		if !strings.Contains(noAtomicWrite, "fetch."+name) {
			t.Errorf("the failure doesn't name fetch.%s", name)
		}
		if !have[modulePath+"/internal/fetch."+name] {
			t.Errorf("fetch.%s: no such function in the module", name)
		}
	}
	// The sleep failure's hooks. typ is "" for one declared at the
	// package's top.
	for _, h := range []struct{ pkg, typ, name string }{
		{"sources", "AniDB", "WaitIdle"}, {"sources", "TVDB", "WaitIdle"},
		{"fetch", "Client", "WaitQueue"}, {"fetch", "hostLimit", "released"},
		{"provider", "", "crewAfterFunc"},
	} {
		if !strings.Contains(noWaitOnClock, h.name) {
			t.Errorf("the failure doesn't name %s", h.name)
		}
		scope := m.pkgs[modulePath+"/internal/"+h.pkg].types.Scope()
		var obj types.Object
		if h.typ == "" {
			obj = scope.Lookup(h.name)
		} else if typ := scope.Lookup(h.typ); typ != nil {
			obj, _, _ = types.LookupFieldOrMethod(typ.Type(), true, typ.Pkg(), h.name)
		}
		if obj == nil {
			t.Errorf("%s.%s.%s: not in the module", h.pkg, h.typ, h.name)
		}
	}
}

// TestRulesCatchWindowsFiles: a _windows.go file is compiled by the windows build only, so only the
// windows pass reads it; the same for a _windows_test.go file.
func TestRulesCatchWindowsFiles(t *testing.T) {
	const src = `package x

import "os"

type Show struct{ Complete bool }

func a(b []byte) { os.WriteFile("s", b, 0o644); go a(nil); (&Show{}).Complete = true }
`
	const test = `package x

import "time"

func b() { time.Sleep(1) }
`
	for _, c := range []struct {
		goos string
		want int
	}{{"linux", 0}, {"windows", 1}} {
		m := newModule(c.goos)
		if err := m.parse("x/a_windows.go", []byte(src)); err != nil {
			t.Fatal(err)
		}
		if err := m.parse("x/a_windows_test.go", []byte(test)); err != nil {
			t.Fatal(err)
		}
		m.check(t)
		for name, rule := range rules {
			if got := rule(m.pkgs[modulePath+"/x"]); len(got) != c.want {
				t.Errorf("%s on %s: %d violations, want %d: %q", name, c.goos, len(got), c.want, got)
			}
		}
	}
}
