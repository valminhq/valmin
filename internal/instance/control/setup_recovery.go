package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

// SetupStagingRoot is where setup save and restore jobs stage their files.
func SetupStagingRoot(dataRoot string) string { return filepath.Join(dataRoot, "staging", "setups") }

type SetupRootPaths struct {
	Root  string   `json:"root"`
	Paths []string `json:"paths"`
}

type SetupJournal struct {
	Roots []SetupRootPaths `json:"roots"`
}

func setupRoot(inst *store.Instance, name string) (string, error) {
	if name == "server" {
		return filepath.Join(inst.DataDir, "server"), nil
	}
	mod, ok := strings.CutPrefix(name, "park:")
	if !ok {
		return "", fmt.Errorf("invalid setup root %q", name)
	}
	if err := installer.CheckFullName(mod); err != nil {
		return "", fmt.Errorf("validate parked package %s: %w", mod, err)
	}
	return filepath.Join(instance.ParkedModsDir(inst.DataDir), mod), nil
}

func WriteSetupJournal(staging string, journal SetupJournal) error {
	raw, err := json.Marshal(journal)
	if err != nil {
		return fmt.Errorf("encode setup rollback journal: %w", err)
	}
	journalPath := filepath.Join(staging, "journal.json")
	f, err := os.OpenFile(journalPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // panel-generated path
	if err != nil {
		return fmt.Errorf("create setup rollback journal: %w", err)
	}
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("write setup rollback journal: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close setup rollback journal: %w", closeErr)
	}
	stageRoot, err := os.OpenRoot(staging)
	if err != nil {
		return fmt.Errorf("open setup staging directory: %w", err)
	}
	defer func() { _ = stageRoot.Close() }()
	dir, err := stageRoot.Open(".")
	if err != nil {
		return fmt.Errorf("open setup staging directory for sync: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync setup staging directory: %w", err)
	}
	return nil
}

func readSetupJournal(staging string) (SetupJournal, error) {
	root, err := os.OpenRoot(staging)
	if err != nil {
		return SetupJournal{}, fmt.Errorf("open setup staging directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open("journal.json")
	if err != nil {
		return SetupJournal{}, fmt.Errorf("open setup rollback journal: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(f)
	if err != nil {
		return SetupJournal{}, fmt.Errorf("read setup rollback journal: %w", err)
	}
	var journal SetupJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		return SetupJournal{}, fmt.Errorf("decode setup rollback journal: %w", err)
	}
	return journal, nil
}

func BackupSetupPaths(inst *store.Instance, staging string, journal SetupJournal) error {
	for i, group := range journal.Roots {
		root, err := setupRoot(inst, group.Root)
		if err != nil {
			return err
		}
		if err := installer.BackupPaths(group.Paths, root,
			filepath.Join(staging, "backup", fmt.Sprintf("%d", i))); err != nil {
			return fmt.Errorf("back up %s files for setup restore: %w", group.Root, err)
		}
	}
	return nil
}

func rollbackSetupPaths(inst *store.Instance, staging string, journal SetupJournal) error {
	var errs []error
	for i, group := range journal.Roots {
		root, err := setupRoot(inst, group.Root)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		entries := make([]installer.ManifestEntry, 0, len(group.Paths))
		for _, p := range group.Paths {
			entries = append(entries, installer.ManifestEntry{Path: p})
		}
		if err := installer.Rollback(entries, root,
			filepath.Join(staging, "backup", fmt.Sprintf("%d", i))); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", group.Root, err))
		}
	}
	return errors.Join(errs...)
}

func (h *Recovery) sweepSetupRestore(ctx context.Context, job *store.Job) {
	var payload SetupJobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil || job.InstanceID == nil {
		slog.ErrorContext(ctx, "interrupted setup restore has invalid payload", slog.String("job_id", job.ID))
		return
	}
	if payload.StagingDir == "" || !withinRoot(SetupStagingRoot(h.DataRoot), payload.StagingDir) {
		slog.ErrorContext(ctx, "interrupted setup restore has unsafe staging path", slog.String("job_id", job.ID))
		return
	}
	journal, err := readSetupJournal(payload.StagingDir)
	if errors.Is(err, os.ErrNotExist) {
		_ = os.RemoveAll(payload.StagingDir)
		return
	}
	if err != nil {
		slog.ErrorContext(
			ctx,
			"interrupted setup restore journal unreadable",
			slog.String("job_id", job.ID),
			slog.Any("error", err),
		)
		return
	}
	inst, err := h.DB.InstanceByID(ctx, *job.InstanceID)
	if err != nil || inst == nil {
		slog.ErrorContext(
			ctx,
			"interrupted setup restore instance unavailable",
			slog.String("job_id", job.ID),
			slog.Any("error", err),
		)
		return
	}
	if err := rollbackSetupPaths(inst, payload.StagingDir, journal); err != nil {
		slog.ErrorContext(
			ctx,
			"interrupted setup restore rollback failed",
			slog.String("job_id", job.ID),
			slog.Any("error", err),
		)
		_, _ = instance.SetState(ctx, h.DB, inst.ID, instance.StateStopped, instance.StateError)
		return
	}
	_ = os.RemoveAll(payload.StagingDir)
}

func (h *Recovery) sweepSetupSave(ctx context.Context, job *store.Job) {
	var payload SetupJobPayload
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return
	}
	if payload.StagingDir != "" && withinRoot(SetupStagingRoot(h.DataRoot), payload.StagingDir) {
		if err := os.RemoveAll(payload.StagingDir); err != nil {
			slog.WarnContext(ctx, "remove interrupted setup save staging", slog.Any("error", err))
		}
	}
}
