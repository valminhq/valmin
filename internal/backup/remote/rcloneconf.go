package remote

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// ErrRemoteExists reports that the rclone configuration already defines the requested remote.
var ErrRemoteExists = errors.New("rclone remote already exists")

var optionKey = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// RcloneOption is one key = value line of an rclone remote.
type RcloneOption struct{ Key, Value string }

// RcloneSection is one remote's entry in rclone.conf, with its options in write order.
type RcloneSection struct {
	Name    string
	Options []RcloneOption
}

// ValidOptionValue reports whether v is written to rclone.conf and read back unchanged: a
// single line without surrounding spaces or a leading quote, which rclone's reader would
// strip or treat as the start of a multi-line value.
func ValidOptionValue(v string) bool {
	if v == "" || strings.TrimSpace(v) != v || strings.ContainsAny(v[:1], "\"'`") {
		return false
	}
	return !strings.ContainsFunc(v, unicode.IsControl)
}

func (s RcloneSection) encode() ([]byte, error) {
	// DEFAULT holds fallback values for every other remote in rclone's reader.
	if !ValidRemoteName(s.Name) || s.Name == "DEFAULT" {
		return nil, ErrConfiguration
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "[%s]\n", s.Name)
	for _, o := range s.Options {
		if !optionKey.MatchString(o.Key) || !ValidOptionValue(o.Value) {
			return nil, ErrConfiguration
		}
		fmt.Fprintf(&b, "%s = %s\n", o.Key, o.Value)
	}
	return b.Bytes(), nil
}

// Obscure returns secret in rclone's reversible obscured form, which rclone requires for
// password fields. The secret travels on stdin.
func (b *RcloneBackend) Obscure(ctx context.Context, secret string) (string, error) {
	out, err := b.runInput(ctx, strings.NewReader(secret+"\n"), "obscure", "-")
	if err != nil {
		return "", err
	}
	obscured := strings.TrimSpace(string(out))
	if obscured == "" {
		return "", &Failure{Message: "rclone returned an empty obscured value"}
	}
	return obscured, nil
}

// PrepareConfig checks that the configuration can take a new remote. It creates the private
// configuration directory when it is missing.
func (b *RcloneBackend) PrepareConfig() error {
	_, err := b.readConfigForAppend()
	return err
}

// AddRemote appends s to the rclone configuration and replaces the file atomically. It
// rereads the file first, so tokens refreshed in the meantime are kept, and refuses a name
// that already exists. The caller holds the remote-backup lock, which keeps rclone from
// rewriting the file during the append.
func (b *RcloneBackend) AddRemote(s RcloneSection) error {
	section, err := s.encode()
	if err != nil {
		return err
	}
	current, err := b.readConfigForAppend()
	if err != nil {
		return err
	}
	if hasSection(current, s.Name) {
		return ErrRemoteExists
	}
	if len(current) > 0 {
		current = append(bytes.TrimRight(current, "\n"), "\n\n"...)
	}
	return writePrivateFile(b.ConfigFile, append(current, section...))
}

// readConfigForAppend returns the current configuration, or nil when the file does not exist
// yet. It refuses a symlink, a file or directory other users can read, and an encrypted
// configuration, which the panel has no password for.
func (b *RcloneBackend) readConfigForAppend() ([]byte, error) {
	if b.ConfigFile == "" || !filepath.IsAbs(b.ConfigFile) {
		return nil, ErrConfiguration
	}
	dir := filepath.Dir(b.ConfigFile)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, &Failure{Message: "rclone configuration directory could not be created by the panel user"}
	}
	if err := checkConfigDir(dir); err != nil {
		return nil, err
	}
	info, err := os.Lstat(b.ConfigFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &Failure{Message: "rclone configuration cannot be read by the panel user"}
	}
	if err := checkConfigFile(info); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(b.ConfigFile)
	if err != nil {
		return nil, &Failure{Message: "rclone configuration cannot be read by the panel user"}
	}
	if encrypted(data) {
		return nil, &Failure{
			Message: "Encrypted rclone configuration is not supported. Add the connection with rclone config instead.",
		}
	}
	return data, nil
}

func encrypted(config []byte) bool {
	scanner := bufio.NewScanner(bytes.NewReader(config))
	for scanner.Scan() {
		if strings.HasPrefix(strings.TrimSpace(scanner.Text()), "RCLONE_ENCRYPT_V") {
			return true
		}
	}
	return false
}

func hasSection(config []byte, name string) bool {
	header := "[" + name + "]"
	scanner := bufio.NewScanner(bytes.NewReader(config))
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == header {
			return true
		}
	}
	return false
}

// writePrivateFile replaces path with data through a mode-0600 temp file in the same
// directory: written, synced, renamed, and the directory synced.
func writePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".valmin-rclone-")
	if err != nil {
		return &Failure{Message: "rclone configuration directory must be writable by the panel user"}
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write rclone configuration: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync rclone configuration: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close rclone configuration: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace rclone configuration: %w", err)
	}
	d, err := os.Open(dir) //nolint:gosec // The directory comes from the operator's rclone_config setting.
	if err != nil {
		return fmt.Errorf("open rclone configuration directory: %w", err)
	}
	if err := errors.Join(d.Sync(), d.Close()); err != nil {
		return fmt.Errorf("fsync rclone configuration directory: %w", err)
	}
	return nil
}
