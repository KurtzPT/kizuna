// Package lint holds the repository's own rules: tests that type-check the
// Go files under cmd/ and internal/ and fail with a message that names the
// fix. They run with `go test ./...`, in CI and in the image build.
//
// To add a rule: write a func(*pkg) []string that returns one message per
// violation (use p.at for the "file:line:" prefix), add it to rules, and
// give it a case in TestRulesCatch that shows the faulty shape failing.
// A deliberate exception is a comment on the offending line:
// `//lint:allow <rule>: <why>`.
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

// pkg is one package: the files one GOOS's build compiles, type-checked.
// Test files aren't read (type-checking them would build every test-only
// import).
type pkg struct {
	*module
	files []*ast.File
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
		},
		funcs: map[*types.Func]*ast.FuncDecl{},
		pkgs:  map[string]*pkg{},
	}
}

// parse adds the file name (slash-separated, relative to the module root)
// to its directory's package.
func (m *module) parse(name string, src []byte) error {
	if strings.HasSuffix(name, "_test.go") {
		return nil
	}
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
	if match {
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
	for _, f := range p.files {
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
	"goroutine": bareGoroutines,
	"writes":    rawWrites,
}

// starters are the calls other than a go statement that run a function on
// a goroutine of its own, by the index of that function's argument.
var starters = map[string]int{
	"(*sync.WaitGroup).Go": 0,
	"time.AfterFunc":       1,
	"context.AfterFunc":    1,
}

// logPanics are the log functions that panic.
var logPanics = map[string]bool{
	"log.Panic": true, "log.Panicf": true, "log.Panicln": true,
	"(*log.Logger).Panic": true, "(*log.Logger).Panicf": true, "(*log.Logger).Panicln": true,
}

// helpers are the functions the failure names: the name as written there,
// and the function it must be.
var helpers = map[string]string{
	"(*group).Go":    "(*kizuna/internal/provider.group).Go",
	"caught.keep":    "(*kizuna/internal/sources.caught).keep",
	"caught.raise":   "(*kizuna/internal/sources.caught).raise",
	"fetch.LogPanic": "kizuna/internal/fetch.LogPanic",
}

const (
	noRecover = " a goroutine with no recover: a panic on it ends the whole process, " +
		"not one show. In package provider, start it with (*group).Go. Elsewhere, its function " +
		"must defer one that recovers and doesn't panic again: in package sources, caught.keep " +
		"when a call waits for it (that call then calls caught.raise); when nobody waits for it, " +
		"`defer func() { if r := recover(); r != nil { fetch.LogPanic(\"<what panicked>\", r) } }()` " +
		"(see internal/sources/panics.go). A deliberate exception: `//lint:allow goroutine: <why>` " +
		"on this line"
	starterValue = " a goroutine starter used as a value: the lint can't see what it runs. Call it " +
		"directly with the function to run, or `//lint:allow goroutine: <why>` on this line"
)

// bareGoroutines: a recover only covers its own goroutine, so a panic on a
// goroutine without one ends the process for every show (#21, #51).
// The function a goroutine starts with must defer, in its own body, a
// function that recovers and doesn't panic again. A literal that only calls
// such a function counts as one. A defer placed after the work
// (`work(); defer keep()`) passes too: the rule doesn't check the order.
func bareGoroutines(p *pkg) []string {
	var out []string
	for _, f := range p.files {
		called := map[*ast.Ident]bool{} // the names a call calls
		ast.Inspect(f, func(n ast.Node) bool {
			var fun ast.Expr
			switch n := n.(type) {
			case *ast.GoStmt:
				fun = n.Call.Fun
			case *ast.CallExpr:
				called[name(n.Fun)] = true
				fn := p.callee(n.Fun)
				if fn == nil {
					return true
				}
				i, ok := starters[fn.FullName()]
				if sel, isSel := ast.Unparen(n.Fun).(*ast.SelectorExpr); isSel && p.info.Selections[sel] != nil &&
					p.info.Selections[sel].Kind() == types.MethodExpr {
					i++ // (*sync.WaitGroup).Go(wg, f): the receiver comes first
				}
				if !ok || i >= len(n.Args) {
					return true
				}
				fun = n.Args[i]
			case *ast.Ident:
				if fn, ok := p.info.Uses[n].(*types.Func); ok && !called[n] {
					if _, ok := starters[fn.Origin().FullName()]; ok && !p.allowed("goroutine", n) {
						out = append(out, p.at(n)+starterValue)
					}
				}
				return true
			default:
				return true
			}
			if !p.guarded(fun) && !p.allowed("goroutine", n) {
				out = append(out, p.at(n)+noRecover)
			}
			return true
		})
	}
	return out
}

// guarded: the function fun names defers a function that recovers, or fun
// is a literal whose body is one call of a function that does. That call's
// receiver and arguments run before the function's defer, so they must be
// unable to panic: names and literals.
func (p *pkg) guarded(fun ast.Expr) bool {
	body := p.body(fun)
	if body == nil {
		return false
	}
	if p.defersRecover(body) {
		return true
	}
	if _, lit := ast.Unparen(fun).(*ast.FuncLit); lit && len(body.List) == 1 {
		if s, ok := body.List[0].(*ast.ExprStmt); ok {
			if c, ok := s.X.(*ast.CallExpr); ok && inert(c.Fun) && slices.IndexFunc(c.Args, func(a ast.Expr) bool { return !inert(a) }) < 0 {
				b := p.body(c.Fun)
				return b != nil && p.defersRecover(b)
			}
		}
	}
	return false
}

// inert: e can't panic when it is evaluated. It is a name, a literal, or a
// name's field or method.
func inert(e ast.Expr) bool {
	switch e := ast.Unparen(e).(type) {
	case *ast.Ident, *ast.BasicLit, *ast.FuncLit:
		return true
	case *ast.SelectorExpr:
		_, ok := ast.Unparen(e.X).(*ast.Ident)
		return ok
	}
	return false
}

// defersRecover: a statement of body itself defers a function that calls
// recover and doesn't panic again. A defer in an if or a loop may not run,
// so it doesn't count. A recover that panics again sends the panic on.
func (p *pkg) defersRecover(body *ast.BlockStmt) bool {
	for _, s := range body.List {
		if d, ok := s.(*ast.DeferStmt); ok {
			if b := p.body(d.Call.Fun); b != nil && p.callsBuiltin(b, "recover") && !p.panics(b, map[*ast.BlockStmt]bool{}) {
				return true
			}
		}
	}
	return false
}

// body is the body of the function fun names: a literal's, or that of the
// function or method declared in the module. Nil for any other (a func
// value, an interface's method): what it runs can't be known.
func (p *pkg) body(fun ast.Expr) *ast.BlockStmt {
	if lit, ok := ast.Unparen(fun).(*ast.FuncLit); ok {
		return lit.Body
	}
	if fd := p.funcs[p.callee(fun)]; fd != nil {
		return fd.Body
	}
	return nil
}

// name is the identifier fun ends in, through parentheses and the type
// arguments of a generic function. Nil when there is none.
func name(fun ast.Expr) *ast.Ident {
	switch fun := ast.Unparen(fun).(type) {
	case *ast.Ident:
		return fun
	case *ast.SelectorExpr:
		return fun.Sel
	case *ast.IndexExpr:
		return name(fun.X)
	case *ast.IndexListExpr:
		return name(fun.X)
	}
	return nil
}

// callee is the function or method fun names, generic ones by their
// declaration, or nil.
func (p *pkg) callee(fun ast.Expr) *types.Func {
	if id := name(fun); id != nil {
		if fn, ok := p.info.Uses[id].(*types.Func); ok {
			return fn.Origin()
		}
	}
	return nil
}

// calls: body has a call for which is is true. A function literal it holds
// doesn't count: it may never run.
func (p *pkg) calls(body *ast.BlockStmt, is func(*ast.CallExpr) bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if _, lit := n.(*ast.FuncLit); lit || found {
			return false
		}
		if c, ok := n.(*ast.CallExpr); ok && is(c) {
			found = true
		}
		return !found
	})
	return found
}

