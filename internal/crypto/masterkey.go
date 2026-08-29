package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
)

const (
	// EnvMasterKey carries the master key as base64 for an operator who would rather
	// manage it than let the panel own a file (10 §3.1).
	EnvMasterKey = "VALMIN_MASTER_KEY"
	// EnvMasterKeyFile names a file holding the same base64 text, so Docker and Podman
	// secrets work without an entrypoint shim.
	EnvMasterKeyFile = "VALMIN_MASTER_KEY_FILE"

	// keyFileMode is required of the panel-managed key file. The operator-managed file
	// named by EnvMasterKeyFile is not mode-checked: a mounted secret is not ours to
	// have permissions opinions about.
	keyFileMode fs.FileMode = 0o600
)

// LoadMasterKey resolves the master key from the environment or from path, generating it
// at path on first start.
//
// The file is created as the user the panel already runs as and is never chowned
// afterwards, for the same reason the provisioning clone is not (08 §3, Q14): a
// defensive chown masks a process running as the wrong user.
func LoadMasterKey(path string, getenv func(string) string) ([]byte, error) {
	inline := getenv(EnvMasterKey)
	file := getenv(EnvMasterKeyFile)

	switch {
	case inline != "" && file != "":
		return nil, fmt.Errorf("%s and %s are both set: pick one, this is not a precedence question",
			EnvMasterKey, EnvMasterKeyFile)

	case inline != "":
		slog.Info("master key supplied by the environment", slog.String("source", EnvMasterKey))
		return decodeMasterKey(inline, EnvMasterKey)

	case file != "":
		//nolint:gosec // the path is an operator-set variable, which is the point of _FILE
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", EnvMasterKeyFile, err)
		}
		slog.Info("master key supplied by the environment",
			slog.String("source", EnvMasterKeyFile), slog.String("path", file))
		return decodeMasterKey(string(raw), file)

	default:
		return keyFile(path)
	}
}

// keyFile reads the panel-managed key, or creates it if this is the first start.
func keyFile(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("secrets.master_key_file is empty")
	}

	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return generateKeyFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("stat master key %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("master key %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm != keyFileMode {
		return nil, fmt.Errorf("master key %s has mode %#o, want %#o", path, perm, keyFileMode)
	}

	//nolint:gosec // the path is secrets.master_key_file, an operator setting
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read master key %s: %w", path, err)
	}
	if len(key) != MasterKeyLen {
		return nil, fmt.Errorf("master key %s is %d bytes, want %d raw bytes", path, len(key), MasterKeyLen)
	}
	return key, nil
}

// generateKeyFile writes a new key with O_EXCL, so a second process racing to create one
// fails rather than overwriting a key that already protects rows.
func generateKeyFile(path string) ([]byte, error) {
	key := make([]byte, MasterKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}

	//nolint:gosec // the path is secrets.master_key_file, an operator setting
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFileMode)
	if err != nil {
		return nil, fmt.Errorf("create master key %s: %w", path, err)
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write master key %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close master key %s: %w", path, err)
	}
	slog.Warn("generated a new master key",
		slog.String("path", path),
		slog.String("note", "back it up separately from the database; without it, stored "+
			"instance passwords, RCON passwords and TOTP secrets are unrecoverable"))
	return key, nil
}

// decodeMasterKey decodes the base64 form. source names where the value came from and is
// never the value itself (11 §9).
func decodeMasterKey(value, source string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("decode master key from %s: not base64", source)
	}
	if len(key) != MasterKeyLen {
		return nil, fmt.Errorf("master key from %s is %d bytes, want %d", source, len(key), MasterKeyLen)
	}
	return key, nil
}
