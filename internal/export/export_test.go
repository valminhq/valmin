package export

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// source lays out a data root with one modded server: what an export keeps and what it leaves
// for the new host to download.
func source(t *testing.T) (root string, inst Instance) {
	t.Helper()
	root = t.TempDir()
	files := map[string]string{
		"panel.db":           "database",
		"secret.key":         "0123456789abcdef0123456789abcdef",
		"rclone/rclone.conf": "[remote]",

		"instances/s1/worlds/worlds_local/Midgard.db":                            "world",
		"instances/s1/worlds/worlds_local/Midgard.fwl":                           "meta",
		"instances/s1/worlds/worlds_local/Midgard_backup_auto-20261024101010.db": "rolling",
		"instances/s1/worlds/adminlist.txt":                                      "Steam_1",
		"instances/s1/worlds/cache/Midgard_biomedatacache.bin":                   "derived",

		"instances/s1/server/valheim_server.x86_64":                  "game",
		"instances/s1/server/doorstop_config.ini":                    "framework",
		"instances/s1/server/server.cfg":                             "root config",
		"instances/s1/server/BepInEx/core/BepInEx.dll":               "framework",
		"instances/s1/server/BepInEx/plugins/Mod/Mod.dll":            "mod",
		"instances/s1/server/BepInEx/plugins/Mod/players.json":       "runtime data",
		"instances/s1/server/BepInEx/config/mod.cfg":                 "edited",
		"instances/s1/server/BepInEx/config/mod.cfg.orig":            "original",
		"instances/s1/mods-disabled/Other/BepInEx/plugins/Other.dll": "parked",
		"instances/s1/logs/old.log":                                  "log",

		"cache/steam/896660/42/valheim_server.x86_64":  "game",
		"cache/steam/896660/41/valheim_server.x86_64":  "old game",
		"cache/thunderstore/Author-Mod-1.0.0.zip":      "zip",
		"cache/thunderstore/Author-Unused-1.0.0.zip":   "unused zip",
		"backups/s1/Midgard-20261024-aaaa.tar.gz":      "backup",
		"backups/s1/Midgard-20261025-bbbb.tar.gz.part": "unfinished",
		"setups/blobs/abc":                             "blob",
		"staging/upload":                               "transient",
	}
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(
		filepath.Join(root, filepath.FromSlash("instances/s1/server/BepInEx/plugins/Mod/Mod.dll")),
		0o755,
	); err != nil {
		t.Fatal(err)
	}
	return root, Instance{ID: "s1", Managed: map[string]bool{
		"doorstop_config.ini":         true,
		"BepInEx/core/BepInEx.dll":    true,
		"BepInEx/plugins/Mod/Mod.dll": true,
		"BepInEx/config/mod.cfg":      true,
	}}
}

func plan(root string, mode Mode, inst Instance) *Plan {
	p := &Plan{
		Manifest: Manifest{Format: formatVersion, Mode: mode, Schema: 5, Panel: "v1.2.3", CreatedAt: time.Now().UTC()},
		Root:     root, Database: filepath.Join(root, "panel.db"), KeyFile: filepath.Join(root, "secret.key"),
		RcloneConfig: filepath.Join(root, "rclone", "rclone.conf"),
		Instances:    []Instance{inst},
	}
	if mode == Full {
		p.Extra = []string{
			"cache/steam/896660/42",
			"cache/thunderstore/Author-Mod-1.0.0.zip",
			"backups/s1",
			"setups/blobs",
		}
	}
	return p
}

func export(t *testing.T, p *Plan) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, p); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.Bytes()
}

