package api

import (
	"archive/zip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// UploadLimitBytes is 11 §8.3's per-route override: the 1 MiB default is for JSON, and a
// world is hundreds of megabytes. A constant rather than a config key — 10 §1.1 names none,
// and a knob nobody turns is a knob nobody tests; it gains one the day an operator asks.
//
// `↯` 11 §8.1.1 / Q23: behind a reverse proxy this limit is *irrelevant*, because nginx's
// own client_max_body_size rejects the upload first with its own 413 that is not in the
// panel's envelope. At M1 this works on a direct deployment and fails behind nginx defaults.
const UploadLimitBytes = 4 << 30 // 4 GiB

// worldImportPayload is the job's persisted arguments (12 §4.1). The staging directory is on
// it so a crash-recovery sweep can find and delete what was left behind (12 §9.4).
type worldImportPayload struct {
	StagingDir         string `json:"staging_dir"`
	AllowBackupVariant bool   `json:"allow_backup_variant"`
}

// importWorld is POST /instances/{id}/worlds/import (04 §3, 12 §3.1): requires `stopped`,
// leaves the instance `stopped`, and holds the lock throughout without changing state.
func (h *Instances) importWorld(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.WorldImport, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	// `↯` C19: a job never implicitly stops a running server. An import against a running
	// instance is 409 instance_must_be_stopped, and the server keeps running.
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(apierr.InstanceMustBeStopped).With("state", inst.State))
		return
	}

	staging, err := os.MkdirTemp(instance.ImportStagingRoot(h.Cfg.Data.Root), "import-*")
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	// Everything below either submits a job that owns the staging directory, or fails and
	// must leave nothing behind (12 §9.4).
	submitted := false
	defer func() {
		if !submitted {
			_ = os.RemoveAll(staging)
		}
	}()

	allowVariant := r.URL.Query().Get("allow_backup_variant") == "true"
	if err := stageUpload(r, staging); err != nil {
		apierr.Write(w, r, err)
		return
	}

	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindWorldImport, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID,
		Payload: worldImportPayload{StagingDir: staging, AllowBackupVariant: allowVariant},
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			// `↯` A stopped→stopped compare-and-swap. It changes nothing and that is the
			// point: 12 §3.1 says this kind holds the lock without moving the state, and the
			// CAS is what makes "still stopped when the lock was taken" atomic with taking it.
			ok, err := store.TxUpdateInstanceState(
				ctx, tx, id, string(instance.StateStopped), string(instance.StateStopped))
			if err != nil {
				return fmt.Errorf("claim world_import for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, h.runWorldImport(inst, staging, allowVariant))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	submitted = true
	Accepted(w, r, job.ID, toJobView(job))
}

// stageUpload streams the request body to disk. `↯` It uses MultipartReader, not
// ParseMultipartForm: the latter buffers into memory up to its threshold and then into
// temporary files of its own choosing, and 11 §8.3 requires a world to reach disk without
// the daemon's RSS following it up.
func stageUpload(r *http.Request, staging string) error {
	mr, err := r.MultipartReader()
	if err != nil {
		return apierr.New(apierr.InvalidParameter).
			With("parameter", "body").
			Wrap(fmt.Errorf("expected a multipart upload: %w", err))
	}

	wrote := 0
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return apierr.New(apierr.PayloadTooLarge).With("limit_bytes", int64(UploadLimitBytes)).Wrap(err)
		}
		name := filepath.Base(part.FileName())
		if name == "" || name == "." || name == string(filepath.Separator) {
			_ = part.Close()
			continue
		}
		n, err := stagePart(part, staging, name)
		_ = part.Close()
		if err != nil {
			return err
		}
		wrote += n
	}
	if wrote == 0 {
		return apierr.New(apierr.WorldPairIncomplete)
	}
	return nil
}

// stagePart writes one uploaded file, expanding a zip in place. It returns how many files
// landed.
//
// `↯` Only the *basename* of a zip entry is ever used, and no path from the archive is
// joined onto anything. That is what makes zip-slip structurally impossible here rather
// than merely checked for: an entry named `../../etc/passwd` stages as a file called
// `passwd`, which then fails validation as neither a `.db` nor a `.fwl` (B5).
func stagePart(part *multipart.Part, staging, name string) (int, error) {
	if !strings.EqualFold(filepath.Ext(name), ".zip") {
		if err := writeStaged(part, filepath.Join(staging, name)); err != nil {
			return 0, err
		}
		return 1, nil
	}

	tmp := filepath.Join(staging, ".upload.zip")
	if err := writeStaged(part, tmp); err != nil {
		return 0, err
	}
	defer func() { _ = os.Remove(tmp) }()

	zr, err := zip.OpenReader(tmp)
	if err != nil {
		return 0, apierr.New(apierr.InvalidParameter).With("parameter", "file").
			Wrap(fmt.Errorf("the uploaded zip could not be read: %w", err))
	}
	defer func() { _ = zr.Close() }()

	wrote := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(filepath.FromSlash(f.Name))
		ext := strings.ToLower(filepath.Ext(base))
		if ext != ".db" && ext != ".fwl" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return 0, apierr.New(apierr.Internal).Wrap(err)
		}
		err = writeStaged(rc, filepath.Join(staging, base))
		_ = rc.Close()
		if err != nil {
			return 0, err
		}
		wrote++
	}
	return wrote, nil
}

