package lint

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"
	"testing"
)

// noWaitOnClock is the failure for a test that waits on the clock. The
// hooks it names are checked by TestHelpersExist.
const noWaitOnClock = " in a test: it waits on the clock for something another goroutine does, so it " +
	"passes or fails with the machine's speed (tests are deterministic). Wait for the event itself: " +
	"WaitIdle (sources.AniDB, sources.TVDB) for background fetches, fetch's (*Client).WaitQueue or " +
	"the throttle's released hook for the request queue, a channel or a sync.WaitGroup the code " +
	"closes or finishes, or an injected timer (provider's crewAfterFunc) for code that waits a " +
	"duration. A test of real time itself: `//lint:allow sleep: <why>` on this line"

// sleeps: in a test file, time.Sleep, runtime.Gosched, time.Tick,
// time.NewTicker, time.NewTimer and time.AfterFunc; a receive from
// time.After (or from a variable set to it) that is a statement, or a
// select case whose body doesn't fail the test (t.Fatal, Fatalf, Error,
// Errorf, FailNow, or panic); and in a for statement, a deadline check
// (time.Since, time.Until, or time.Now() with Before, After, Sub or
// Compare), which is a poll. A range loop is a table's cases, so it may
// check a time against the clock.
//
// It catches the polls and sleeps of a9cb896 and ca377d2, and the 50 ms
// "nothing comes" wait of 351ba3a. It doesn't catch a wait that hides
// behind a helper or a timeout: 9e760cc, 73f3f84 (production code) and
// 85ef256 (a context.WithTimeout over a build) pass. A time.After case that
// fails the test is a guard against a hang, not a wait, so it passes; so
// does one whose body fails the test for another reason, as only the call
// is checked. Test files aren't type-checked, so time and runtime are known
// by their import name in the file: a dot import isn't seen, a variable
// named time is taken for the package, and a variable set to time.After is
// found by its name alone.
func sleeps(p *pkg) []string {
	var out []string
	for _, f := range p.tests {
		imported := map[string]string{} // import path by the name the file gives it
		for _, im := range f.Imports {
			path := strings.Trim(im.Path.Value, `"`)
			if im.Name != nil {
				imported[im.Name.Name] = path
			} else {
				imported[path] = path // time and runtime: the name is the path
			}
		}
		// is: e is a call of pkg.fn, for one of fns.
		is := func(e ast.Expr, pkg string, fns ...string) bool {
			c, ok := ast.Unparen(e).(*ast.CallExpr)
			if !ok {
				return false
			}
			sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr)
			if !ok {
				return false
			}
			x, ok := sel.X.(*ast.Ident)
			return ok && imported[x.Name] == pkg && slices.Contains(fns, sel.Sel.Name)
		}
		// afters: the names a time.After channel is kept in.
		afters := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			var lhs []ast.Expr
			var rhs []ast.Expr
			switch n := n.(type) {
			case *ast.AssignStmt:
				lhs, rhs = n.Lhs, n.Rhs
			case *ast.ValueSpec:
				for _, id := range n.Names {
					lhs = append(lhs, id)
				}
				rhs = n.Values
			}
			for i, r := range rhs {
				if id, ok := lhs[min(i, len(lhs)-1)].(*ast.Ident); ok && len(lhs) == len(rhs) && is(r, "time", "After") {
					afters[id.Name] = true
				}
			}
			return true
		})
		// waits: s receives from time.After, as a statement or into a
		// variable (_ included).
		waits := func(s ast.Stmt) bool {
			var e ast.Expr
			switch s := s.(type) {
			case *ast.ExprStmt:
				e = s.X
			case *ast.AssignStmt:
				if len(s.Rhs) != 1 {
					return false
				}
				e = s.Rhs[0]
			default:
				return false
			}
			u, ok := ast.Unparen(e).(*ast.UnaryExpr)
			if !ok || u.Op != token.ARROW {
				return false
			}
			id, _ := ast.Unparen(u.X).(*ast.Ident)
			return is(u.X, "time", "After") || id != nil && afters[id.Name]
		}
		// fails: body calls t.Fatal, Fatalf, Error, Errorf or FailNow on
		// something, or panic.
		fails := func(body []ast.Stmt) bool {
			found := false
			for _, s := range body {
				ast.Inspect(s, func(n ast.Node) bool {
					c, ok := n.(*ast.CallExpr)
					if !ok {
						return !found
					}
					switch fn := ast.Unparen(c.Fun).(type) {
					case *ast.SelectorExpr:
						found = found || slices.Contains([]string{"Fatal", "Fatalf", "Error", "Errorf", "FailNow"}, fn.Sel.Name)
					case *ast.Ident:
						found = found || fn.Name == "panic"
					}
					return !found
				})
			}
			return found
		}
		// nowCompared: c is t.Before, After, Sub or Compare, with time.Now()
		// as t or as the argument.
		nowCompared := func(c *ast.CallExpr) bool {
			sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr)
			if !ok || !slices.Contains([]string{"Before", "After", "Sub", "Compare"}, sel.Sel.Name) {
				return false
			}
			now := func(e ast.Expr) bool { return is(e, "time", "Now") }
			return now(sel.X) || slices.ContainsFunc(c.Args, now)
		}
		report := func(n ast.Node, what string) {
			if !p.allowed("sleep", n) {
				out = append(out, p.at(n)+" "+what+noWaitOnClock)
			}
		}
		cases := map[ast.Stmt]bool{} // the receives of select cases
		var walk func(n ast.Node, inLoop bool)
		walk = func(n ast.Node, inLoop bool) {
			ast.Inspect(n, func(c ast.Node) bool {
				switch c := c.(type) {
				case *ast.ForStmt, *ast.FuncLit:
					if c != n {
						_, loop := c.(*ast.ForStmt)
						walk(c, loop) // a literal's body runs on its own, maybe once
						return false
					}
				case *ast.CommClause:
					cases[c.Comm] = true
					if c.Comm != nil && waits(c.Comm) && !fails(c.Body) {
						report(c, "a select case on time.After that doesn't fail the test")
					}
				case *ast.ExprStmt, *ast.AssignStmt:
					if s := c.(ast.Stmt); !cases[s] && waits(s) {
						report(c, "a receive from time.After")
					}
				case *ast.CallExpr:
					switch {
					case is(c, "time", "Sleep", "Tick", "NewTicker", "NewTimer", "AfterFunc"), is(c, "runtime", "Gosched"):
						report(c, types.ExprString(c.Fun))
					case inLoop && (is(c, "time", "Since", "Until") || nowCompared(c)):
						report(c, "a deadline check in a for loop (a poll)")
					}
				}
				return true
			})
		}
		walk(f, false)
	}
	return out
}