// files lists every regular file under root, slash-separated.
func files(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func TestAnExportKeepsOnlyWhatCannotBeDownloaded(t *testing.T) {
	kept := []string{
		"instances/s1/mods-disabled/Other/BepInEx/plugins/Other.dll",
		"instances/s1/server/BepInEx/config/mod.cfg",
		"instances/s1/server/BepInEx/config/mod.cfg.orig",
		"instances/s1/server/BepInEx/plugins/Mod/players.json",
		"instances/s1/server/server.cfg",
		"instances/s1/worlds/adminlist.txt",
		"instances/s1/worlds/worlds_local/Midgard.db",
		"instances/s1/worlds/worlds_local/Midgard.fwl",
		"instances/s1/worlds/worlds_local/Midgard_backup_auto-20261024101010.db",
		"panel.db",
		"rclone/rclone.conf",
		"secret.key",
	}
	full := append(slices.Clone(kept),
		"backups/s1/Midgard-20261024-aaaa.tar.gz",
		"cache/steam/896660/42/valheim_server.x86_64",
		"cache/thunderstore/Author-Mod-1.0.0.zip",
		"setups/blobs/abc",
	)
	slices.Sort(full)

	for _, tc := range []struct {
		mode Mode
		want []string
	}{{Light, kept}, {Full, full}} {
		t.Run(string(tc.mode), func(t *testing.T) {
			src, inst := source(t)
			dest := t.TempDir()
			m, err := Restore(bytes.NewReader(export(t, plan(src, tc.mode, inst))), dest, 5)
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if err := FinishRestore(dest); err != nil {
				t.Fatal(err)
			}
			if m.Mode != tc.mode || m.Panel != "v1.2.3" {
				t.Errorf("manifest = %+v, want mode %s from v1.2.3", m, tc.mode)
			}
			if got := files(t, dest); !slices.Equal(got, tc.want) {
				t.Errorf("restored files:\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

func TestARestoreKeepsContentAndPermissions(t *testing.T) {
	src, inst := source(t)
	inst.Managed = nil
	dest := t.TempDir()
	if _, err := Restore(bytes.NewReader(export(t, plan(src, Light, inst))), dest, 5); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash("instances/s1/server/BepInEx/config/mod.cfg")))
	if err != nil || string(got) != "edited" {
		t.Errorf("mod.cfg = %q, %v; want the edited config", got, err)
	}
	for name, want := range map[string]fs.FileMode{
		"secret.key": 0o600,
		"instances/s1/server/BepInEx/plugins/Mod/Mod.dll": 0o755,
	} {
		fi, err := os.Stat(filepath.Join(dest, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v", name, fi.Mode().Perm(), want)
		}
	}
}

func TestARestoreRefusesANonEmptyDataRoot(t *testing.T) {
	src, inst := source(t)
	if _, err := Restore(bytes.NewReader(export(t, plan(src, Light, inst))), src, 5); err == nil {
		t.Fatal("Restore into a used data root succeeded")
	}
}

func TestARestoreRefusesANewerSchema(t *testing.T) {
	src, inst := source(t)
	dest := t.TempDir()
	_, err := Restore(bytes.NewReader(export(t, plan(src, Light, inst))), dest, 4)
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("Restore = %v, want a newer-schema refusal", err)
	}
	if left := files(t, dest); len(left) != 0 {
		t.Errorf("refused restore left %v", left)
	}
}

// archive builds an export whose entries after the manifest are hdrs, each with body "x".
func archive(t *testing.T, hdrs ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	raw, _ := json.Marshal(Manifest{Format: formatVersion, Mode: Light})
	if err := writeBytes(tw, ManifestName, raw); err != nil {
		t.Fatal(err)
	}
	for _, hdr := range hdrs {
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = 1
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte("x"))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestARestoreRefusesEntriesOutsideTheRootAndUndoesItself(t *testing.T) {
	for name, bad := range map[string]*tar.Header{
		"parent":   {Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o600},
		"absolute": {Name: "/etc/escape", Typeflag: tar.TypeReg, Mode: 0o600},
		"symlink":  {Name: "instances/link", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
		"hardlink": {Name: "instances/link", Typeflag: tar.TypeLink, Linkname: "panel.db"},
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "root")
			if err := os.Mkdir(dest, 0o750); err != nil {
				t.Fatal(err)
			}
			good := &tar.Header{Name: "panel.db", Typeflag: tar.TypeReg, Mode: 0o600}
			if _, err := Restore(bytes.NewReader(archive(t, good, bad)), dest, 5); err == nil {
				t.Fatal("Restore accepted the entry")
			}
			if left := files(t, parent); len(left) != 0 {
				t.Errorf("refused restore left %v", left)
			}
		})
	}
}

func TestARestoreRefusesAnArchiveWithoutAManifest(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = writeBytes(tw, "panel.db", []byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	if _, err := Restore(&buf, t.TempDir(), 5); err == nil {
		t.Fatal("Restore accepted an archive with no manifest")
	}
}

// Asserts a restore cut short is redone from scratch, and that the lock file an admin command
// leaves in a fresh data root does not count as content.
func TestAnUnfinishedRestoreIsRedone(t *testing.T) {
	src, inst := source(t)
	data := export(t, plan(src, Light, inst))
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, ".valmind.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(bytes.NewReader(data[:len(data)/2]), dest, 5); err == nil {
		t.Fatal("Restore of a truncated archive succeeded")
	}
	// A restore killed mid-unpack leaves its files and the mark behind.
	if err := os.WriteFile(filepath.Join(dest, IncompleteMark), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "panel.db"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !RestoreIncomplete(dest) {
		t.Fatal("RestoreIncomplete = false with the mark in place")
	}

	if _, err := Restore(bytes.NewReader(data), dest, 5); err != nil {
		t.Fatalf("Restore over an unfinished one: %v", err)
	}
	if err := FinishRestore(dest); err != nil {
		t.Fatal(err)
	}
	if RestoreIncomplete(dest) {
		t.Error("the mark outlived FinishRestore")
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "panel.db")); string(got) != "database" {
		t.Errorf("panel.db = %q, want the archive's", got)
	}
}

func TestACancelledExportStopsWriting(t *testing.T) {
	w := &checkedWriter{w: io.Discard, check: func() error { return errCancelled }}
	if _, err := w.Write(make([]byte, 1024)); err != nil {
		t.Fatalf("first write = %v, want it through before the first check", err)
	}
	if _, err := w.Write(make([]byte, checkBytes)); !errors.Is(err, errCancelled) {
		t.Fatalf("write past the check = %v, want errCancelled", err)
	}
}

// Asserts the key and rclone config are read where the operator put them, through a symlink,
// and archived at the paths a restore unpacks them to.
func TestAnExportFollowsOperatorPaths(t *testing.T) {
	src, inst := source(t)
	elsewhere := t.TempDir()
	key := filepath.Join(elsewhere, "key")
	if err := os.Rename(filepath.Join(src, "secret.key"), key); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(key, filepath.Join(src, "secret.key")); err != nil {
		t.Fatal(err)
	}
	rclone := filepath.Join(elsewhere, "rclone.conf")
	if err := os.Rename(filepath.Join(src, "rclone", "rclone.conf"), rclone); err != nil {
		t.Fatal(err)
	}
	p := plan(src, Light, inst)
	p.RcloneConfig = rclone

	dest := t.TempDir()
	if _, err := Restore(bytes.NewReader(export(t, p)), dest, 5); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for name, want := range map[string]string{KeyName: "0123456789abcdef0123456789abcdef", RcloneConfigName: "[remote]"} {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v, want %q", name, got, err, want)
		}
	}
}

func TestSweepRemovesOnlyUnfinishedExports(t *testing.T) {
	root := t.TempDir()
	dir := Dir(root)
	finished := filePrefix + "light-20261025T100000Z.tar.gz"
	for _, name := range []string{finished, finished + ".part", workPrefix + "123/panel.db"} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Sweep(root); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := files(t, dir); !slices.Equal(got, []string{finished}) {
		t.Errorf("after Sweep = %v, want only %s", got, finished)
	}
	if err := Sweep(t.TempDir()); err != nil {
		t.Errorf("Sweep without an exports directory = %v", err)
	}
}
