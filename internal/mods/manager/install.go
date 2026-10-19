// Package manager runs mod jobs over the registry index: install, update, toggle and uninstall,
// with their rollback and crash recovery, and the registry sync.
package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/cache"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/mods/thunderstore"
	"github.com/valminhq/valmin/internal/store"
)

const bepinexConfig = "BepInEx/config/BepInEx.cfg"

const (
	CheckpointResolved        = "resolved"
	CheckpointDownloaded      = "downloaded"
	CheckpointStaged          = "staged"
	CheckpointBackedUp        = "backed_up"
	CheckpointManifestWritten = "manifest_written"
	CheckpointApplied         = "applied"
)

// Installer owns mod install submissions, staging, placement, and rollback.
type Installer struct {
	DB            *store.DB
	Engine        *jobs.Engine
	Commands      *command.Manager
	Clients       map[source.Source]*thunderstore.Client
	Caches        map[source.Source]*cache.Cache
	DataRoot      string
	ArchiveWorlds ArchiveWorlds
}

func (i *Installer) enabledSources() []source.Source {
	out := make([]source.Source, 0, len(i.Clients))
	for _, src := range source.All() {
		if _, ok := i.Clients[src]; ok {
			out = append(out, src)
		}
	}
	return out
}

// Planner is a Planner over this installer's database and enabled registries.
func (i *Installer) Planner() *Planner { return &Planner{DB: i.DB, Enabled: i.enabledSources()} }

func (i *Installer) newIndex(ctx context.Context, id string, prefer source.Source) *Index {
	return NewIndex(ctx, i.DB, id, prefer, i.enabledSources())
}

func (i *Installer) failureCode(err error) errcode.Code {
	if errors.Is(err, instance.ErrServerRunning) {
		return errcode.InstanceMustBeStopped
	}
	return errcode.Internal
}

func stagedPackageDir(stagingDir, fullName string) string {
	return filepath.Join(stagingDir, "pkg", fullName)
}

