//go:build integration

// Verifies the shipped deployment against the tools that will read it: `docker compose
// config` renders compose.yaml the way the daemon will see it, and Caddy's own adapter
// parses the Caddyfile. Neither needs the stack to be running, and both catch the drift that
// is otherwise invisible until an operator's first boot.
package deploy_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// env is a complete deployment, so `config` resolves every required variable. The values are
// deliberately not the defaults: a placeholder that happens to match one would hide a
// variable the compose file forgot to pass through.
var env = []string{
	"VALMIN_IMAGE=example.invalid/valmind@sha256:" + strings.Repeat("a", 64),
	"VALMIN_GAME_IMAGE=example.invalid/valheim@sha256:" + strings.Repeat("b", 64),
	"VALMIN_HOST_DATA_ROOT=/srv/valmin-elsewhere",
	"VALMIN_DOMAIN=panel.example.invalid",
}

// rendered is compose.yaml as docker compose resolves it.
type rendered struct {
	Services map[string]struct {
		Image       string            `json:"image"`
		User        string            `json:"user"`
		GroupAdd    []string          `json:"group_add"`
		Environment map[string]string `json:"environment"`
		Ports       []struct {
			Published any `json:"published"`
		} `json:"ports"`
		Volumes []struct {
			Source string `json:"source"`
			Target string `json:"target"`
		} `json:"volumes"`
		Networks map[string]struct {
			IPv4Address string `json:"ipv4_address"`
		} `json:"networks"`
	} `json:"services"`
}

func config(t *testing.T) rendered {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", "compose.yaml", "config", "--format", "json")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose config: %v\n%s", err, out)
	}
	var got rendered
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse rendered compose file: %v", err)
	}
	return got
}

// TestThePanelCannotReachTheSocketItself guards ADR-170 and A3. The panel drives Docker
// through the proxy, so the socket is in a container the panel has no path to: no bind mount
// and no group that could open one.
func TestThePanelCannotReachTheSocketItself(t *testing.T) {
	svc := config(t).Services
	panel := svc["valmind"]
	if panel.User != "10000:10000" {
		t.Errorf("user = %q, want 10000:10000", panel.User)
	}
	if len(panel.GroupAdd) != 0 {
		t.Errorf("group_add = %v; the panel needs no socket group once it talks to the proxy", panel.GroupAdd)
	}
	for _, v := range panel.Volumes {
		if strings.Contains(v.Source, "docker.sock") {
			t.Errorf("the panel mounts %s; the proxy exists so that it does not", v.Source)
		}
	}
	if got := panel.Environment["VALMIN_DOCKER_ENDPOINT"]; !strings.HasPrefix(got, "tcp://docker-proxy:") {
		t.Errorf("docker endpoint = %q, want the proxy", got)
	}

	proxy := svc["docker-proxy"]
	var mounted bool
	for _, v := range proxy.Volumes {
		if strings.Contains(v.Source, "docker.sock") {
			mounted = true
		}
	}
	if !mounted {
		t.Error("the proxy does not mount the socket, so nothing can drive Docker at all")
	}
	if len(proxy.Ports) != 0 {
		t.Errorf("the proxy publishes %v; it is reachable from the panel and nothing else", proxy.Ports)
	}
}

// TestTheProxyGrantsOnlyTheDeclaredSurface guards Q3's answer: the permission set is exactly
// what internal/runtime calls, and internal/runtime/surface_test.go is the other half of that
// claim. Anything switched on here that the panel does not use is surface bought for nothing.
func TestTheProxyGrantsOnlyTheDeclaredSurface(t *testing.T) {
	granted := config(t).Services["docker-proxy"].Environment
	want := map[string]string{"CONTAINERS": "1", "IMAGES": "1", "POST": "1", "PING": "1", "VERSION": "1"}

	for key, value := range granted {
		if value != "1" {
			continue
		}
		if _, ok := want[key]; !ok {
			t.Errorf("the proxy grants %s, which the panel never calls", key)
		}
	}
	for key := range want {
		if granted[key] != "1" {
			t.Errorf("the proxy denies %s, which the panel needs on every start", key)
		}
	}
	if granted["EVENTS"] != "0" {
		t.Error("EVENTS is on; it is default-on in this image and describes containers that are not the panel's")
	}
}

