package errcode

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// registryRows reads the Code literals out of errors.go rather than a second list kept
// beside them. A second list is a second place to forget a row.
func registryRows(t *testing.T) []Code {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "code.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var rows []Code
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "Code" {
			return true
		}
		if len(lit.Elts) != 3 {
			t.Fatalf("Code literal has %d fields, want name, status, message", len(lit.Elts))
		}
		rows = append(rows, Code{
			name:    unquote(t, lit.Elts[0]),
			status:  atoi(t, lit.Elts[1]),
			message: unquote(t, lit.Elts[2]),
		})
		return true
	})
	return rows
}

func unquote(t *testing.T, e ast.Expr) string {
	t.Helper()
	lit, ok := e.(*ast.BasicLit)
	if !ok {
		t.Fatalf("want a literal, got %T", e)
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func atoi(t *testing.T, e ast.Expr) int {
	t.Helper()
	lit, ok := e.(*ast.BasicLit)
	if !ok {
		t.Fatalf("want a literal, got %T", e)
	}
	n, err := strconv.Atoi(lit.Value)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRegistryIsWellFormed guards the closed registry of ADR-034. A duplicate name would
// put two different failures on the wire under one code, which no client could tell apart.
func TestRegistryIsWellFormed(t *testing.T) {
	rows := registryRows(t)
	if len(rows) < 30 {
		t.Fatalf("found %d codes, want the whole 11 §2.5 table", len(rows))
	}

	seen := map[string]bool{}
	for _, c := range rows {
		if seen[c.name] {
			t.Errorf("duplicate code %q", c.name)
		}
		seen[c.name] = true

		if c.name != strings.ToLower(c.name) || strings.ContainsAny(c.name, " -.") {
			t.Errorf("code %q is not lower snake_case (11 §1.1)", c.name)
		}
		if c.message == "" {
			t.Errorf("code %q has no message; every code is renderable to any caller", c.name)
		}
		if c.status != 0 && http.StatusText(c.status) == "" {
			t.Errorf("code %q has status %d, which is not an HTTP status", c.name, c.status)
		}
	}
}

// TestJobOnlyCodesHaveNoStatus holds the 11 §2.5 note that job_runs.error_code draws from
// this table, and that a job failing has no HTTP status to report.
func TestJobOnlyCodesHaveNoStatus(t *testing.T) {
	for _, c := range []Code{Interrupted, Stalled} {
		if c.Status() != 0 {
			t.Errorf("%s has status %d, want none", c, c.Status())
		}
	}
}
