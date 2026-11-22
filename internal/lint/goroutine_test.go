package lint

import (
	"fmt"
	"go/ast"
	"go/types"
	"maps"
	"slices"
	"testing"
)

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

// TestRulesCatchGoroutine runs the goroutine rule on the faulty shapes it exists for, and on
// the right ones.
func TestRulesCatchGoroutine(t *testing.T) {
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
}