// Submit stages a directory and submits one mod_install job for payload, filling in its
// StagingDir. Every install goes through here, the single-package one and "Update all" alike.
// audit is the operator's request entry, nil for a job a definition chain submits on its own.
func (i *Installer) Submit(
	ctx context.Context,
	inst *store.Instance,
	payload *InstallPayload,
	what string,
	requestedBy string,
	audit *store.AuditEntry,
	afterFinish func(context.Context),
) (*store.Job, error) {
	root := stagingRoot(i.DataRoot)
	if err := fsutil.MkdirAllExact(root); err != nil {
		return nil, fmt.Errorf("create the mod staging root: %w", err)
	}
	staging, err := os.MkdirTemp(root, what+"-*")
	if err != nil {
		return nil, fmt.Errorf("create a staging directory for a mod %s: %w", what, err)
	}
	submitted := false
	defer func() {
		if !submitted {
			_ = os.RemoveAll(staging)
		}
	}()

	id := inst.ID
	payload.StagingDir = staging
	job, err := i.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindModInstall, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: requestedBy, Payload: *payload,
		Audit: audit,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			// A stopped→stopped compare-and-swap: the kind holds the lock without moving
			// the state, and the CAS makes "still stopped" atomic with taking the lock.
			ok, err := instance.HoldStateTx(ctx, tx, id, instance.StateStopped)
			if err != nil {
				return fmt.Errorf("claim mod_install for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, i.runModInstallThen(inst, payload, afterFinish))
	if err != nil {
		// Not wrapped: writeJobSubmitError reads the typed conflicts out with errors.As.
		return nil, err //nolint:wrapcheck // the caller matches on the engine's typed errors
	}
	submitted = true
	return job, nil
}

// runModInstallThen is runModInstall with a continuation that runs only on success.
// Starting the server after a failed install would create the world unmodded, which is the
// outcome installing before first boot exists to prevent.
func (i *Installer) runModInstallThen(
	inst *store.Instance, payload *InstallPayload, afterFinish func(context.Context),
) jobs.Runner {
	run := i.runModInstall(inst, payload)
	if afterFinish == nil {
		return run
	}
	return func(ctx context.Context, h *jobs.Handle) jobs.Outcome {
		out := run(ctx, h)
		if out.Status == jobs.StatusSucceeded {
			out.AfterFinish = afterFinish
		}
		return out
	}
}

// stagedPackage is one package of the closure, carried between the runner's phases.
type stagedPackage struct {
	fullName string
	// src is the registry this package's bytes come from. It is chosen once, during resolve,
	// and then drives the download, the cache root and the recorded install (B14).
	src        source.Source
	version    string
	transitive bool
	// remove is a package the change uninstalls: nothing is downloaded or placed, its row goes
	// in the job's Finish transaction, and its files come off as an update's stale files do.
	remove      bool
	zipPath     string
	stagingDir  string
	changes     []installer.Change
	manifest    []installer.ManifestEntry
	manifestRaw string
	// prev is the row this package replaces on an update, nil on a first install. Its file
	// manifest is the only exact record of what the old version put on disk: the placement
	// heuristics describe the package as published now, not as it was installed.
	prev *store.InstanceMod
	// prevManifest is prev's decoded manifest, and prevStale the part the new version does
	// not write — the files an update removes. The rest are overwritten in place by Apply.
	prevManifest []installer.ManifestEntry
	prevStale    []string
}

// writePrevRows records every row this install is about to replace, before it replaces it.
func writePrevRows(stagingDir string, pkgs []*stagedPackage) error {
	for _, p := range pkgs {
		if p.prev == nil {
			continue
		}
		if err := fsutil.MkdirAllExact(prevRowDir(stagingDir)); err != nil {
			return fmt.Errorf("create the staging directory for replaced rows: %w", err)
		}
		raw, err := json.Marshal(replaced{Row: *p.prev, Stale: p.prevStale})
		if err != nil {
			return fmt.Errorf("encode the replaced row for %s: %w", p.fullName, err)
		}
		if err := fsutil.WriteFileAtomic(prevRowPath(stagingDir, p.fullName), raw); err != nil {
			return fmt.Errorf("record the replaced row for %s: %w", p.fullName, err)
		}
	}
	return nil
}

// runModInstall is the mod_install Runner. The phase order is load-bearing: resolve,
// download, stage, write the manifest, then move files. Every failure at or after the
// manifest is undone from the manifest, the only exact record of what moved.
func (i *Installer) runModInstall(inst *store.Instance, payload *InstallPayload) jobs.Runner {
	return func(ctx context.Context, h *jobs.Handle) jobs.Outcome {
		defer func() { _ = os.RemoveAll(payload.StagingDir) }()

		if len(payload.Updates) > 0 {
			h.Log(updateSummary(payload.Updates))
		}
		pkgs, err := i.prepareInstall(ctx, h, inst, payload)
		if err != nil {
			return outcomeOf(err)
		}
		if len(pkgs) == 0 {
			h.Progress(ctx, 100, "already installed; nothing to do")
			return jobs.Outcome{Status: jobs.StatusSucceeded}
		}
		// The archive comes after everything that can still be abandoned for free and before
		// the first file moves: a download that fails leaves no archive nobody needed, and the
		// world is saved before anything could change what it needs.
		var archived func(context.Context, *sql.Tx) error
		if payload.Backup || replacesInstalled(pkgs) {
			if h.CancelRequested(ctx) {
				return jobs.Outcome{Status: jobs.StatusCancelled}
			}
			var err error
			if archived, err = archiveBeforeUpdate(ctx, h, inst, i.ArchiveWorlds); err != nil {
				return jobs.Failed(i.failureCode(err), err)
			}
		}
		return withArchive(i.commitInstall(ctx, h, inst, payload, pkgs), archived)
	}
}

// replacesInstalled reports whether any package moves an installed one to another version.
func replacesInstalled(pkgs []*stagedPackage) bool {
	return slices.ContainsFunc(pkgs, func(p *stagedPackage) bool { return p.prev != nil })
}

// mark records a checkpoint, returning the terminal outcome if it could not be written — a
// job whose resume marker is not on the row is one the crash sweep would misread.
func mark(ctx context.Context, h *jobs.Handle, checkpoint string) error {
	if err := h.Checkpoint(ctx, checkpoint); err != nil {
		return fmt.Errorf("record checkpoint %s: %w", checkpoint, err)
	}
	return nil
}

// planError maps installer's typed refusals onto error codes. A package that cannot be placed
// and a path another package owns are both answers about the request, so neither is a 500.
func planError(err error) error {
	var conflict *installer.ConflictError
	if errors.As(err, &conflict) {
		return coded(errcode.ModConflict, err)
	}
	var unowned *installer.UnownedError
	if errors.As(err, &unowned) {
		return coded(errcode.ModUnownedFiles, unowned)
	}
	var dup *installer.DuplicateDestError
	switch {
	case errors.As(err, &dup),
		errors.Is(err, installer.ErrInvalidFullName),
		errors.Is(err, installer.ErrUnsupportedEntry),
		errors.Is(err, installer.ErrUnsafeDest):
		return coded(errcode.PackageInvalid, err)
	}
	return err
}

// jobError is a failure that names the error code a mod job reports it with.
type jobError struct {
	code errcode.Code
	err  error
}

func (e *jobError) Error() string { return e.err.Error() }

func (e *jobError) Unwrap() error { return e.err }

// coded attaches the error code a mod job reports err with.
func coded(code errcode.Code, err error) error { return &jobError{code: code, err: err} }

// outcomeOf is the terminal outcome of a mod job that stopped on err: cancelled for a
// cancelled download, the attached code for a coded error, and internal for anything else.
func outcomeOf(err error) jobs.Outcome {
	if errors.Is(err, errDownloadCancelled) {
		return jobs.Outcome{Status: jobs.StatusCancelled}
	}
	var je *jobError
	if errors.As(err, &je) {
		return jobs.Failed(je.code, err)
	}
	return jobs.Failed(errcode.Internal, err)
}
