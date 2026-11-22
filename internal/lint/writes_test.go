package lint

import (
	"fmt"
	"go/ast"
	"go/types"
	"strings"
	"testing"
)

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

// TestRulesCatchWrites runs the writes rule on the faulty shapes it exists for, and on
// the right ones.
func TestRulesCatchWrites(t *testing.T) {
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
}
