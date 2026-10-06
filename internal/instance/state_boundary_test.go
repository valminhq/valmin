package instance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestInstanceStateWritesUseTheValidatedBoundary asserts that no package outside store writes
// instances.state except through the validated helpers in state.go.
func TestInstanceStateWritesUseTheValidatedBoundary(t *testing.T) {
	forbidden := map[string]bool{
		"TxUpdateInstanceState":      true,
		"UpdateInstanceState":        true,
		"UpdateInstanceStateAudited": true,
		"TxFinishProvisioning":       true,
		"TxFinishStart":              true,
	}
	root := repoRoot(t)
	var found []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == filepath.Join("internal", "instance", "state.go") ||
			strings.HasPrefix(rel, filepath.Join("internal", "store")+string(filepath.Separator)) {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
				value, unquoteErr := strconv.Unquote(literal.Value)
				if unquoteErr == nil && strings.Contains(
					strings.ToUpper(strings.Join(strings.Fields(value), " ")), "UPDATE INSTANCES SET STATE",
				) {
					found = append(found, fset.Position(literal.Pos()).String())
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && forbidden[sel.Sel.Name] {
				found = append(found, fset.Position(call.Pos()).String())
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("instance state writes bypass the validated helpers in internal/instance/state.go: %v", found)
	}
}

// TestTransactionalStateHelpersRejectIllegalEdgesBeforeWriting asserts that each helper
// validates the edge before it touches the transaction.
func TestTransactionalStateHelpersRejectIllegalEdgesBeforeWriting(t *testing.T) {
	if _, err := SetStateTx(t.Context(), nil, "inst-a", StateRunning, StateProvisioning); err == nil {
		t.Fatal("SetStateTx accepted running -> provisioning")
	}
	if err := FinishProvisioningTx(
		t.Context(), nil, "inst-a", StateRunning, StateProvisioning, "container", "build",
	); err == nil {
		t.Fatal("FinishProvisioningTx accepted running -> provisioning")
	}
	if err := FinishStartTx(t.Context(), nil, "inst-a", StateRunning, StateProvisioning); err == nil {
		t.Fatal("FinishStartTx accepted running -> provisioning")
	}
	if _, err := SetStateAudited(t.Context(), nil, "inst-a", StateRunning, StateProvisioning, nil); err == nil {
		t.Fatal("SetStateAudited accepted running -> provisioning")
	}
}
