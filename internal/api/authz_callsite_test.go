package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Enforces ADR-037 (09 §4): every handler calls Can() at its own call site.

// handlersMissingCan reports every http.HandlerFunc-shaped function under dir whose
// body contains no call to Can.
func handlersMissingCan(dir string) ([]string, error) {
	var missing []string
	fset := token.NewFileSet()

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" && dir != path {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !isHandler(fn) {
				continue
			}
			if !callsCan(fn.Body) {
				missing = append(missing, fn.Name.Name+" at "+fset.Position(fn.Pos()).String())
			}
		}
		return nil
	})
	return missing, err
}

// isHandler reports whether fn is shaped like an http.HandlerFunc: it takes
// (http.ResponseWriter, *http.Request) and returns nothing. The result check is what keeps
// helpers that take the same pair and hand something back out of the count — they are not
// routes, and a route is what needs authorizing.
func isHandler(fn *ast.FuncDecl) bool {
	if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
		return false
	}
	params := fn.Type.Params.List
	var types []ast.Expr
	for _, p := range params {
		n := len(p.Names)
		if n == 0 {
			n = 1
		}
		for range n {
			types = append(types, p.Type)
		}
	}
	if len(types) != 2 {
		return false
	}
	return isSelector(types[0], "http", "ResponseWriter") && isStarSelector(types[1], "http", "Request")
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

func isStarSelector(e ast.Expr, pkg, name string) bool {
	star, ok := e.(*ast.StarExpr)
	return ok && isSelector(star.X, pkg, name)
}

// callsCan reports whether body contains a call to anything named Can.
func callsCan(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "Can" {
				found = true
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "Can" {
				found = true
			}
		}
		return !found
	})
	return found
}

// unauthenticated names the handlers that deliberately have no caller to authorize, as
// "<file>:<func>". It is a closed list rather than a marker comment: adding a row is an
// edit to this test, which is the review flag ADR-037 wants. A marker someone can write
// beside a new handler is an exemption that grants itself.
var unauthenticated = map[string]string{
	"health.go:live":      "liveness probe; no auth, no DB, no Docker (11 §10)",
	"health.go:ready":     "readiness probe, read by a proxy that has no session (11 §10)",
	"router.go:dispatch":  "route lookup; an unmatched path has no resource to authorize (G4)",
	"router.go:ServeHTTP": "delegates to the mux; it resolves no resource of its own",
	"permissions.go:mine": "the resource is the caller themselves and 09 §3 has no action " +
		"for it; what the answer contains is filtered through Allowed and VisibleInstances",
}

func TestEveryHandlerCallsCan(t *testing.T) {
	missing, err := handlersMissingCan(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range missing {
		if _, ok := unauthenticated[key(m)]; ok {
			continue
		}
		t.Errorf("handler does not call Can(): %s (ADR-037, 09 §4)", m)
	}
}

// key turns "live at health.go:36:1" into "health.go:live".
func key(missing string) string {
	name, pos, ok := strings.Cut(missing, " at ")
	if !ok {
		return missing
	}
	file, _, _ := strings.Cut(pos, ":")
	return filepath.Base(file) + ":" + name
}

// roleBranches reports every place under dir where a .Role field is used to decide
// something — a comparison, or a switch. Reporting a role in a payload is fine and 04 §3
// asks for it; branching on one is the bug (09 §4).
func roleBranches(dir string) ([]string, error) {
	var found []string
	fset := token.NewFileSet()

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" && dir != path {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}

		report := func(e ast.Expr) {
			if e == nil {
				return
			}
			ast.Inspect(e, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Role" {
					found = append(found, fset.Position(sel.Pos()).String())
				}
				return true
			})
		}

		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.BinaryExpr:
				report(node.X)
				report(node.Y)
			case *ast.SwitchStmt:
				report(node.Tag)
			case *ast.CaseClause:
				for _, e := range node.List {
					report(e)
				}
			}
			return true
		})
		return nil
	})
	return found, err
}

// TestNoHandlerBranchesOnARole is the other half of 09 §4's call-site discipline. A handler
// that asks "is this an admin" has re-implemented authorization beside the seam, where the
// next capability change will not reach it.
func TestNoHandlerBranchesOnARole(t *testing.T) {
	found, err := roleBranches(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range found {
		t.Errorf("handler branches on a role at %s; ask Can() instead (09 §4, F3)", at)
	}
}

// TestRoleDetectorFires checks the detector against the fixture, so a rule that has stopped
// matching anything cannot pass as a rule that is satisfied.
func TestRoleDetectorFires(t *testing.T) {
	found, err := roleBranches("testdata/authzfixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Error("detector found no role branch in the fixture that has one")
	}
}

// TestDetectorFiresOnMissingCan checks the detector itself against a known-bad fixture.
func TestDetectorFiresOnMissingCan(t *testing.T) {
	missing, err := handlersMissingCan("testdata/authzfixture")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(missing, "\n")
	if !strings.Contains(got, "handlerWithoutCan") {
		t.Errorf("detector missed the unguarded handler; found: %q", got)
	}
	if strings.Contains(got, "handlerWithCan") || strings.Contains(got, "handlerWithMethodCan") {
		t.Errorf("detector flagged a guarded handler; found: %q", got)
	}
	if strings.Contains(got, "notAHandler") {
		t.Errorf("detector flagged a non-handler; found: %q", got)
	}
}
