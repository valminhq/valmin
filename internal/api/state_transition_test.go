package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
)

func TestInstanceStateWritesUseTheValidatedBoundary(t *testing.T) {
	forbidden := map[string]bool{
		"TxUpdateInstanceState": true,
		"UpdateInstanceState":   true,
		"TxFinishProvisioning":  true,
		"TxFinishStart":         true,
	}
	var found []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasSuffix(path, "_test.go") || path == "state_transition.go" ||
			!strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if ok && literal.Kind == token.STRING {
				value, unquoteErr := strconv.Unquote(literal.Value)
				if unquoteErr == nil && strings.Contains(
					strings.ToUpper(strings.Join(strings.Fields(value), " ")),
					"UPDATE INSTANCES SET STATE",
				) {
					found = append(found, fset.Position(literal.Pos()).String())
				}
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if ok && forbidden[sel.Sel.Name] {
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
		t.Fatalf("instance state writes bypass the validated boundary: %v", found)
	}
}

func TestTransactionalStateHelpersRejectIllegalEdgesBeforeWriting(t *testing.T) {
	if _, err := setStateTx(
		t.Context(), nil, "inst-a", instance.StateRunning, instance.StateProvisioning,
	); err == nil {
		t.Fatal("setStateTx accepted running -> provisioning")
	}
	if err := finishProvisioningState(
		t.Context(), nil, "inst-a", instance.StateRunning, instance.StateProvisioning, "container", "build",
	); err == nil {
		t.Fatal("finishProvisioningState accepted running -> provisioning")
	}
	if err := finishStartState(
		t.Context(), nil, "inst-a", instance.StateRunning, instance.StateProvisioning,
	); err == nil {
		t.Fatal("finishStartState accepted running -> provisioning")
	}
}
