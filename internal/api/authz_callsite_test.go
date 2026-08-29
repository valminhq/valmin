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

// isHandler reports whether fn takes (http.ResponseWriter, *http.Request).
func isHandler(fn *ast.FuncDecl) bool {
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
	"health.go:live":  "liveness probe; no auth, no DB, no Docker (11 §10)",
	"health.go:ready": "readiness probe, read by a proxy that has no session (11 §10)",
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
