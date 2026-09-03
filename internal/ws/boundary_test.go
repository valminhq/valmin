package ws

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestTheHubKnowsNothingAboutTheGame is ADR-042 as a guard rather than a promise.
//
// It is an allowlist, not a ban on two package names. A denylist would pass the day
// someone reaches for internal/backup or internal/runtime instead, and the point of the
// boundary is not that Valheim is spelled out here — it is that framing, patterns,
// readiness and job semantics stay on the other side of it, where the measured facts they
// depend on already live (03 §3.5, 12 §7). Anything added to this list is a decision, and
// this test is where it gets made.
func TestTheHubKnowsNothingAboutTheGame(t *testing.T) {
	allowed := map[string]bool{
		"github.com/valminhq/valmin/internal/api/errors":     true, // the closed code registry (11 §2.5)
		"github.com/valminhq/valmin/internal/api/middleware": true, // the session already in context
		"github.com/valminhq/valmin/internal/authz":          true, // Can, and the Action set
		"github.com/valminhq/valmin/internal/store":          true, // the user a decision is made about
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(path, "github.com/valminhq/valmin/") {
				continue
			}
			if !allowed[path] {
				t.Errorf("%s imports %s; the hub is a transport (ADR-042, 14 §9)", name, path)
			}
		}
	}
}