// callsBuiltin: body calls the builtin name itself. Recover only stops a
// panic when the deferred function calls it, not one it holds.
func (p *pkg) callsBuiltin(body *ast.BlockStmt, name string) bool {
	return p.calls(body, func(c *ast.CallExpr) bool {
		id, ok := ast.Unparen(c.Fun).(*ast.Ident)
		if !ok {
			return false
		}
		b, ok := p.info.Uses[id].(*types.Builtin)
		return ok && b.Name() == name
	})
}

// panics: body calls panic, a log function that panics, or a function of
// the module that does, so a recover that calls it sends the panic on.
// seen holds the bodies already looked at, for recursion.
func (p *pkg) panics(body *ast.BlockStmt, seen map[*ast.BlockStmt]bool) bool {
	if seen[body] {
		return false
	}
	seen[body] = true
	if p.callsBuiltin(body, "panic") {
		return true
	}
	return p.calls(body, func(c *ast.CallExpr) bool {
		fn := p.callee(c.Fun)
		if fn == nil {
			return false
		}
		fd := p.funcs[fn]
		return logPanics[fn.FullName()] || fd != nil && p.panics(fd.Body, seen)
	})
}

// noAtomicWrite is the failure for a file written in place or renamed
// without the Windows retry. Both helpers are named here, and
// TestHelpersExist checks they are still there.
const noAtomicWrite = " a raw os write: a file written in place is left half-written by a crash, a full disk or " +
	"a reader mid-write, and a plain os.Rename fails on Windows while a reader holds the file. " +
	"Replace it with fetch.WriteFile (the data goes to a temp file, is synced, then renamed over " +
	"the old one) or, for a rename, fetch.Rename. Appending is fine: os.OpenFile with os.O_APPEND. " +
	"A deliberate exception: `//lint:allow writes: <why>` on this line"