// writeStaged copies src to path with a hard byte cap, so a body that lies about its length
// still cannot fill the disk.
func writeStaged(src io.Reader, path string) error {
	// path is the staging dir plus a basename; no caller-supplied directory reaches it.
	f, err := os.Create(path) //nolint:gosec // see above
	if err != nil {
		return apierr.New(apierr.Internal).Wrap(err)
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(f, io.LimitReader(src, UploadLimitBytes)); err != nil {
		return apierr.New(apierr.PayloadTooLarge).With("limit_bytes", int64(UploadLimitBytes)).Wrap(err)
	}
	if err := f.Close(); err != nil {
		return apierr.New(apierr.Internal).Wrap(err)
	}
	return nil
}

// runWorldImport is the job (12 §6): validate in staging, snapshot what is there, then move.
// Nothing under worlds/ is touched until the first two have both succeeded.
func (h *Instances) runWorldImport(inst *store.Instance, staging string, allowVariant bool) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		defer func() { _ = os.RemoveAll(staging) }()

		jh.Progress(ctx, 10, "validating the upload")
		world, violations := instance.ValidateImport(staging, allowVariant)
		if len(violations) > 0 {
			return jobs.Outcome{
				Status: "failed", ErrorCode: apierr.ValidationFailed.String(),
				Error: violations[0].Error(),
			}
		}
		if jh.CancelRequested(ctx) {
			return jobs.Outcome{Status: "cancelled"}
		}

		jh.Progress(ctx, 35, "backing up the world already there")
		snapshot, err := h.snapshotBeforeImport(ctx, inst)
		if err != nil {
			return jobs.Outcome{
				Status: "failed", ErrorCode: apierr.Internal.String(),
				Error: fmt.Sprintf("could not back up the existing world: %v", err),
			}
		}
		// `↯` The last point of no return (12 §8): past the move, the old world is gone from
		// worlds/ and only the snapshot has it.
		if jh.CancelRequested(ctx) {
			return jobs.Outcome{Status: "cancelled"}
		}

		jh.Progress(ctx, 75, "installing the world")
		if err := h.installWorld(inst, world); err != nil {
			return jobs.Outcome{
				Status: "failed", ErrorCode: apierr.Internal.String(),
				Error: fmt.Sprintf("could not install the world: %v", err),
			}
		}

		msg := "world imported"
		if world.Info.Name != inst.WorldName {
			// Not a failure: the game's own rolling backups carry a name that differs from
			// their filename (03 §4.1 rule 3, measured 31 Aug 2026). Surfaced so the operator
			// is not surprised by what the world calls itself.
			msg = fmt.Sprintf("world imported (its internal name is %q, the instance loads %q)",
				world.Info.Name, inst.WorldName)
		}
		jh.Progress(ctx, 100, msg)
		return jobs.Outcome{Status: "succeeded", OnFinish: snapshot}
	}
}

// snapshotBeforeImport is 03 §4.1 rule 6. It returns the OnFinish that records the archive,
// so the catalogue row lands in the job's own Finish transaction from data already in
// memory (12 §6) — and never before the archive file itself exists.
func (h *Instances) snapshotBeforeImport(
	ctx context.Context, inst *store.Instance,
) (func(context.Context, *sql.Tx) error, error) {
	// worldsDir is data_dir + "worlds"; data_dir is panel-generated and no user string
	// reaches the column (checked again by the delete job's own root guard).
	worldsDir := filepath.Clean(instance.WorldsDir(inst.DataDir))
	if _, err := os.Stat(worldsDir); errors.Is(err, os.ErrNotExist) {
		// Nothing to lose yet — a first import into a fresh instance.
		return nil, nil
	}

	dest := filepath.Join(instance.BackupsDir(h.Cfg.Data.Root), inst.ID,
		backup.Name(inst.Name, time.Now().UTC().Format("20060102T150405Z")))
	res, err := backup.Archive(worldsDir, dest)
	if err != nil {
		return nil, fmt.Errorf("archive %s: %w", worldsDir, err)
	}
	_ = ctx

	row := &store.Backup{
		ID: store.NewID(), InstanceID: inst.ID, Path: res.Path,
		SizeBytes: res.SizeBytes, SHA256: res.SHA256, WorldName: inst.WorldName,
		Trigger: store.TriggerPreImport,
		// The instance is stopped — 12 §3.1 requires it — so this archive is consistent by
		// construction, unlike M4's hot-copy mode (02 §4.4).
		Consistent: true,
	}
	return func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO backups (id, instance_id, path, size_bytes, sha256, world_name, trigger, consistent, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.ID, row.InstanceID, row.Path, row.SizeBytes, row.SHA256,
			row.WorldName, row.Trigger, row.Consistent, store.Now()); err != nil {
			return fmt.Errorf("record pre-import backup: %w", err)
		}
		return nil
	}, nil
}

// installWorld moves the validated pair into worlds_local/ under the instance's own world
// name.
//
// `↯` The rename is mandatory, not cosmetic: `-world` names the *file basename* (03 §1.3),
// so a world whose files keep the uploader's name is a world the server will never open. The
// internal name inside the `.fwl` is deliberately left alone — the game itself ships files
// whose internal name differs from their filename (03 §4.1 rule 3), so rewriting it would be
// changing bytes on the strength of an assumption nobody has measured.
func (h *Instances) installWorld(inst *store.Instance, world *instance.UploadedWorld) error {
	for _, ext := range []string{".db", ".fwl"} {
		src := world.DBPath
		if ext == ".fwl" {
			src = world.FWLPath
		}
		data, err := os.ReadFile(src) //nolint:gosec // src comes from ValidateImport over the panel's own staging dir
		if err != nil {
			return fmt.Errorf("read staged %s: %w", ext, err)
		}
		rel := filepath.Join(instance.WorldsLocalDir, inst.WorldName+ext)
		if err := instance.WriteWorldFile(inst.DataDir, rel, data); err != nil {
			return fmt.Errorf("install %s: %w", rel, err)
		}
	}
	return nil
}
