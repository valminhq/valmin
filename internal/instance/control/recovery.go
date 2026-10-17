package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// Recovery resolves filesystem work left by interrupted instance jobs.
type Recovery struct {
	DB       *store.DB
	Runtime  runtime.Runtime
	DataRoot string
}

// withinRoot reports whether path is strictly inside root.
func withinRoot(root, path string) bool {
	within, err := filepath.Rel(root, path)
	return err == nil && within != "." && within != ".." &&
		!strings.HasPrefix(within, ".."+string(filepath.Separator))
}

// sweepImportStaging removes an interrupted world upload's staging tree.
func (s *Recovery) sweepImportStaging(ctx context.Context, j *store.Job) {
	var payload WorldImportPayload
	if err := json.Unmarshal([]byte(j.Payload), &payload); err != nil {
		slog.WarnContext(ctx, "interrupted import: payload unreadable, staging left in place",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	if payload.StagingDir == "" {
		return
	}
	root := instance.ImportStagingRoot(s.DataRoot)
	if !withinRoot(root, payload.StagingDir) {
		slog.ErrorContext(ctx, "interrupted import names a staging directory outside the staging root; not removing",
			slog.String("job_id", j.ID), slog.String("staging_dir", payload.StagingDir),
			slog.String("staging_root", root))
		return
	}
	if err := os.RemoveAll(payload.StagingDir); err != nil {
		slog.WarnContext(ctx, "interrupted import: staging directory not removed",
			slog.String("job_id", j.ID), slog.String("staging_dir", payload.StagingDir),
			slog.Any("error", err))
		return
	}
	slog.InfoContext(ctx, "removed the staging directory of an interrupted import",
		slog.String("job_id", j.ID), slog.String("staging_dir", payload.StagingDir))
}

// sweepBackupPart removes the unpublished part file of an interrupted backup.
func (s *Recovery) sweepBackupPart(ctx context.Context, j *store.Job) {
	var payload BackupPayload
	if err := json.Unmarshal([]byte(j.Payload), &payload); err != nil || payload.Dest == "" {
		return
	}
	root := instance.BackupsDir(s.DataRoot)
	if !withinRoot(root, payload.Dest) {
		slog.ErrorContext(ctx, "interrupted backup names an archive outside the backups root; not removing",
			slog.String("job_id", j.ID), slog.String("dest", payload.Dest),
			slog.String("backups_root", root))
		return
	}
	if err := os.Remove(payload.Dest + backup.PartSuffix); err != nil && !os.IsNotExist(err) {
		slog.WarnContext(ctx, "interrupted backup: partial archive not removed",
			slog.String("job_id", j.ID), slog.Any("error", err))
	}
}

// sweepCloneStaging removes an unpublished clone archive and resolves its world swap.
func (s *Recovery) sweepCloneStaging(ctx context.Context, j *store.Job) {
	if j.InstanceID == nil {
		return
	}
	var payload ClonePayload
	if err := json.Unmarshal([]byte(j.Payload), &payload); err == nil && payload.ArchivePath != "" {
		root := filepath.Join(instance.BackupsDir(s.DataRoot), *j.InstanceID)
		if withinRoot(root, payload.ArchivePath) {
			if err := os.Remove(payload.ArchivePath + backup.PartSuffix); err != nil && !os.IsNotExist(err) {
				slog.WarnContext(ctx, "interrupted clone: partial archive not removed",
					slog.String("job_id", j.ID), slog.Any("error", err))
			}
		}
	}
	destination, err := s.DB.InstanceByID(ctx, *j.InstanceID)
	if err != nil || destination == nil {
		return
	}
	if action, err := backup.RecoverSwap(instance.WorldsDir(destination.DataDir)); err != nil {
		slog.ErrorContext(ctx, "interrupted clone: destination world swap unresolved",
			slog.String("job_id", j.ID), slog.Any("error", err))
	} else if action != "no restore swap was in progress" {
		slog.InfoContext(ctx, "recovered interrupted clone world swap",
			slog.String("job_id", j.ID), slog.String("action", action))
	}
}

// sweepUpdateSwap resolves the server tree after an interrupted game update.
func (s *Recovery) sweepUpdateSwap(ctx context.Context, j *store.Job) {
	if j.InstanceID == nil {
		return
	}
	inst, err := s.DB.InstanceByID(ctx, *j.InstanceID)
	if err != nil || inst == nil {
		slog.ErrorContext(ctx, "interrupted game update: instance unreadable, the swap is unresolved",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	staged := instance.StagedBuildID(inst.DataDir)
	action, err := instance.RecoverUpdate(inst.DataDir)
	if err != nil {
		slog.ErrorContext(ctx, "interrupted game update: the swap could not be resolved",
			slog.String("job_id", j.ID), slog.String("instance_id", inst.ID),
			slog.String("staged_build_id", staged), slog.Any("error", err))
		return
	}
	slog.InfoContext(ctx, "resolved an interrupted game update: "+action,
		slog.String("job_id", j.ID), slog.String("instance_id", inst.ID),
		slog.String("staged_build_id", staged))
}

// sweepRestoreSwap resolves an interrupted restore only after proving the server is stopped.
func (s *Recovery) sweepRestoreSwap(ctx context.Context, j *store.Job) {
	if j.InstanceID == nil {
		return
	}
	inst, err := s.DB.InstanceByID(ctx, *j.InstanceID)
	if err != nil || inst == nil {
		slog.ErrorContext(ctx, "interrupted restore: instance unreadable, the swap is unresolved",
			slog.String("job_id", j.ID), slog.Any("error", err))
		return
	}
	running, err := runningInDocker(ctx, s.Runtime, inst)
	if err != nil {
		slog.ErrorContext(ctx,
			"interrupted restore: could not establish whether the server is stopped, the swap is unresolved",
			slog.String("job_id", j.ID), slog.String("instance_id", inst.ID), slog.Any("error", err))
		return
	}
	worldsDir := filepath.Join(instance.WorldsDir(inst.DataDir), instance.WorldsLocalDir)
	if running {
		slog.ErrorContext(ctx,
			"interrupted restore: the server is running, so the swap is left untouched. "+
				"Stop the container, then resolve worlds_local, worlds_local.new and worlds_local.old by hand",
			slog.String("job_id", j.ID), slog.String("instance_id", inst.ID),
			slog.String("worlds_dir", worldsDir))
		return
	}
	action, err := backup.RecoverSwapKeeping(worldsDir, backup.IsAutoSave)
	if err != nil {
		slog.ErrorContext(ctx, "interrupted restore: the swap could not be resolved",
			slog.String("job_id", j.ID), slog.String("instance_id", inst.ID), slog.Any("error", err))
		return
	}
	slog.InfoContext(ctx, "resolved an interrupted restore: "+action,
		slog.String("job_id", j.ID), slog.String("instance_id", inst.ID))
}