// writers are the functions that write a file in place, or replace one
// without the Windows retry.
var writers = map[string]bool{
	"os.WriteFile": true, "os.Create": true, "os.Rename": true, "os.OpenFile": true,
	"io/ioutil.WriteFile": true,
}

// rawWrites: a call of os.WriteFile, ioutil.WriteFile, os.Create or os.Rename, or of
// os.OpenFile that truncates, or that creates or opens for writing without
// os.O_APPEND (the flags must be written out as os.O_* names: any other
// expression may hide one). One of them used as a value, which hides its
// call and flags, counts too. fetch.WriteFile and fetch.Rename are the way to write.
func rawWrites(p *pkg) []string {
	var out []string
	for _, f := range p.files {
		called := map[*ast.Ident]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				id := name(n.Fun)
				if id == nil || !p.isWriter(id) {
					return true
				}
				called[id] = true
				if id.Name == "OpenFile" && len(n.Args) == 3 && !p.writesInPlace(n.Args[1]) {
					return true
				}
				if !p.allowed("writes", n) {
					out = append(out, p.at(n)+noAtomicWrite)
				}
			case *ast.Ident:
				if p.isWriter(n) && !called[n] && !p.allowed("writes", n) {
					out = append(out, p.at(n)+noAtomicWrite)
				}
			}
			return true
		})
	}
	return out
}

// isWriter: id names one of writers.
func (p *pkg) isWriter(id *ast.Ident) bool {
	fn, ok := p.info.Uses[id].(*types.Func)
	return ok && writers[fn.FullName()]
}

// writesInPlace: flag, an os.OpenFile's flags, truncates the file, or
// creates it or opens it for writing without appending, or isn't made of
// os.O_* names so that it can't be told.
func (p *pkg) writesInPlace(flag ast.Expr) bool {
	var create, write, trunc, appends, other bool
	ast.Inspect(flag, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if _, pkgName := p.info.Uses[n].(*types.PkgName); pkgName {
				break
			}
			c, ok := p.info.Uses[n].(*types.Const)
			if !ok || c.Pkg() == nil || c.Pkg().Path() != "os" || !strings.HasPrefix(c.Name(), "O_") {
				other = true
				break
			}
			switch c.Name() {
			case "O_CREATE":
				create = true
			case "O_WRONLY", "O_RDWR":
				write = true
			case "O_TRUNC":
				trunc = true
			case "O_APPEND":
				appends = true
			}
		case nil, *ast.BasicLit, *ast.BinaryExpr, *ast.ParenExpr, *ast.SelectorExpr:
		default:
			other = true
		}
		return true
	})
	return other || trunc || !appends && (create || write)
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
}