// TestRulesCatchSleep runs the sleep rule on the faulty shapes it exists for, and on
// the right ones.
func TestRulesCatchSleep(t *testing.T) {
	// check runs the rule on package x, made of the test files srcs,
	// each with the imports a case uses.
	check := func(t *testing.T, want int, srcs ...string) {
		t.Helper()
		const imports = `package x

import (
	"runtime"
	"testing"
	"time"
	clock "time"
)

var (
	_ = runtime.NumCPU
	_ testing.T
	_ time.Duration
	_ clock.Duration
)

`
		m := newModule("linux")
		for i, src := range srcs {
			if err := m.parse(fmt.Sprintf("x/x%d_test.go", i), []byte(imports+src+"\n")); err != nil {
				t.Fatal(err)
			}
		}
		m.check(t)
		if got := rules["sleep"](m.pkgs[modulePath+"/x"]); len(got) != want {
			t.Errorf("%d violations, want %d: %q", len(got), want, got)
		}
	}
	for _, c := range []struct {
		name string
		want int
		srcs []string
	}{
		{"time.Sleep", 1, []string{`func TestA(t *testing.T) { time.Sleep(10 * time.Millisecond) }`}},
		{"time.Sleep through an alias", 1, []string{`func TestA(t *testing.T) { clock.Sleep(time.Millisecond) }`}},
		{"time.Sleep in a goroutine", 1, []string{`func TestA(t *testing.T) { go func() { time.Sleep(1) }() }`}},
		{"runtime.Gosched", 1, []string{`func TestA(t *testing.T) { for !done() { runtime.Gosched() } }; func done() bool { return true }`}},
		{"time.Tick", 1, []string{`func TestA(t *testing.T) { for range time.Tick(time.Millisecond) { break } }`}},
		{"time.NewTicker", 1, []string{`func TestA(t *testing.T) { tk := time.NewTicker(time.Millisecond); tk.Stop() }`}},
		{"time.NewTimer", 1, []string{`func TestA(t *testing.T) { tm := time.NewTimer(time.Millisecond); <-tm.C }`}},
		{"time.AfterFunc", 1, []string{`func TestA(t *testing.T) { time.AfterFunc(time.Millisecond, func() {}) }`}},
		{"a receive from time.After", 1, []string{`func TestA(t *testing.T) { <-time.After(time.Millisecond) }`}},
		{"a select case that only waits", 1, []string{`func TestA(t *testing.T, ch chan int) {
				select {
				case <-ch:
					t.Fatal("too early")
				case <-time.After(50 * time.Millisecond):
				}
			}`}},
		{"a select case that continues", 1, []string{`func TestA(t *testing.T, ch chan int) {
				for {
					select {
					case <-ch:
						return
					case <-time.After(50 * time.Millisecond):
						continue
					}
				}
			}`}},
		{"a select case that returns", 1, []string{`func TestA(t *testing.T, ch chan int) {
				select {
				case <-ch:
				case <-time.After(50 * time.Millisecond):
					return
				}
			}`}},
		{"a select case that only logs", 1, []string{`func TestA(t *testing.T, ch chan int) {
				select {
				case <-ch:
				case <-time.After(50 * time.Millisecond):
					t.Log("nothing came")
				}
			}`}},
		{"a select case that assigns the receive", 1, []string{`func TestA(t *testing.T, ch chan int) {
				select {
				case <-ch:
					t.Fatal("too early")
				case _ = <-time.After(50 * time.Millisecond):
				}
			}`}},
		{"a receive from time.After assigned", 1, []string{`func TestA(t *testing.T) { _ = <-time.After(time.Millisecond) }`}},
		{"a receive from a kept time.After", 1, []string{`func TestA(t *testing.T) { w := time.After(time.Millisecond); <-w }`}},
		{"a select case on a kept time.After that only waits", 1, []string{`func TestA(t *testing.T, ch chan int) {
				w := time.After(time.Millisecond)
				select {
				case <-ch:
				case <-w:
				}
			}`}},
		{"a kept time.After guarding a hang", 0, []string{`func TestA(t *testing.T, ch chan int) {
				hung := time.After(time.Second)
				select {
				case <-ch:
				case <-hung:
					t.Fatal("hung")
				}
			}`}},
		{"time.After guarding a hang with panic", 0, []string{`func TestA(t *testing.T, ch chan int) {
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					panic("hung")
				}
			}`}},
		{"time.After guarding a hang with Errorf", 0, []string{`func TestA(t *testing.T, ch chan int) {
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Errorf("hung")
				}
			}`}},
		{"time.After guarding a hang", 0, []string{`func TestA(t *testing.T, ch chan int) {
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatal("hung")
				}
			}`}},
		// a9cb896's poll, as it was.
		{"poll with a deadline and a sleep", 2, []string{`func TestA(t *testing.T) {
				for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
					if done() {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("never drained")
					}
				}
			}
			func done() bool { return true }`}},
		{"busy poll on a deadline", 1, []string{`func TestA(t *testing.T, deadline time.Time) { for deadline.After(time.Now()) { } }`}},
		{"busy poll on time.Since", 1, []string{`func TestA(t *testing.T, start time.Time) { for time.Since(start) < time.Second { } }`}},
		{"busy poll on Sub", 1, []string{`func TestA(t *testing.T, start time.Time) { for { if time.Now().Sub(start) > time.Second { return } } }`}},
		{"clock checked outside a loop", 0, []string{`func TestA(t *testing.T, at time.Time) { if time.Until(at) < time.Hour { t.Fatal(at) } }`}},
		{"clock checked over a table", 0, []string{`func TestA(t *testing.T, at []time.Time) {
				for _, a := range at {
					if time.Until(a) < time.Hour { t.Fatal(a) }
				}
			}`}},
		{"time.Now in a loop, not compared", 0, []string{`func TestA(t *testing.T) { var at []time.Time; for range 3 { at = append(at, time.Now().Add(-time.Hour)) }; _ = at }`}},
		{"deadline check in a goroutine a loop starts", 0, []string{`func TestA(t *testing.T, start time.Time) {
				for range 2 {
					go func() { _ = time.Since(start) }()
				}
			}`}},
		{"another type's Sleep", 0, []string{`type z struct{}; func (z) Sleep(time.Duration) {}
				func TestA(t *testing.T) { var w z; w.Sleep(1) }`}},
		{"allowed", 0, []string{`func TestA(t *testing.T) { time.Sleep(time.Millisecond) //lint:allow sleep: the gap is real time
				}`}},
		{"allowed without a reason", 1, []string{`func TestA(t *testing.T) { time.Sleep(time.Millisecond) //lint:allow sleep:
				}`}},
		{"allowed for another rule", 1, []string{`func TestA(t *testing.T) { time.Sleep(time.Millisecond) //lint:allow writes: no
				}`}},
	} {
		t.Run(c.name, func(t *testing.T) { check(t, c.want, c.srcs...) })
	}
}