// TestTheTrustedProxyIsExactlyTheProxy guards D9. A CIDR wider than the proxy's own address
// is a header anyone inside it can spoof past the invite rate limiter, and the two values
// live in different halves of the file, so nothing but a test keeps them in step.
func TestTheTrustedProxyIsExactlyTheProxy(t *testing.T) {
	svc := config(t).Services
	proxy := svc["caddy"].Networks["valmin"].IPv4Address
	if proxy == "" {
		t.Fatal("the proxy has no static address; the trusted-proxy value below cannot be knowable")
	}
	if got := svc["valmind"].Environment["VALMIN_SERVER_TRUSTED_PROXIES"]; got != proxy+"/32" {
		t.Errorf("trusted proxies = %q, want exactly the proxy at %s", got, proxy)
	}
	// Both addresses are static or neither is: Docker hands out the first free address in
	// the subnet, so a dynamic panel takes the proxy's and whichever starts second fails to
	// attach — with "Address already in use", which names neither service.
	if svc["valmind"].Networks["valmin"].IPv4Address == "" {
		t.Error("the panel has no static address; it can take the proxy's on start")
	}
}

// TestThePanelPublishesNoPort guards 02 §5: the panel speaks plain HTTP and is reachable
// only through the proxy, so nothing but the proxy binds a host port.
func TestThePanelPublishesNoPort(t *testing.T) {
	svc := config(t).Services
	if got := svc["valmind"].Ports; len(got) != 0 {
		t.Errorf("the panel publishes %v; only the proxy may bind a host port", got)
	}
	if len(svc["caddy"].Ports) == 0 {
		t.Error("the proxy publishes nothing, so the panel is unreachable")
	}
}

// TestTheHostPathIsPassedAsTheHostSeesIt guards C22 at the layer that gets it wrong: the
// bind mount's source and the path the panel is told about must be the same host path, since
// a mismatch is a container that starts and generates a brand new world.
func TestTheHostPathIsPassedAsTheHostSeesIt(t *testing.T) {
	panel := config(t).Services["valmind"]
	var mounted string
	for _, v := range panel.Volumes {
		if v.Target == "/srv/valmin" {
			mounted = v.Source
		}
	}
	if mounted != "/srv/valmin-elsewhere" {
		t.Fatalf("data root mounted from %q, want the configured host path", mounted)
	}
	if got := panel.Environment["VALMIN_DATA_HOST_ROOT"]; got != mounted {
		t.Errorf("data.host_root = %q but the mount comes from %q", got, mounted)
	}
	if got := panel.Environment["VALMIN_DATA_ROOT"]; got != "/srv/valmin" {
		t.Errorf("data.root = %q, want the path inside the container", got)
	}
}

// TestTheCaddyfileParses runs Caddy's own adapter. An empty templated value that is a parse
// error — which is how the optional ACME address was first written — takes the whole stack
// down on a restart months later.
func TestTheCaddyfileParses(t *testing.T) {
	image := "caddy:2-alpine"
	out, err := exec.Command("docker", "run", "--rm",
		"-e", "VALMIN_DOMAIN=panel.example.invalid", "-e", "VALMIN_TLS=",
		"-v", wd(t)+"/Caddyfile:/etc/caddy/Caddyfile:ro", image,
		"caddy", "validate", "--config", "/etc/caddy/Caddyfile").CombinedOutput()
	if err != nil {
		t.Fatalf("caddy validate: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Valid configuration") {
		t.Errorf("caddy did not report a valid configuration:\n%s", out)
	}
}

func wd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	return dir
}