// TestRulesCatch runs each rule on the faulty shapes it exists for, and on
// the right ones.
func TestRulesCatch(t *testing.T) {
	t.Run("goroutine", func(t *testing.T) {
		// check runs the rule on package x, made of the files srcs, each with
		// every import a case uses, and a prelude; and a package y with a
		// recovering y.Keep.
		check := func(t *testing.T, want int, srcs ...string) {
			t.Helper()
			const imports = `package x

import (
	"context"
	"log"
	"sync"
	"time"

	"kizuna/y"
)

var (
	_ context.Context
	_ = log.Panicf
	_ sync.WaitGroup
	_ time.Duration
	_ = y.Keep
)

`
			m := newModule("linux")
			files := map[string]string{
				"y/y.go":       "package y\n\nfunc Keep() { _ = recover() }\n",
				"x/prelude.go": "package x\n\nfunc work() {}\n\nfunc keep() { _ = recover() }\n\nfunc get() int { return 1 }\n\ntype runner interface{ Run() }\n",
			}
			for i, src := range srcs {
				files[fmt.Sprintf("x/x%d.go", i)] = imports + src + "\n"
			}
			for _, name := range slices.Sorted(maps.Keys(files)) {
				if err := m.parse(name, []byte(files[name])); err != nil {
					t.Fatal(err)
				}
			}
			m.check(t)
			if got := rules["goroutine"](m.pkgs[modulePath+"/x"]); len(got) != want {
				t.Errorf("%d violations, want %d: %q", len(got), want, got)
			}
		}
		for _, c := range []struct {
			name string
			want int
			srcs []string
		}{
			{"literal", 1, []string{`func a() { go func() { work() }() }`}},
			{"function", 1, []string{`func a() { go b() }; func b() { work() }`}},
			{"interface method", 1, []string{`func a(r runner) { go r.Run() }`}},
			{"func value", 1, []string{`func a(f func()) { go f() }`}},
			{"literal recovers", 0, []string{`func a() { go func() { defer func() { recover() }(); work() }() }`}},
			{"function recovers", 0, []string{`func a() { go b() }; func b() { defer keep(); work() }`}},
			{"other package's recover", 0, []string{`func a() { go func() { defer y.Keep(); work() }() }`}},
			{"literal calling one that recovers", 0, []string{`func a() { go func() { b() }() }; func b() { defer keep(); work() }`}},
			{"literal calling one with plain arguments", 0, []string{`func a(n int) { go func() { b(n, 1) }() }; func b(int, int) { defer keep() }`}},
			{"literal calling a method of a name", 0, []string{`type s struct{}; func (s) b() { defer keep() }
				func a(x s) { go func() { x.b() }() }`}},
			{"literal calling one with a call as argument", 1, []string{`func a() { go func() { b(get()) }() }; func b(int) { defer keep() }`}},
			{"literal calling a method of a call", 1, []string{`type s struct{}; func (s) b() { defer keep() }
				func mk() s { return s{} }
				func a() { go func() { mk().b() }() }`}},
			{"literal calling more", 1, []string{`func a() { go func() { b(); work() }() }; func b() { defer keep() }`}},
			{"defer in an if", 1, []string{`func a(ok bool) { go func() { if ok { defer keep() }; work() }() }`}},
			{"recover in a nested literal", 1, []string{`func a() { go func() { defer func() { func() { recover() }() }(); work() }() }`}},
			{"recover in an inner goroutine", 1, []string{`func a() { go func() { go func() { defer keep() }(); work() }() }`}},
			{"recover that panics again", 1, []string{`func a() { go b() }; func b() { defer again(); work() }
				func again() { if r := recover(); r != nil { panic(r) } }`}},
			{"recover that logs a panic", 1, []string{`func a() { go b() }; func b() { defer again(); work() }
				func again() { if r := recover(); r != nil { log.Panicf("%v", r) } }`}},
			{"recover that panics through a function", 1, []string{`func a() { go b() }; func b() { defer again(); work() }
				func again() { if r := recover(); r != nil { rethrow(r) } }
				func rethrow(r any) { panic(r) }`}},
			{"recover that calls a function that doesn't panic", 0, []string{`func a() { go b() }; func b() { defer again(); work() }
				func again() { if r := recover(); r != nil { note(r) } }
				func note(any) {}`}},
			{"same name, other method", 1, []string{`type lazy[T any] struct{}; type group struct{}
				func (l *lazy[T]) keepPanic() { if r := recover(); r != nil { panic(r) } }
				func (g *group) keepPanic() { recover() }
				func (l *lazy[T]) run() { defer l.keepPanic(); work() }
				func a(l *lazy[int]) { go l.run() }`}},
			{"generic method recovers", 0, []string{`type box[T any] struct{}
				func (b *box[T]) keep() { recover() }
				func (b *box[T]) run() { defer b.keep(); work() }
				func a(b *box[int]) { go b.run() }`}},
			{"generic function recovers", 0, []string{`func a() { go run[int]() }; func run[T any]() { defer keep(); work() }`}},
			{"generic function", 1, []string{`func a() { go run[int]() }; func run[T any]() { work() }`}},
			{"WaitGroup.Go", 1, []string{`func a() { var wg sync.WaitGroup; wg.Go(func() { work() }); wg.Wait() }`}},
			{"WaitGroup.Go through a pointer", 1, []string{`func a(wg *sync.WaitGroup) { wg.Go(work) }`}},
			{"WaitGroup.Go recovers", 0, []string{`type s struct{ wg sync.WaitGroup }
				func (x *s) a() { x.wg.Go(x.b) }; func (x *s) b() { defer keep(); work() }`}},
			{"WaitGroup.Go as a method expression", 1, []string{`func a(wg *sync.WaitGroup) { (*sync.WaitGroup).Go(wg, work) }`}},
			{"WaitGroup.Go as a method expression recovers", 0, []string{`func a(wg *sync.WaitGroup) { (*sync.WaitGroup).Go(wg, b) }; func b() { defer keep() }`}},
			{"WaitGroup.Go as a value", 1, []string{`func a(wg *sync.WaitGroup) { g := wg.Go; g(work) }`}},
			{"WaitGroup.Go as a method expression value", 1, []string{`var g = (*sync.WaitGroup).Go`}},
			{"another type's Go", 0, []string{`type g struct{}; func (g) Go(f func()) { f() }
				func a() { var x g; x.Go(func() { work() }) }`}},
			{"another type's Go as a value", 0, []string{`type g struct{}; func (g) Go(f func()) { f() }
				func a() { var x g; h := x.Go; h(work) }`}},
			{"time.AfterFunc", 1, []string{`func a() { time.AfterFunc(time.Second, work) }`}},
			{"time.AfterFunc as a value", 1, []string{`var after = time.AfterFunc; func a() { after(time.Second, work) }`}},
			{"time.AfterFunc as a value, allowed", 0, []string{`var after = time.AfterFunc //lint:allow goroutine: the callers recover
				func a() { after(time.Second, work) }`}},
			{"context.AfterFunc", 1, []string{`func a(ctx context.Context) { context.AfterFunc(ctx, work) }`}},
			{"context.AfterFunc as a value", 1, []string{`func a(ctx context.Context) { after := context.AfterFunc; after(ctx, work) }`}},
			{"allowed", 0, []string{`func a() { go work() //lint:allow goroutine: ends with the process
				}`}},
			{"allowed without a reason", 1, []string{`func a() { go work() //lint:allow goroutine:
				}`}},
			{"allowed in another file", 1, []string{`func a() { go work() }`, `// //lint:allow goroutine: not here`}},
		} {
			t.Run(c.name, func(t *testing.T) { check(t, c.want, c.srcs...) })
		}
	})

	t.Run("writes", func(t *testing.T) {
		// check runs the rule on package x, made of the files srcs, each with
		// the imports a case uses.
		check := func(t *testing.T, want int, srcs ...string) {
			t.Helper()
			const imports = `package x

import (
	"io/ioutil"
	"os"
	osalias "os"
	"path/filepath"
)

var (
	_ = ioutil.ReadAll
	_ = os.Getpid
	_ = osalias.Getpid
	_ = filepath.Join
)

`
			m := newModule("linux")
			for i, src := range srcs {
				if err := m.parse(fmt.Sprintf("x/x%d.go", i), []byte(imports+src+"\n")); err != nil {
					t.Fatal(err)
				}
			}
			m.check(t)
			if got := rules["writes"](m.pkgs[modulePath+"/x"]); len(got) != want {
				t.Errorf("%d violations, want %d: %q", len(got), want, got)
			}
		}
		for _, c := range []struct {
			name string
			want int
			srcs []string
		}{
			{"os.WriteFile", 1, []string{`func a(b []byte) error { return os.WriteFile("s.json", b, 0o644) }`}},
			{"os.WriteFile through an alias", 1, []string{`func a(b []byte) error { return osalias.WriteFile("s.json", b, 0o644) }`}},
			{"os.WriteFile as a value", 1, []string{`var w = os.WriteFile`}},
			{"os.WriteFile as a value, called", 1, []string{`func a(b []byte) error { w := os.WriteFile; return w("s.json", b, 0o644) }`}},
			{"ioutil.WriteFile", 1, []string{`func a(b []byte) error { return ioutil.WriteFile("s.json", b, 0o644) }`}},
			{"os.Create", 1, []string{`func a() { f, _ := os.Create("s.json"); f.Close() }`}},
			{"os.Rename", 1, []string{`func a() error { return os.Rename("a", "b") }`}},
			{"os.Rename as a value", 1, []string{`var rename = os.Rename`}},
			{"os.OpenFile truncating", 1, []string{`func a() { f, _ := os.OpenFile("s", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644); f.Close() }`}},
			{"os.OpenFile creating", 1, []string{`func a() { f, _ := os.OpenFile("s", os.O_WRONLY|os.O_CREATE, 0o644); f.Close() }`}},
			{"os.OpenFile truncating through an alias", 1, []string{`func a() { f, _ := osalias.OpenFile("s", osalias.O_TRUNC, 0o644); f.Close() }`}},
			{"os.OpenFile truncating and appending", 1, []string{`func a() { f, _ := os.OpenFile("s", os.O_WRONLY|os.O_APPEND|os.O_TRUNC, 0o644); f.Close() }`}},
			{"os.OpenFile writing in place", 1, []string{`func a() { f, _ := os.OpenFile("s", os.O_WRONLY, 0); f.Close() }`}},
			{"os.OpenFile reading and writing in place", 1, []string{`func a() { f, _ := os.OpenFile("s", os.O_RDWR, 0); f.Close() }`}},
			{"os.OpenFile reading and appending", 0, []string{`func a() { f, _ := os.OpenFile("s", os.O_RDWR|os.O_APPEND, 0); f.Close() }`}},
			{"os.OpenFile appending", 0, []string{`func a() { f, _ := os.OpenFile("s", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); f.Close() }`}},
			{"os.OpenFile reading", 0, []string{`func a() { f, _ := os.OpenFile("s", os.O_RDONLY, 0); f.Close() }`}},
			{"os.OpenFile with flags in a variable", 1, []string{`func a(flag int) { f, _ := os.OpenFile("s", flag, 0o644); f.Close() }`}},
			{"os.OpenFile with flags in a constant", 1, []string{`const flags = os.O_APPEND; func a() { f, _ := os.OpenFile("s", flags, 0o644); f.Close() }`}},
			{"os.OpenFile as a value", 1, []string{`var open = os.OpenFile`}},
			{"os.ReadFile", 0, []string{`func a() { os.ReadFile("s") }`}},
			{"os.Remove", 0, []string{`func a() { os.Remove("s") }`}},
			{"another package's WriteFile", 0, []string{`type w struct{}; func (w) WriteFile(string, []byte) error { return nil }
				func a() { var x w; x.WriteFile("s", nil) }`}},
			{"allowed", 0, []string{`func a(b []byte) { os.WriteFile("s", b, 0o644) //lint:allow writes: a lost file only costs a request
				}`}},
			{"allowed without a reason", 1, []string{`func a(b []byte) { os.WriteFile("s", b, 0o644) //lint:allow writes:
				}`}},
			{"allowed for another rule", 1, []string{`func a(b []byte) { os.WriteFile("s", b, 0o644) //lint:allow goroutine: no
				}`}},
		} {
			t.Run(c.name, func(t *testing.T) { check(t, c.want, c.srcs...) })
		}
	})
	// A _windows.go file is compiled by the windows build only, so only the
	// windows pass reads it.
	t.Run("windows files", func(t *testing.T) {
		const src = `package x

import "os"

func a(b []byte) { os.WriteFile("s", b, 0o644); go a(nil) }
`
		for _, c := range []struct {
			goos string
			want int
		}{{"linux", 0}, {"windows", 1}} {
			m := newModule(c.goos)
			if err := m.parse("x/a_windows.go", []byte(src)); err != nil {
				t.Fatal(err)
			}
			m.check(t)
			for name, rule := range rules {
				if got := rule(m.pkgs[modulePath+"/x"]); len(got) != c.want {
					t.Errorf("%s on %s: %d violations, want %d: %q", name, c.goos, len(got), c.want, got)
				}
			}
		}
	})
}
