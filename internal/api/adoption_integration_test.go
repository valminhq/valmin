//go:build integration

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
)

func TestAdoptionPreservesARealContainer(t *testing.T) {
	t.Parallel()
	for _, running := range []bool{false, true} {
		name := "stopped"
		if running {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			rt, db, docker, admin := lifecycleRouter(t)
			cfg := rt.Supervisor().inst.Cfg
			if err := os.Chmod(cfg.Data.Root, 0o755); err != nil {
				t.Fatalf("make data root searchable: %v", err)
			}
			basePort, err := instance.NewAllocator(db, nil, cfg.Ports.Base, cfg.Ports.Stride).Allocate(t.Context())
			if err != nil {
				t.Fatalf("allocate adoption ports: %v", err)
			}
			instanceID := "e2e-adopt-" + nameSuffix()
			crossplayID := "crossplay-" + nameSuffix()
			dataDir := filepath.Join(cfg.Data.HostRoot, "instances", instanceID)
			worldDir := filepath.Join(dataDir, "worlds", instance.WorldsLocalDir)
			for _, dir := range []string{
				filepath.Join(dataDir, "server", "steamapps"), worldDir, filepath.Join(dataDir, "logs"),
			} {
				if err := os.MkdirAll(dir, 0o777); err != nil {
					t.Fatalf("create adoption directory: %v", err)
				}
				if err := os.Chmod(dir, 0o777); err != nil {
					t.Fatalf("make adoption directory writable: %v", err)
				}
			}
			manifest := filepath.Join(dataDir, "server", "steamapps", "appmanifest_896660.acf")
			if err := os.WriteFile(manifest,
				[]byte(`"AppState" { "appid" "896660" "buildid" "123456" }`), 0o666); err != nil {
				t.Fatalf("write installed manifest: %v", err)
			}
			worldMarker := filepath.Join(worldDir, "RecoveredWorld.fwl")
			if err := os.WriteFile(worldMarker, []byte("existing world"), 0o666); err != nil {
				t.Fatalf("write existing world: %v", err)
			}
			if err := os.WriteFile(filepath.Join(worldDir, "RecoveredWorld.db"),
				[]byte("existing database"), 0o666); err != nil {
				t.Fatalf("write existing database: %v", err)
			}

			launch := &instance.LaunchSpec{
				InstanceID: instanceID, DataDir: dataDir, BasePort: basePort,
				ServerName: "Recovered Server", WorldName: "RecoveredWorld", Password: adoptionPassword,
				CrossplayInstanceID: crossplayID, MemLimitMB: seededMemLimitMB,
			}
			spec, err := instance.BuildSpec(launch, cfg.Game.Image, cfg.Game.StopTimeout.Std())
			if err != nil {
				t.Fatalf("build managed container: %v", err)
			}
			containerID, err := docker.Create(t.Context(), spec)
			if err != nil {
				t.Fatalf("create managed orphan: %v", err)
			}
			t.Cleanup(func() {
				ctx := context.Background()
				found, listErr := docker.List(ctx, map[string]string{instance.LabelInstanceID: instanceID})
				if listErr != nil {
					t.Errorf("cleanup: list adopted containers: %v", listErr)
					return
				}
				for i := range found {
					if removeErr := docker.Remove(ctx, found[i].ID, true); removeErr != nil {
						t.Errorf("cleanup: remove adopted container: %v", removeErr)
					}
				}
			})
			if running {
				if err := docker.Start(t.Context(), containerID); err != nil {
					t.Fatalf("start managed orphan: %v", err)
				}
			}

			before, err := docker.Inspect(t.Context(), containerID)
			if err != nil {
				t.Fatalf("inspect managed orphan: %v", err)
			}
			if before.ImageDefaults == nil || len(before.Spec.Env) <= len(spec.Env) ||
				!slices.ContainsFunc(before.Spec.Env, func(value string) bool {
					return strings.HasPrefix(value, "PATH=")
				}) {
				t.Fatalf("inspect did not expose image-inherited environment: %+v", before)
			}

			body := map[string]any{
				"name": "Recovered " + name, "server_name": launch.ServerName,
				"world_name": launch.WorldName, "password": launch.Password,
				"public": launch.Public, "crossplay": launch.Crossplay,
				"preset": "", "modifiers": map[string]string{}, "extra_args": "",
				"mem_limit_mb": launch.MemLimitMB, "cpu_limit": nil,
			}
			rec := as(rt, admin, httptest.NewRequest(
				http.MethodPost, adoptionPath(containerID), jsonBody(t, body)))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("adopt real container = %d, want 202 (%s)", rec.Code, rec.Body)
			}
			var accepted jobView
			decodeInto(t, rec, &accepted)
			if final := waitForJobTerminal(t, rt, admin, accepted.JobID); final.Status != "succeeded" {
				t.Fatalf("adoption job = %+v, want succeeded", final)
			}

			adopted, err := db.InstanceByID(t.Context(), instanceID)
			if err != nil || adopted == nil {
				t.Fatalf("read adopted instance: %v", err)
			}
			wantState := "stopped"
			if running {
				wantState = "running"
			}
			if adopted.ContainerID == nil || *adopted.ContainerID != containerID ||
				adopted.State != wantState || adopted.DataDir != dataDir ||
				adopted.BasePort != basePort || adopted.CrossplayInstanceID != crossplayID {
				t.Errorf("adopted row = %+v, want existing container identity in %s", adopted, wantState)
			}
			after, err := docker.Inspect(t.Context(), containerID)
			if err != nil {
				t.Fatalf("inspect adopted container: %v", err)
			}
			if after.Running != running {
				t.Errorf("container running = %v, want unchanged %v", after.Running, running)
			}
			found, err := docker.List(t.Context(), map[string]string{instance.LabelInstanceID: instanceID})
			if err != nil {
				t.Fatalf("list adopted containers: %v", err)
			}
			if len(found) != 1 || found[0].ID != containerID {
				t.Errorf("containers after adoption = %+v, want only %s", found, containerID)
			}
			if got, err := os.ReadFile(worldMarker); err != nil || string(got) != "existing world" {
				t.Errorf("world marker = %q, %v; want unchanged", got, err)
			}
		})
	}
}
