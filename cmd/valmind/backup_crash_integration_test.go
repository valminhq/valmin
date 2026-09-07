//go:build integration

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// containerCommand runs the same daemon binary in Docker so the crash uses docker kill.
// Identical host and container data paths preserve the startup bind-mount round trip.
func (p *panel) containerCommand(env map[string]string) *exec.Cmd {
	p.t.Helper()
	socket, err := os.Stat("/var/run/docker.sock")
	if err != nil {
		p.t.Fatal(err)
	}
	args := []string{
		"run", "--rm", "--name", p.containerName, "--network", "host",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--group-add", fmt.Sprint(socket.Sys().(*syscall.Stat_t).Gid),
		"--volume", p.root + ":" + p.root,
		"--volume", valmind(p.t) + ":/valmind:ro",
		"--volume", "/var/run/docker.sock:/var/run/docker.sock",
		"--entrypoint", "/valmind",
	}
	for key, value := range env {
		args = append(args, "--env", key+"="+value)
	}
	args = append(args, stubImage)
	return exec.Command("docker", args...)
}

func (p *panel) dockerCommand(args ...string) {
	p.t.Helper()
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		p.t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
}

// A hot copy can lose both live processes during archiving; quiescing has already
// stopped the game by that point. The second case proves the durable restart obligation.
func TestBackupCrashPreservesWorldAndRecoversIntent(t *testing.T) {
	for _, mode := range []string{"hot", "quiesced"} {
		t.Run(mode, func(t *testing.T) {
			p := newPanel(t, nil)
			p.containerName = "valmin-backup-crash-" + suffix()
			d := docker(t)
			id, containerID := seedInstanceWithWorldBind(t, p, d, "backup-crash", true)
			world := filepath.Join(p.root, "instances", id, "worlds", "worlds_local")
			before := writeCrashWorld(t, world)
			p.start()
			p.setup()
			if job := p.awaitJob(p.submit("/api/v1/instances/" + id + "/start")); job.Status != "succeeded" {
				t.Fatalf("start = %+v", job)
			}
			started := inspect(t, d, containerID).StartedAt
			jobID := p.submit("/api/v1/instances/" + id + "/backups?mode=" + mode)
			db := openPanelDB(t, p)
			defer func() { _ = db.Close() }()
			part := awaitBackupPart(t, p, db, jobID)
			if running := inspect(t, d, containerID).Running; running != (mode == "hot") {
				t.Fatalf("game running during %s archive = %t", mode, running)
			}
			p.kill()
			if mode == "hot" {
				p.dockerCommand("kill", containerID)
			}
			if p.healthy() || inspect(t, d, containerID).Running {
				t.Fatal("a process survived the crash")
			}
			job, err := db.JobByID(t.Context(), jobID)
			if err != nil {
				t.Fatal(err)
			}
			if job == nil || job.Status != "running" || job.ResumeAfter != (mode == "quiesced") {
				t.Fatalf("crash did not interrupt the intended job: %+v", job)
			}
			if _, err := os.Stat(part); err != nil {
				t.Fatalf("no interrupted part: %v", err)
			}
			if _, err := os.Stat(strings.TrimSuffix(part, ".part")); !os.IsNotExist(err) {
				t.Fatalf("interrupted archive was published: %v", err)
			}
			assertCrashCatalogueEmpty(t, db, id)
			assertCrashWorld(t, world, before)
			p.restart()
			if job := p.awaitJob(
				jobID,
			); job.Status != "failed" || job.ErrorCode == nil ||
				*job.ErrorCode != "interrupted" {
				t.Fatalf("recovered backup = %+v", job)
			}
			if _, err := os.Stat(part); !os.IsNotExist(err) {
				t.Fatalf("part survived startup: %v", err)
			}
			assertCrashCatalogueEmpty(t, db, id)
			assertCrashWorld(t, world, before)
			want := "stopped"
			if mode == "quiesced" {
				want = "running"
			}
			p.awaitState(id, want)
			if mode == "quiesced" {
				after := inspect(t, d, containerID)
				if !after.Running || !after.StartedAt.After(started) {
					t.Fatal("resume intent did not start the game")
				}
				var starts int
				if err := db.Reader.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM job_runs WHERE instance_id = ? AND kind = 'start' AND status = 'succeeded'`, id).
					Scan(&starts); err != nil {
					t.Fatal(err)
				}
				if starts != 2 {
					t.Fatalf("succeeded start jobs = %d, want initial and recovery starts", starts)
				}
			} else if inspect(t, d, containerID).Running {
				t.Fatal("hot-copy crash acquired a resume intent")
			}
		})
	}
}

// Incompressible bytes hold the real compressor open long enough to land the kill.
// A completed job or missing part fails the premise instead of passing vacuously.
func writeCrashWorld(t *testing.T, world string) map[string]string {
	t.Helper()
	if err := os.MkdirAll(world, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int64{"World.db": 128 << 20, "World.fwl": 1024} {
		f, err := os.Create(filepath.Join(world, name))
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.CopyN(f, rand.Reader, size)
		closeErr := f.Close()
		if copyErr != nil {
			t.Fatal(copyErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	return crashWorldHashes(t, world)
}

func crashWorldHashes(t *testing.T, world string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(world)
	if err != nil {
		t.Fatal(err)
	}
	hashes := make(map[string]string, len(entries))
	for _, entry := range entries {
		f, err := os.Open(filepath.Join(world, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			t.Fatal(copyErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		hashes[entry.Name()] = hex.EncodeToString(h.Sum(nil))
	}
	return hashes
}

func assertCrashWorld(t *testing.T, world string, before map[string]string) {
	t.Helper()
	after := crashWorldHashes(t, world)
	if len(after) != len(before) {
		t.Fatalf("world file count changed: %v", after)
	}
	for name, hash := range before {
		if after[name] != hash {
			t.Fatalf("world file %s changed", name)
		}
	}
}

func assertCrashCatalogueEmpty(t *testing.T, db *store.DB, id string) {
	t.Helper()
	var count int
	if err := db.Reader.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM backups WHERE instance_id = ?", id).
		Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("interrupted backup has %d catalogue rows", count)
	}
}

func awaitBackupPart(t *testing.T, p *panel, db *store.DB, jobID string) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		job, err := db.JobByID(t.Context(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		if job != nil {
			if job.Status != "running" {
				t.Fatalf("backup finished before crash: %+v\n%s", job, p.out.String())
			}
			var payload struct {
				Dest string `json:"dest"`
			}
			if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
				t.Fatal(err)
			}
			part := payload.Dest + ".part"
			if info, err := os.Stat(part); err == nil && info.Size() > 0 {
				return part
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("backup never wrote a part file")
	return ""
}
