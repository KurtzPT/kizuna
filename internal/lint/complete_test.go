package lint

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

// completeOutsideBuild is the failure for a write of Show.Complete outside
// buildShow. The functions it names are checked by TestHelpersExist.
const completeOutsideBuild = " Show.Complete written outside buildShow: whether a show is complete is decided in one " +
	"place, from its build's ledger, and a write anywhere else can make a show complete that missed a value. " +
	"Record the miss in the reading actor's ledger instead (lazy.get takes it; misses.add, misses.failed for a " +
	"direct read): buildShow sets Complete when the build's merged ledger is empty. " +
	"A deliberate exception: `//lint:allow complete: <why>` on this line"

// completeWrites: a write of a Show's Complete field in a non-test file,
// outside the method buildShow of *Provider: an assignment or a range's key
// or value, its address taken (a write through the pointer can't be followed),
// or the field set in a composite literal, keyed or positional. A copy of a
// whole Show (*s = c) writes Complete too, and syntax can't tell it from any
// other copy: review catches that one.
func completeWrites(p *pkg) []string {
	var out []string
	report := func(n ast.Node) {
		if !p.allowed("complete", n) {
			out = append(out, p.at(n)+completeOutsideBuild)
		}
	}
	isComplete := func(x ast.Expr) bool {
		sel, ok := ast.Unparen(x).(*ast.SelectorExpr)
		return ok && isShowComplete(p.info.Uses[sel.Sel])
	}
	for _, f := range p.files {
		for _, d := range f.Decls {
			if isBuildShow(d) {
				continue
			}
			ast.Inspect(d, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range n.Lhs {
						if isComplete(lhs) {
							report(lhs)
						}
					}
				case *ast.RangeStmt:
					for _, x := range []ast.Expr{n.Key, n.Value} {
						if isComplete(x) {
							report(x)
						}
					}
				case *ast.UnaryExpr:
					if n.Op == token.AND && isComplete(n.X) {
						report(n)
					}
				case *ast.KeyValueExpr:
					if key, ok := n.Key.(*ast.Ident); ok && isShowComplete(p.info.Uses[key]) {
						report(n)
					}
				case *ast.CompositeLit:
					if len(n.Elts) == 0 {
						break
					}
					if _, keyed := n.Elts[0].(*ast.KeyValueExpr); !keyed && hasShowComplete(p.info.TypeOf(n)) {
						report(n)
					}
				}
				return true
			})
		}
	}
	return out
}

// isBuildShow: d is the method buildShow of *Provider.
func isBuildShow(d ast.Decl) bool {
	fd, ok := d.(*ast.FuncDecl)
	if !ok || fd.Recv == nil || fd.Name.Name != "buildShow" {
		return false
	}
	star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	recv, ok := star.X.(*ast.Ident)
	return ok && recv.Name == "Provider"
}

// hasShowComplete: t is a struct with Show's Complete field (Show, or a type
// defined from it).
func hasShowComplete(t types.Type) bool {
	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := range st.NumFields() {
		if isShowComplete(st.Field(i)) {
			return true
		}
	}
	return false
}

// isShowComplete: obj is the Complete field of the struct type Show
// declared in obj's package.
func isShowComplete(obj types.Object) bool {
	v, ok := obj.(*types.Var)
	if !ok || !v.IsField() || v.Name() != "Complete" || v.Pkg() == nil {
		return false
	}
	show, ok := v.Pkg().Scope().Lookup("Show").(*types.TypeName)
	if !ok {
		return false
	}
	st, ok := show.Type().Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := range st.NumFields() {
		if st.Field(i) == v {
			return true
		}
	}
	return false
}

