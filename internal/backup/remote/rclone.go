package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var remoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type RcloneBackend struct {
	Binary     string
	ConfigFile string
	Remote     string
	Folder     string
}

func (b *RcloneBackend) Validate() error {
	if b.Binary == "" || b.ConfigFile == "" || !remoteName.MatchString(b.Remote) ||
		(b.Folder != "" && !ValidKey(b.Folder)) {
		return ErrConfiguration
	}
	return nil
}

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if left := (1 << 20) - b.Len(); left > 0 {
		_, _ = b.Buffer.Write(p[:min(left, n)])
	}
	return n, nil
}

func (b *RcloneBackend) run(ctx context.Context, args ...string) ([]byte, error) {
	args = append([]string{
		"--config", b.ConfigFile, "--retries", "1", "--low-level-retries", "1",
		"--contimeout", "10s", "--timeout", "2m", "--max-duration", "1h",
	}, args...)
	//nolint:gosec // Executable is deployment-owned; validated arguments never pass through a shell.
	cmd := exec.CommandContext(
		ctx,
		b.Binary,
		args...)
	// Exclude inherited rclone overrides and proxy settings; the dedicated config is authoritative.
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "RCLONE_") || strings.HasSuffix(strings.ToUpper(name), "_PROXY") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	var out boundedOutput
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, &Failure{Message: "rclone transfer was interrupted", Temporary: true}
		}
		return nil, rcloneError(err)
	}
	return out.Bytes(), nil
}

func rcloneError(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 3, 4:
			return ErrNotFound
		case 5:
			return &Failure{Message: "rclone reported a temporary transfer failure", Temporary: true}
		case 7:
			return &Failure{Message: "rclone rejected the configuration or credentials"}
		case 10:
			return &Failure{Message: "rclone transfer exceeded its deadline", Temporary: true}
		default:
			return &Failure{Message: "rclone transfer failed; check remote configuration"}
		}
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return &Failure{Message: "rclone is not installed"}
	}
	if errors.Is(err, os.ErrPermission) {
		return &Failure{Message: "rclone is not executable by the panel user"}
	}
	return &Failure{Message: "rclone could not complete the command", Temporary: true}
}

func (b *RcloneBackend) target(key string) (string, error) {
	if err := b.Validate(); err != nil {
		return "", err
	}
	if !ValidKey(key) {
		return "", ErrConfiguration
	}
	return b.Remote + ":" + path.Join(b.Folder, key), nil
}

func (b *RcloneBackend) Put(ctx context.Context, key, localPath string) (Object, error) {
	target, err := b.target(key)
	if err != nil {
		return Object{}, err
	}
	info, err := os.Stat(localPath)
	if err != nil || !info.Mode().IsRegular() {
		return Object{}, &Failure{Message: "local archive is unavailable"}
	}
	if _, err := b.run(ctx, "copyto", "--ignore-times", "--", localPath, target); err != nil {
		return Object{}, err
	}
	return Object{Ref: ObjectRef{Key: key}, SizeBytes: info.Size()}, nil
}

func (b *RcloneBackend) Stat(ctx context.Context, ref ObjectRef) (Object, error) {
	target, err := b.target(ref.Key)
	if err != nil {
		return Object{}, err
	}
	raw, err := b.run(ctx, "lsjson", "--stat", "--", target)
	if err != nil {
		return Object{}, err
	}
	var entry struct {
		Size  int64
		IsDir bool
		ID    string
	}
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Size < 0 || entry.IsDir {
		return Object{}, &Failure{Message: "rclone returned invalid object metadata"}
	}
	ref.ProviderID = entry.ID
	return Object{Ref: ref, SizeBytes: entry.Size}, nil
}

func (b *RcloneBackend) Delete(ctx context.Context, ref ObjectRef) error {
	target, err := b.target(ref.Key)
	if err != nil {
		return err
	}
	_, err = b.run(ctx, "deletefile", "--", target)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (b *RcloneBackend) Remotes(ctx context.Context) ([]string, error) {
	raw, err := b.run(ctx, "listremotes")
	if err != nil {
		return nil, err
	}
	names := []string{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		name := strings.TrimSuffix(strings.TrimSpace(line), ":")
		if remoteName.MatchString(name) {
			names = append(names, name)
		}
	}
	return names, nil
}

func (b *RcloneBackend) CheckConfig(ctx context.Context) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if err := b.checkConfigStorage(); err != nil {
		return err
	}

	names, err := b.Remotes(ctx)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == b.Remote {
			return nil
		}
	}
	return &Failure{Message: "configured rclone remote was not found"}
}

func (b *RcloneBackend) checkConfigStorage() error {
	f, err := os.OpenFile(b.ConfigFile, os.O_RDWR, 0)
	if err != nil {
		return &Failure{Message: "rclone configuration must exist and be writable by the panel user"}
	}
	info, statErr := f.Stat()
	closeErr := f.Close()
	if statErr != nil || closeErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return &Failure{Message: "rclone configuration must be a private regular file (mode 0600)"}
	}
	dir := filepath.Dir(b.ConfigFile)
	info, err = os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return &Failure{Message: "rclone configuration directory must be private (mode 0700)"}
	}
	probe, err := os.CreateTemp(dir, ".valmin-write-check-")
	if err != nil {
		return &Failure{Message: "rclone configuration directory must be writable for token refresh"}
	}
	name := probe.Name()
	closeErr = probe.Close()
	removeErr := os.Remove(name)
	if closeErr != nil || removeErr != nil {
		return &Failure{Message: "rclone configuration directory must be writable for token refresh"}
	}
	return nil
}
