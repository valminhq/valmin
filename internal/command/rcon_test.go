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

func TestReadOrCreateConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "org.tristan.rcon.cfg")
	port, password, raw, changed, err := readOrCreateConfig(path)
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
	port, password, _, changed, err = readOrCreateConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if port != 2478 || password != "existing-secret" || changed {
		t.Errorf("existing config = port %d password %q changed %v", port, password, changed)
	}
}

func TestValidateCommandRestrictsOperators(t *testing.T) {
	tests := []struct {
		name         string
		command      string
		unrestricted bool
		wantErr      error
	}{
		{name: "safe operator command", command: "kick player", wantErr: nil},
		{name: "operator passthrough", command: "consoleCommand env", wantErr: ErrCommandForbidden},
		{name: "admin passthrough", command: "consoleCommand env", unrestricted: true, wantErr: nil},
		{name: "multiline", command: "save\nshutdown", unrestricted: true, wantErr: ErrInvalidCommand},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateCommand(tt.command, tt.unrestricted)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("validateCommand(%q) error = %v, want %v", tt.command, err, tt.wantErr)
			}
		})
	}
}

// A config with no Port key is an error, not a guess: the document cannot add the key and
// the plugin's own default is a different number.
func TestReadOrCreateConfigRejectsAConfigWithoutAPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "org.tristan.rcon.cfg")
	if err := os.WriteFile(path, []byte("[1. Rcon]\nPassword = existing-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := readOrCreateConfig(path); err == nil {
		t.Fatal("a config without a Port key was accepted")
	}
}
