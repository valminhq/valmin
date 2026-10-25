package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/export"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/store"
)

// runRestore is `valmind admin restore`. It unpacks an installation export read from in into
// the empty data root and readies the database for this host. The panel then rebuilds each
// server's game installation on its first start. A restore that fails or is cut short leaves
// the root marked incomplete: the panel will not start on it, and running this again redoes it.
func runRestore(ctx context.Context, getenv func(string) string, in io.Reader, out io.Writer) error {
	cfg, err := config.Load(nil, getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	if running, err := store.DaemonRunning(cfg.Data.Root); err != nil || running {
		if err != nil {
			return fmt.Errorf("check for a running panel: %w", err)
		}
		return errPanelRunning
	}
	if err := requireRestorePaths(cfg); err != nil {
		return err
	}
	schema, err := store.SchemaVersion()
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	m, err := export.Restore(in, cfg.Data.Root, schema)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}

	_, db, err := openAdminDB(ctx, getenv)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	restored, err := prepareRestored(ctx, db, cfg.Data.Root, m.Builds)
	if err != nil {
		return fmt.Errorf("%w; run the restore again to start over", err)
	}
	if err := export.FinishRestore(cfg.Data.Root); err != nil {
		return err //nolint:wrapcheck // FinishRestore names the step
	}
	return printRestored(out, m, restored, keyNote(cfg, getenv))
}

// requireRestorePaths refuses a configuration that keeps the database, the master key file or
// the rclone configuration away from the place in the data root a restore unpacks it to.
func requireRestorePaths(cfg *config.Config) error {
	root := cfg.Data.Root
	dbPath, _, _ := strings.Cut(strings.TrimPrefix(cfg.DB.DSN, "file:"), "?")
	for _, p := range []struct{ setting, configured, restored string }{
		{"db.dsn", filepath.Clean(dbPath), export.DatabaseName},
		{"secrets.master_key_file", cfg.Secrets.MasterKeyFile, export.KeyName},
		{"remote_backups.rclone_config", cfg.RemoteBackups.RcloneConfig, export.RcloneConfigName},
	} {
		want := filepath.Join(root, filepath.FromSlash(p.restored))
		if p.configured != want {
			return fmt.Errorf("%s names %s, but a restore unpacks it to %s; unset %s for the restore "+
				"and move the file afterwards", p.setting, p.configured, want, p.setting)
		}
	}
	return nil
}

// prepareRestored lays out every server's directories, marks each provisioned one for a
// rebuild, and readies the database: servers stopped, the builds the export recorded, and
// the archives the export left out uncatalogued. It returns how many servers it marked.
func prepareRestored(ctx context.Context, db *store.DB, root string, builds map[string]string) (int, error) {
	instances, err := db.ListInstances(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("list servers: %w", err)
	}
	marked := 0
	for i := range instances {
		inst := &instances[i]
		if inst.DataDir != instance.DataDir(root, inst.ID) {
			return 0, fmt.Errorf("server %s keeps its data at %s, outside %s", inst.Name, inst.DataDir, root)
		}
		if err := instance.EnsureInstanceDirs(inst.DataDir); err != nil {
			return 0, fmt.Errorf("lay out %s: %w", inst.Name, err)
		}
		if err := fsutil.MkdirAllExact(instance.ServerDir(inst.DataDir)); err != nil {
			return 0, fmt.Errorf("lay out %s: %w", inst.Name, err)
		}
		// A server never provisioned has no installation to rebuild, and one parked in error
		// stays parked until someone looks at it.
		if st := instance.State(inst.State); st == instance.StateCreated || st == instance.StateError {
			continue
		}
		if err := instance.MarkRestorePending(inst.DataDir); err != nil {
			return 0, fmt.Errorf("mark %s: %w", inst.Name, err)
		}
		marked++
	}

	backups, err := db.BackupFiles(ctx)
	if err != nil {
		return 0, err //nolint:wrapcheck // BackupFiles names the query
	}
	var missing []store.BackupFile
	for _, b := range backups {
		if _, err := os.Lstat(b.Path); errors.Is(err, os.ErrNotExist) {
			missing = append(missing, b)
		}
	}
	if err := db.PrepareRestored(ctx, builds, missing); err != nil {
		return 0, err //nolint:wrapcheck // PrepareRestored names the step
	}
	return marked, nil
}

// keyNote says what the operator must do about the master key, "" when the restored key file
// is the one the panel will use.
func keyNote(cfg *config.Config, getenv func(string) string) string {
	_, err := os.Lstat(cfg.Secrets.MasterKeyFile)
	restored := err == nil
	fromEnv := getenv(crypto.EnvMasterKey) != "" || getenv(crypto.EnvMasterKeyFile) != ""
	switch {
	case restored && fromEnv:
		return fmt.Sprintf("The export's master key is at %s, but %s or %s is set and takes its place. "+
			"Unset it, or make sure it holds the old panel's key.", cfg.Secrets.MasterKeyFile,
			crypto.EnvMasterKey, crypto.EnvMasterKeyFile)
	case fromEnv:
		return "The export holds no master key, so the panel uses the one the environment supplies, " +
			"which must be the old panel's key."
	case !restored:
		return "The export holds no master key. Put the old secret.key in place, or run " +
			"valmind admin accept-new-key --confirm-key-loss before starting the panel."
	}
	return ""
}

// printRestored reports what the restore did and what happens next.
func printRestored(out io.Writer, m *export.Manifest, servers int, keyNote string) error {
	report := fmt.Sprintf("Restored a %s export made by panel %s at %s.\n",
		m.Mode, m.Panel, m.CreatedAt.Format("2006-01-02 15:04 MST"))
	report += fmt.Sprintf("%d server(s) will rebuild their game installation when the panel starts; "+
		"each one stays stopped until you start it.\n", servers)
	if keyNote != "" {
		report += "\n" + keyNote + "\n"
	}
	return writeOut(out, report)
}
