package remote

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func driveSection(name string) RcloneSection {
	return RcloneSection{Name: name, Options: []RcloneOption{
		{"type", "drive"},
		{"scope", "drive.file"},
		{"token", `{"access_token":"a","refresh_token":"r"}`},
	}}
}

func TestAddRemoteCreatesPrivateStorage(t *testing.T) {
	config := filepath.Join(t.TempDir(), "rclone", "rclone.conf")
	b := &RcloneBackend{ConfigFile: config}
	if err := b.AddRemote(driveSection("drive-backups")); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{filepath.Dir(config): 0o700, config: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("mode of %s = %o, want %o", path, got, want)
		}
	}
	got, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	want := "[drive-backups]\ntype = drive\nscope = drive.file\n" +
		`token = {"access_token":"a","refresh_token":"r"}` + "\n"
	if string(got) != want {
		t.Fatalf("config = %q, want %q", got, want)
	}
}

func TestAddRemoteKeepsExistingRemotes(t *testing.T) {
	config := privateConfig(t, "# operator notes\n[backup]\ntype = webdav\ntoken = refreshed\n")
	b := &RcloneBackend{ConfigFile: config}
	if err := b.AddRemote(driveSection("drive-backups")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(
		string(got),
		"# operator notes\n[backup]\ntype = webdav\ntoken = refreshed\n\n[drive-backups]\n",
	) {
		t.Fatalf("config = %q; want the existing remote kept ahead of the new one", got)
	}
	if err := b.AddRemote(driveSection("backup")); !errors.Is(err, ErrRemoteExists) {
		t.Fatalf("AddRemote(existing name) error = %v, want ErrRemoteExists", err)
	}
}

func TestAddRemoteRefusesUnsafeConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T) string
	}{
		{"encrypted", func(t *testing.T) string {
			return privateConfig(t, "# Encrypted rclone configuration File\n\nRCLONE_ENCRYPT_V0:\nabc\n")
		}},
		{"group-readable file", func(t *testing.T) string {
			config := privateConfig(t, "")
			if err := os.Chmod(config, 0o640); err != nil {
				t.Fatal(err)
			}
			return config
		}},
		{"group-accessible directory", func(t *testing.T) string {
			config := privateConfig(t, "")
			if err := os.Chmod(filepath.Dir(config), 0o750); err != nil {
				t.Fatal(err)
			}
			return config
		}},
		{"symlinked file", func(t *testing.T) string {
			target := privateConfig(t, "")
			link := filepath.Join(filepath.Dir(target), "linked.conf")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
		{"relative path", func(*testing.T) string { return "rclone.conf" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := tt.setup(t)
			before, _ := os.ReadFile(config)
			b := &RcloneBackend{ConfigFile: config}
			if err := b.AddRemote(driveSection("drive-backups")); err == nil {
				t.Fatal("AddRemote succeeded")
			}
			if after, _ := os.ReadFile(config); !bytes.Equal(after, before) {
				t.Fatalf("config changed to %q", after)
			}
		})
	}
}

func TestAddRemoteRejectsValuesRcloneWouldReadDifferently(t *testing.T) {
	for _, value := range []string{"", " padded", "padded ", `"quoted"`, "'quoted'", "`quoted`", "two\nlines", "tab\there"} {
		b := &RcloneBackend{ConfigFile: privateConfig(t, "")}
		section := RcloneSection{Name: "s3-backups", Options: []RcloneOption{{"secret_access_key", value}}}
		if err := b.AddRemote(section); !errors.Is(err, ErrConfiguration) {
			t.Errorf("AddRemote(value %q) error = %v, want ErrConfiguration", value, err)
		}
	}
	for _, name := range []string{"DEFAULT", "bad name", ""} {
		b := &RcloneBackend{ConfigFile: privateConfig(t, "")}
		if err := b.AddRemote(driveSection(name)); !errors.Is(err, ErrConfiguration) {
			t.Errorf("AddRemote(name %q) error = %v, want ErrConfiguration", name, err)
		}
	}
}

func TestObscurePassesTheSecretOnStdin(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("VALMIN_RCLONE_ARGS", argsFile)
	b := &RcloneBackend{Binary: fakeRclone(t), ConfigFile: filepath.Join(t.TempDir(), "rclone.conf")}
	got, err := b.Obscure(context.Background(), "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "obscured-hunter2" {
		t.Fatalf("Obscure() = %q, want %q", got, "obscured-hunter2")
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "hunter2") {
		t.Fatalf("secret appeared in arguments %q", args)
	}
}

func privateConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "rclone")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "rclone.conf")
	if err := os.WriteFile(config, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return config
}