// TestRulesCatchComplete runs the complete rule on the faulty shapes it
// exists for, and on the right ones.
func TestRulesCatchComplete(t *testing.T) {
	check := func(t *testing.T, want int, src string) {
		t.Helper()
		const decls = `package x

type Show struct {
	Root     string
	Complete bool
}

type Provider struct{}

type result struct{ Complete bool }

`
		m := newModule("linux")
		if err := m.parse("x/x.go", []byte(decls+src+"\n")); err != nil {
			t.Fatal(err)
		}
		m.check(t)
		if got := rules["complete"](m.pkgs[modulePath+"/x"]); len(got) != want {
			t.Errorf("%d violations, want %d: %q", len(got), want, got)
		}
	}
	for _, c := range []struct {
		name string
		want int
		src  string
	}{
		{"assigned in a build", 1, `func build(s *Show) { s.Complete = false }`},
		{"assigned with another value", 1, `func build(s *Show) (n int) { s.Complete, n = false, 1; return }`},
		{"in a literal", 1, `func build() *Show { return &Show{Root: "tvdb-1", Complete: true} }`},
		{"in an elided literal", 1, `var shows = []Show{{Complete: true}}`},
		{"in a closure", 1, `func build(s *Show) func() { return func() { s.Complete = true } }`},
		{"in buildShow", 0, `func (p *Provider) buildShow(s *Show) { s.Complete = true }`},
		{"in a closure in buildShow", 0, `func (p *Provider) buildShow(s *Show) { func() { s.Complete = true }() }`},
		{"buildShow that isn't a method", 1, `func buildShow(s *Show) { s.Complete = true }`},
		{"buildShow on another type", 1, `type other struct{}
func (o *other) buildShow(s *Show) { s.Complete = true }`},
		{"parenthesized", 1, `func build(s *Show) { (s.Complete) = true }`},
		{"through a pointer", 1, `func build(s *Show) { p := &s.Complete; *p = true }`},
		{"passed by pointer", 1, `func set(b *bool) { *b = true }
func build(s *Show) { set(&s.Complete) }`},
		{"a range's value", 1, `func build(s *Show) { for _, s.Complete = range []bool{true} {} }`},
		{"a range's key", 1, `func build(s *Show) { for s.Complete = range map[bool]int{} {} }`},
		{"in a positional literal", 1, `func build() *Show { return &Show{"tvdb-1", true} }`},
		{"another type's positional literal", 0, `var r = result{true}`},
		{"read", 0, `func ttl(s *Show) bool { return s.Complete }`},
		{"another type's Complete", 0, `func r(s *Show) result { x := result{Complete: s.Complete}; x.Complete = true; return x }`},
		{"allowed", 0, `func build(s *Show) { s.Complete = false //lint:allow complete: a test double
				}`},
		{"allowed without a reason", 1, `func build(s *Show) { s.Complete = false //lint:allow complete:
				}`},
	} {
		t.Run(c.name, func(t *testing.T) { check(t, c.want, c.src) })
	}
	t.Run("a test file", func(t *testing.T) {
		m := newModule("linux")
		if err := m.parse("x/x.go", []byte("package x\n\ntype Show struct{ Complete bool }\n")); err != nil {
			t.Fatal(err)
		}
		if err := m.parse("x/x_test.go", []byte("package x\n\nvar s = Show{Complete: true}\n")); err != nil {
			t.Fatal(err)
		}
		m.check(t)
		if got := rules["complete"](m.pkgs[modulePath+"/x"]); len(got) != 0 {
			t.Errorf("%d violations, want none: %q", len(got), got)
		}
	})
}

// TestCompleteHelpersExist: the failure names functions of internal/provider
// (by type and method), so a rename must not leave one naming a function
// that is gone.
func TestCompleteHelpersExist(t *testing.T) {
	scope := loadRepo(t, "linux").pkgs[modulePath+"/internal/provider"].types.Scope()
	for _, h := range []struct{ typ, method, named string }{
		{"Provider", "buildShow", "buildShow"}, {"lazy", "get", "lazy.get"},
		{"misses", "add", "misses.add"}, {"misses", "failed", "misses.failed"},
	} {
		if !strings.Contains(completeOutsideBuild, h.named) {
			t.Errorf("the failure doesn't name %s", h.named)
		}
		typ := scope.Lookup(h.typ)
		if typ == nil {
			t.Errorf("provider.%s: not in the module", h.typ)
			continue
		}
		if obj, _, _ := types.LookupFieldOrMethod(typ.Type(), true, typ.Pkg(), h.method); obj == nil {
			t.Errorf("provider.%s.%s: not in the module", h.typ, h.method)
		}
	}
}
