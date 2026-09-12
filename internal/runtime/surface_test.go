package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// The Docker API surface the panel uses, and the permission each call needs from a socket
// proxy (02 §6, Q3). The set is closed: a call added here without a matching proxy
// permission is a panel that works on a raw socket and fails in production, which is the
// failure mode a proxy is most likely to introduce.
//
// Everything the proxy can deny — exec, build, images beyond inspection, networks, volumes,
// secrets, configs, swarm, system, events — is absent by construction rather than by policy,
// because nothing in this file calls it.
var dockerSurface = map[string]string{
	"Ping":                  "GET /_ping (PING)",
	"Close":                 "no request",
	"ContainerCreate":       "POST /containers/create (CONTAINERS, POST)",
	"ContainerStart":        "POST /containers/{id}/start (CONTAINERS, POST)",
	"ContainerStop":         "POST /containers/{id}/stop (CONTAINERS, POST)",
	"ContainerWait":         "POST /containers/{id}/wait (CONTAINERS, POST)",
	"ContainerRemove":       "DELETE /containers/{id} (CONTAINERS, POST)",
	"ContainerInspect":      "GET /containers/{id}/json (CONTAINERS)",
	"ContainerList":         "GET /containers/json (CONTAINERS)",
	"ContainerLogs":         "GET /containers/{id}/logs (CONTAINERS)",
	"ContainerStatsOneShot": "GET /containers/{id}/stats (CONTAINERS)",
	"ImageInspect":          "GET /images/{name}/json (IMAGES)",
}

// TestTheDockerSurfaceIsTheDeclaredOne asserts the adapter calls exactly what the socket
// proxy is configured to allow, no more. The panel never pulls an image — it inspects one,
// and a missing image is a refusal that names it (ADR-048) — so IMAGES stays read-only and
// BUILD is never needed.
func TestTheDockerSurfaceIsTheDeclaredOne(t *testing.T) {
	called := dockerCalls(t)

	for _, name := range called {
		if _, ok := dockerSurface[name]; !ok {
			t.Errorf("client.%s is called but is not in the declared surface; "+
				"add it here and to the socket proxy's permissions, or it fails in production", name)
		}
	}
	for name := range dockerSurface {
		if !slices.Contains(called, name) {
			t.Errorf("client.%s is declared but no longer called; the proxy grants more than the panel uses", name)
		}
	}
}

// dockerCalls reports every method invoked on the embedded Docker client.
func dockerCalls(t *testing.T) []string {
	t.Helper()

	var out []string
	for _, file := range []string{"docker.go", "throwaway.go", "runtime.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if !isClient(sel.X) {
				return true
			}
			if !slices.Contains(out, sel.Sel.Name) {
				out = append(out, sel.Sel.Name)
			}
			return true
		})
	}
	return out
}

// isClient reports whether expr is the Docker client — `d.cli` on the adapter, or the local
// `cli` the constructor holds before the adapter exists.
func isClient(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name == "cli"
	case *ast.Ident:
		return e.Name == "cli"
	}
	return false
}

// TestTheSurfaceNeedsNoPrivilegedEndpoint is the negative control: the surface above is only
// meaningful while it excludes the endpoints that make a proxy pointless. Exec in particular
// is a shell in a container the panel owns, which is the panel's own trust boundary
// re-crossed from the other side.
func TestTheSurfaceNeedsNoPrivilegedEndpoint(t *testing.T) {
	for _, forbidden := range []string{
		"ContainerExec", "ContainerAttach", "ImageBuild", "ImagePull", "ImagePush",
		"NetworkCreate", "VolumeCreate", "SecretCreate", "ConfigCreate", "SwarmInit",
		"Info", "Events", "ContainerCommit",
	} {
		for name := range dockerSurface {
			if strings.HasPrefix(name, forbidden) {
				t.Errorf("%s is in the declared surface; the proxy cannot then deny %s",
					name, forbidden)
			}
		}
	}
}
