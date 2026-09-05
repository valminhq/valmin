//go:build integration

// The launch config becoming editable, proved against a real daemon rather than a fake: the
// unit tests in rebuild_test.go assert the same claims through runtime.Fake, where the spec
// a rebuild writes is the spec the test reads back. Here the container is a real one, and
// the argv and limits are read out of Docker's own inspect.
package api

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

// TestEditedLaunchFieldsReachTheRealContainer edits every launch field the settings screen
// offers, restarts, and reads the running container back out of Docker.
//
// A container's argv and host config are fixed at creation, so every assertion below holds
// only if the start noticed the row no longer described the container and rebuilt it —
// and the last two are the ones that make a rebuild safe rather than merely effective: the
// labels reconciliation joins on (A2) and the immutable crossplay identity (A5) must come
// through a rebuild byte for byte.
func TestEditedLaunchFieldsReachTheRealContainer(t *testing.T) {
	rt, db, d, admin := lifecycleRouter(t)
	name := "rebuild-" + nameSuffix()
	seedRealInstance(t, rt, db, d, name)

	before := containerOf(t, db, name)
	oldContainer, err := d.Inspect(t.Context(), before)
	if err != nil {
		t.Fatalf("inspect the seeded container: %v", err)
	}
	oldLabels := maps.Clone(oldContainer.Labels)

	const wantMB = 2048
	patch := as(rt, admin, httptest.NewRequest(http.MethodPatch, "/api/v1/instances/"+name,
		jsonBody(t, map[string]any{
			"server_name":  "Renamed",
			"password":     "another-secret",
			"public":       true,
			"crossplay":    true,
			"preset":       "hardcore",
			"modifiers":    map[string]string{"raids": "none"},
			"mem_limit_mb": wantMB,
		})))
	if patch.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%s)", patch.Code, patch.Body)
	}

	if final := runJob(t, rt, admin, http.MethodPost, "/api/v1/instances/"+name+"/start"); final.Status != "succeeded" {
		t.Fatalf("start job = %+v, want succeeded", final)
	}
	t.Cleanup(func() { _ = runJobQuietly(rt, admin, http.MethodPost, "/api/v1/instances/"+name+"/stop") })

	after := containerOf(t, db, name)
	if after == before {
		t.Fatal("the container id did not change, so nothing was rebuilt and this proves nothing")
	}

	live := inspectRaw(t, after)
	argv := live.Config.Cmd
	for _, want := range [][2]string{
		{"-name", "Renamed"},
		{"-password", "another-secret"},
		{"-preset", "hardcore"},
		{"-public", "1"},
	} {
		if !containsArgValue(argv, want[0], want[1]) {
			t.Errorf("argv = %v, want %s %q", argv, want[0], want[1])
		}
	}
	if !slices.Contains(argv, "-crossplay") {
		t.Errorf("argv = %v, want -crossplay", argv)
	}
	if !containsModifier(argv, "raids", "none") {
		t.Errorf("argv = %v, want -modifier raids none", argv)
	}
	if got := live.HostConfig.Memory; got != int64(wantMB)<<20 {
		t.Errorf("memory limit = %d bytes, want %d", got, int64(wantMB)<<20)
	}

	// A5. The crossplay identity is minted once and is immutable for the instance's life, so
	// turning crossplay on must not mint a new one — a server that came back under a
	// different identity is a different server to everyone trying to join it.
	if !containsArgValue(argv, "-instanceid", "cp-"+name) {
		t.Errorf("argv = %v, want -instanceid cp-%s carried through the rebuild (A5)", argv, name)
	}
	// A2. Reconciliation joins Docker to the database on these alone; a rebuild that varied
	// one would orphan the container it just created.
	for _, label := range []string{
		instance.LabelManaged, instance.LabelSchema, instance.LabelInstanceID, instance.LabelBasePort,
	} {
		if live.Config.Labels[label] != oldLabels[label] {
			t.Errorf("label %s = %q after the rebuild, want %q (A2)",
				label, live.Config.Labels[label], oldLabels[label])
		}
	}
	// A1, A3, A7: creation-time properties that cannot be set afterwards and fail silently.
	if !live.Config.OpenStdin || live.Config.StdinOnce || live.Config.Tty {
		t.Errorf("stdin/tty flags = open %v, once %v, tty %v; want true, false, false (A1)",
			live.Config.OpenStdin, live.Config.StdinOnce, live.Config.Tty)
	}
	if live.Config.User != testContainerUser {
		t.Errorf("user = %q, want %q (A3)", live.Config.User, testContainerUser)
	}
}

// containerOf reads the container id the instance row currently points at. A rebuild moves
// it, so it must be read after the start rather than captured before it.
func containerOf(t *testing.T, db *store.DB, id string) string {
	t.Helper()
	inst, err := db.InstanceByID(t.Context(), id)
	if err != nil || inst == nil {
		t.Fatalf("load instance %s: %v", id, err)
	}
	if inst.ContainerID == nil {
		t.Fatalf("instance %s has no container", id)
	}
	return *inst.ContainerID
}

// inspectRaw is Docker's own inspect, not the panel's projection of it: runtime.Container
// carries what the panel needs and the argv and the memory limit are not on it. Reading
// them here rather than widening that type keeps a production interface from growing a
// field only a test wants.
func inspectRaw(t *testing.T, containerID string) container.InspectResponse {
	t.Helper()
	cli, err := client.NewClientWithOpts(
		client.WithHost("unix:///var/run/docker.sock"), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	resp, err := cli.ContainerInspect(t.Context(), containerID)
	if err != nil {
		t.Fatalf("inspect %s: %v", containerID, err)
	}
	return resp
}

// containsModifier finds one "-modifier key value" triple, which is how 04 §2's JSON object
// reaches the argv.
func containsModifier(argv []string, key, value string) bool {
	for i := range argv[:max(len(argv)-2, 0)] {
		if argv[i] == "-modifier" && argv[i+1] == key && argv[i+2] == value {
			return true
		}
	}
	return false
}
