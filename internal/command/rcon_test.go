package command

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecuteRCONAuthenticatesAndReturnsThePluginResponse(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	done := make(chan error, 1)
	go func() {
		login, err := readPacket(server)
		if err != nil {
			done <- err
			return
		}
		if login.id != 1 || login.typ != packetLogin || login.body != "secret" {
			done <- errors.New("unexpected login packet")
			return
		}
		if err := writePacket(server, packet{id: 1, typ: packetCommand, body: "Login success"}); err != nil {
			done <- err
			return
		}
		cmd, err := readPacket(server)
		if err != nil {
			done <- err
			return
		}
		if cmd.id != 2 || cmd.typ != packetCommand || cmd.body != "save" {
			done <- errors.New("unexpected command packet")
			return
		}
		done <- writePacket(server, packet{id: 2, typ: packetCommand, body: "World saved"})
	}()

	output, err := executeRCON(t.Context(), client, "secret", "save")
	if err != nil {
		t.Fatal(err)
	}
	if output != "World saved" {
		t.Errorf("output = %q, want World saved", output)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestExecuteRCONRejectsAuthenticationFailure(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	go func() {
		_, _ = readPacket(server)
		_ = writePacket(server, packet{id: -1, typ: packetCommand, body: "Login failed"})
	}()
	_, err := executeRCON(context.Background(), client, "wrong", "save")
	if !errors.Is(err, ErrAuthentication) {
		t.Errorf("executeRCON error = %v, want ErrAuthentication", err)
	}
}

// serverRoot opens a fresh server directory with BepInEx/config in it, and returns the root
// and the config file's path on disk.
func serverRoot(t *testing.T) (root *os.Root, configPath string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "BepInEx", "config"), 0o750); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	configPath = filepath.Join(dir, filepath.FromSlash(ConfigPath))
	return root, configPath
}

func TestReadOrCreateConfig(t *testing.T) {
	root, path := serverRoot(t)
	port, password, raw, changed, err := readOrCreateConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if port != DefaultRCONPort || password == "" || !changed {
		t.Fatalf("new config = port %d password %q changed %v", port, password, changed)
	}
	if !strings.Contains(string(raw), "Password = "+password) {
		t.Error("generated config does not contain its password")
	}

	existing := "[1. Rcon]\nPort = 2478\nPassword = existing-secret\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	port, password, _, changed, err = readOrCreateConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if port != 2478 || password != "existing-secret" || changed {
		t.Errorf("existing config = port %d password %q changed %v", port, password, changed)
	}
}

func TestValidateCommandRestrictsOperators(t *testing.T) {
	tests := []struct {
		name    string
		command string
		scope   Scope
		wantErr error
	}{
		{name: "safe operator command", command: "kick player", scope: ScopeOperator, wantErr: nil},
		{
			name:    "operator passthrough",
			command: "consoleCommand env",
			scope:   ScopeOperator,
			wantErr: ErrCommandForbidden,
		},
		{name: "admin passthrough", command: "consoleCommand env", scope: ScopeAny, wantErr: nil},
		{name: "multiline", command: "save\nshutdown", scope: ScopeAny, wantErr: ErrInvalidCommand},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateCommand(tt.command, tt.scope)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("validateCommand(%q) error = %v, want %v", tt.command, err, tt.wantErr)
			}
		})
	}
}

// A config with no Port key is an error, not a guess: the document cannot add the key and
// the plugin's own default is a different number.
func TestReadOrCreateConfigRejectsAConfigWithoutAPort(t *testing.T) {
	root, path := serverRoot(t)
	if err := os.WriteFile(path, []byte("[1. Rcon]\nPassword = existing-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := readOrCreateConfig(root); err == nil {
		t.Fatal("a config without a Port key was accepted")
	}
}

// Asserts a failure after the command was written is marked as one the server may have acted
// on, while a failure before it is not.
func TestExecuteRCONMarksAFailureAfterTheCommandWasSent(t *testing.T) {
	tests := []struct {
		name        string
		server      func(net.Conn)
		wantNoReply bool
	}{
		{"closed before the login reply", func(c net.Conn) {
			_, _ = readPacket(c)
		}, false},
		{"closed after reading the command", func(c net.Conn) {
			_, _ = readPacket(c)
			_ = writePacket(c, packet{id: 1, typ: packetCommand, body: "Login success"})
			_, _ = readPacket(c)
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			t.Cleanup(func() { _ = client.Close() })
			go func() {
				tc.server(server)
				_ = server.Close()
			}()
			_, err := executeRCON(t.Context(), client, "secret", "save")
			if err == nil {
				t.Fatal("executeRCON succeeded against a server that never replied")
			}
			if got := errors.Is(err, ErrNoReply); got != tc.wantNoReply {
				t.Errorf("errors.Is(%v, ErrNoReply) = %v, want %v", err, got, tc.wantNoReply)
			}
		})
	}
}

// TestReadOrCreateConfigRefusesASymlink asserts that a config file replaced by a symlink, such
// as one pointing at another server's config, is refused rather than followed.
func TestReadOrCreateConfigRefusesASymlink(t *testing.T) {
	root, path := serverRoot(t)
	other := filepath.Join(t.TempDir(), "other.cfg")
	if err := os.WriteFile(other, []byte("[1. Rcon]\nPort = 2455\nPassword = other-server\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, path); err != nil {
		t.Fatal(err)
	}
	if _, password, _, _, err := readOrCreateConfig(root); err == nil {
		t.Fatalf("a symlinked config was read, password %q", password)
	}
}

// TestReadOrCreateConfigRefusesASymlinkedDirectory asserts that a config directory replaced by
// a symlink out of the server tree cannot be read through.
func TestReadOrCreateConfigRefusesASymlinkedDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "BepInEx"), 0o750); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, ConfigFile),
		[]byte("[1. Rcon]\nPort = 2455\nPassword = other-server\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "BepInEx", "config")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, password, _, _, err := readOrCreateConfig(root); err == nil {
		t.Fatalf("a config behind a symlinked directory was read, password %q", password)
	}
}

// TestClearPasswordLeavesTheNextConfigureToMintOne asserts that a cleared config keeps its port
// and that reading it afterwards generates a password different from the copied one.
func TestClearPasswordLeavesTheNextConfigureToMintOne(t *testing.T) {
	root, path := serverRoot(t)
	if err := os.WriteFile(path, []byte("[1. Rcon]\nPort = 2478\nPassword = source-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ClearPassword(filepath.Dir(filepath.Dir(filepath.Dir(path)))); err != nil {
		t.Fatal(err)
	}
	port, password, _, changed, err := readOrCreateConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if port != 2478 || password == "" || password == "source-secret" || !changed {
		t.Errorf(
			"after clearing: port %d password %q changed %v; want 2478, a new password, changed",
			port,
			password,
			changed,
		)
	}
}
