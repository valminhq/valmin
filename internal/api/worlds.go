package api

import (
	"archive/zip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/backup"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// UploadLimitBytes is 11 §8.3's per-route override: the 1 MiB default is for JSON, and a world
// is hundreds of megabytes.
//
// Behind a reverse proxy it is irrelevant (Q23): nginx's own client_max_body_size rejects the
// upload first, with a 413 outside the panel's envelope.
const (
	UploadLimitBytes = 4 << 30 // 4 GiB
	uploadEntryLimit = 128
)

// The two files a Valheim world is (03 §1). Both must move together: a .db without its
// .fwl is not a world the server will load.
const (
	worldDBExt  = ".db"
	worldFWLExt = ".fwl"
)

type uploadBudget struct {
	limit      int64
	remaining  int64
	entries    int
	maxEntries int
}

func newUploadBudget(limit int64, maxEntries int) *uploadBudget {
	return &uploadBudget{limit: limit, remaining: limit, maxEntries: maxEntries}
}

func (b *uploadBudget) write(src io.Reader, path string) error {
	if b.entries >= b.maxEntries {
		return apierr.New(errcode.PayloadTooLarge).
			With("limit_bytes", b.limit).
			With("limit_entries", b.maxEntries)
	}
	b.entries++
	n, err := copyStaged(src, path, b.remaining)
	b.remaining -= min(n, b.remaining)
	if err != nil {
		var apiErr *apierr.Error
		if errors.As(err, &apiErr) && apiErr.Code == errcode.PayloadTooLarge {
			return apierr.New(errcode.PayloadTooLarge).
				With("limit_bytes", b.limit).
				With("limit_entries", b.maxEntries)
		}
		return err
	}
	return nil
}

// importWorld is POST /instances/{id}/worlds/import (04 §3, 12 §3.1): requires `stopped`,
// leaves the instance `stopped`, and holds the lock throughout without changing state.
// worldView is one world the panel can see in an instance's savedir. A null size is a file
// that is not there, which is a different statement from zero and is how half a pair reads.
type worldView struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
	// Layout is "directory" for 1.0's world directory and "pair" for the pre-1.0 `.db`/`.fwl`
	// (03 §4). Both are in the field and an operator looking at a savedir sees one of them.
	Layout string `json:"layout"`
	// Bytes is everything the world occupies. For 1.0 that is the whole directory: the `.db2`
	// alone is a small part of a world whose chunks hold the rest.
	Bytes      int64  `json:"bytes"`
	DBBytes    *int64 `json:"db_bytes"`
	FWLBytes   *int64 `json:"fwl_bytes"`
	ModifiedAt string `json:"modified_at"`
	// Loaded marks the world this instance is configured to load. False on every row means the
	// server would find nothing, which is the answer a backup refusing to verify is reporting
	// from the other end (02 §4.4 step 5).
	Loaded bool `json:"loaded"`
	// Complete is both halves of the pair present. A world missing one is not loadable and is
	// not archivable, and the panel says so here rather than only when a backup refuses.
	Complete bool `json:"complete"`
}

// listWorlds is GET /instances/{id}/worlds (04 §3): what the panel can actually see under this
// instance's savedir, which is the tree a backup archives.
//
// It exists because the failure it answers is silent from every other direction: a backup that
// refuses to verify says the pair it wanted is not in the archive, and nothing said whether the
// world is under another name, in another directory, or not there at all (operator report,
// 14 Sep 2026). Gated on backups.list, the same viewer capability as the catalogue it explains.
func (h *Instances) listWorlds(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.BackupsList, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	worlds, err := instance.ListWorlds(inst.DataDir)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	items := make([]worldView, 0, len(worlds))
	for i := range worlds {
		world := &worlds[i]
		items = append(items, worldView{
			Name: world.Name, Dir: world.Dir, Layout: layoutName(world.Directory),
			Bytes:   world.Bytes,
			DBBytes: sizeOrNil(world.DataBytes), FWLBytes: sizeOrNil(world.HeaderBytes),
			ModifiedAt: store.FormatTime(world.ModifiedAt),
			Loaded:     world.Name == inst.WorldName,
			Complete:   world.Loadable(),
		})
	}
	// One instance holds a handful of worlds, so there is nothing to page through (04 §3).
	JSON(w, r, http.StatusOK, NewPage(items, nil))
}

// layoutName names which of 03 §4's two on-disk shapes this world is in.
func layoutName(directory bool) string {
	if directory {
		return "directory"
	}
	return "pair"
}

// sizeOrNil renders ListWorlds's -1 as JSON null: the file is absent, not empty.
func sizeOrNil(size int64) *int64 {
	if size < 0 {
		return nil
	}
	return &size
}

func (h *Instances) importWorld(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.WorldImport, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	// C19: a job never implicitly stops a running server. An import against a running
	// instance is 409 instance_must_be_stopped, and the server keeps running.
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(errcode.InstanceMustBeStopped).With("state", inst.State))
		return
	}

	staging, err := os.MkdirTemp(instance.ImportStagingRoot(h.Cfg.Data.Root), "import-*")
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
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
		Payload: control.WorldImportPayload{StagingDir: staging, AllowBackupVariant: allowVariant},
		Audit: jobAudit(r.Context(), u.ID, id, "instances.worlds.import",
			map[string]string{"world": stagedWorldName(staging)}),
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			// A stopped→stopped compare-and-swap. It changes nothing and that is the
			// point: 12 §3.1 says this kind holds the lock without moving the state, and the
			// CAS is what makes "still stopped when the lock was taken" atomic with taking it.
			ok, err := instance.HoldStateTx(ctx, tx, id, instance.StateStopped)
			if err != nil {
				return fmt.Errorf("claim world_import for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, h.snapshotter().RunWorldImport(inst, staging, allowVariant))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	submitted = true
	Accepted(w, r, job.ID, toJobView(job))
}

// restoreWorldFromDisk is POST /instances/{id}/worlds/{name}/restore: roll the instance's world
// back to another world already in its savedir, without a download-and-reupload round trip.
//
// Its reason for existing is 03 §4.1 rule 5's other half. The game keeps rolling saves beside
// the live world and the panel already lists them, but the only way to go back to one was to
// fetch it off the host and upload it again. They are ordinary worlds to the scanner (ADR-179),
// so the whole of world import applies to them unchanged — validation, the snapshot of what is
// being replaced, and the atomic swap. This handler only fills the staging directory from disk
// instead of from a request body, and then submits that same job.
//
// allow_backup_variant is implied rather than asked: rule 5 exists so a bulk upload does not
// silently restore an older state, and naming one world is the explicit choice it wants.
func (h *Instances) restoreWorldFromDisk(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.WorldImport, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	// C19, as for an upload: the world this replaces is the one players would be in.
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(errcode.InstanceMustBeStopped).With("state", inst.State))
		return
	}

	staging, err := os.MkdirTemp(instance.ImportStagingRoot(h.Cfg.Data.Root), "restore-*")
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	submitted := false
	defer func() {
		if !submitted {
			_ = os.RemoveAll(staging)
		}
	}()

	if err := stageWorldFromDisk(inst, r.PathValue("name"), staging); err != nil {
		apierr.Write(w, r, err)
		return
	}

	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindWorldImport, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID,
		Payload: control.WorldImportPayload{StagingDir: staging, AllowBackupVariant: true},
		Audit: jobAudit(r.Context(), u.ID, id, "instances.worlds.restore",
			map[string]string{"world": r.PathValue("name")}),
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := instance.HoldStateTx(ctx, tx, id, instance.StateStopped)
			if err != nil {
				return fmt.Errorf("claim world_import for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, h.snapshotter().RunWorldImport(inst, staging, true))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	submitted = true
	Accepted(w, r, job.ID, toJobView(job))
}

// deleteWorld is DELETE /instances/{id}/worlds/{name}: remove one world from the instance's
// savedir, archiving it first. Deleting the loaded world resets the server, which generates a
// fresh one on its next start.
func (h *Instances) deleteWorld(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	// Removing a world is gated by the action that authorizes replacing one: same bytes,
	// same undo (ADR-186a).
	if !h.Authz.Can(r.Context(), u, authz.WorldImport, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	// C19: the world being removed is the one players would be in.
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(errcode.InstanceMustBeStopped).With("state", inst.State))
		return
	}

	// Matched against the listing rather than joined onto a path: it names a world this
	// instance's savedir holds or it is a 404 (ADR-038, B5).
	name := r.PathValue("name")
	worlds, err := instance.ListWorlds(inst.DataDir)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	idx := slices.IndexFunc(worlds, func(world instance.World) bool { return world.Name == name })
	if idx < 0 {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	world := worlds[idx]

	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindWorldDelete, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID,
		Payload: control.WorldDeletePayload{World: name},
		Audit:   jobAudit(r.Context(), u.ID, id, "instances.worlds.delete", map[string]string{"world": name}),
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			ok, err := instance.HoldStateTx(ctx, tx, id, instance.StateStopped)
			if err != nil {
				return fmt.Errorf("claim world_delete for instance %s: %w", id, err)
			}
			if !ok {
				return fmt.Errorf("instance %s is no longer stopped", id)
			}
			return nil
		},
	}, h.snapshotter().RunWorldDelete(inst, &world))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// stageWorldFromDisk copies one world out of the instance's worlds_local/ into staging, leaving
// the original where it is so the same rollback can be taken twice.
//
// name is looked up as an exact key in the scan rather than joined onto a path, so a traversal
// attempt cannot name anything: it either matches a world the scanner found in this instance's
// own savedir or it is a 404 (ADR-038, B5).
func stageWorldFromDisk(inst *store.Instance, name, staging string) error {
	local := filepath.Join(instance.WorldsDir(inst.DataDir), instance.WorldsLocalDir)
	scan, err := backup.ScanWorlds(local)
	if err != nil {
		return apierr.New(errcode.Internal).Wrap(err)
	}
	found, ok := scan[name]
	if !ok {
		return apierr.New(errcode.NotFound)
	}
	if !found.Complete() {
		return apierr.New(errcode.ValidationFailed).
			With("world", name).
			With("reason", "half a world: it is missing one of its two halves")
	}
	for _, rel := range found.Files {
		if err := copyInto(filepath.Join(local, rel), filepath.Join(staging, rel)); err != nil {
			return apierr.New(errcode.Internal).Wrap(err)
		}
	}
	return nil
}

// stagedWorldName is the one world an upload staged, or "" when it holds none or several: the
// import job's own validation rejects those, so the entry then names nothing.
func stagedWorldName(staging string) string {
	scan, err := backup.ScanWorlds(staging)
	if err != nil || len(scan) != 1 {
		return ""
	}
	for name := range scan {
		return name
	}
	return ""
}

// copyInto copies one file, creating the directory a 1.0 world needs above it.
func copyInto(src, dst string) error {
	//nolint:gosec // G703: dst is staging joined with a path the scanner produced by walking
	// the instance's own savedir, so it carries no caller string and filepath.Rel has already
	// resolved it; src is that same walk's own output.
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
	}
	in, err := os.Open(src) //nolint:gosec // G304: src is a path the scanner found under the
	// instance's own savedir, never a caller string.
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst) //nolint:gosec // G304: panel-built staging path.
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dst, err)
	}
	return nil
}

// stageUpload streams the request body to disk. It uses MultipartReader rather than
// ParseMultipartForm, which buffers into memory and then into temporary files of its own
// choosing: a world must reach disk without the daemon's RSS following it (11 §8.3).
func stageUpload(r *http.Request, staging string) error {
	return stageUploadWithLimits(r, staging, UploadLimitBytes, uploadEntryLimit)
}

func stageUploadWithLimits(r *http.Request, staging string, limit int64, maxEntries int) error {
	mr, err := r.MultipartReader()
	if err != nil {
		return apierr.New(errcode.InvalidParameter).
			With("parameter", "body").
			Wrap(fmt.Errorf("expected a multipart upload: %w", err))
	}

	budget := newUploadBudget(limit, maxEntries)
	wrote := 0
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return apierr.New(errcode.PayloadTooLarge).With("limit_bytes", int64(UploadLimitBytes)).Wrap(err)
		}
		// The supplied name is carried whole to stagedName, which rebuilds it from at most
		// two components it has taken the base of: a browser uploading a folder sends
		// `Worild1/_main.17.db2`, and that directory is the world's name (ADR-180).
		name := suppliedName(part)
		if base := filepath.Base(name); base == "" || base == "." || base == string(filepath.Separator) {
			_ = part.Close()
			continue
		}
		n, err := stagePart(part, staging, name, budget)
		_ = part.Close()
		if err != nil {
			return err
		}
		wrote += n
	}
	if wrote == 0 {
		return apierr.New(errcode.WorldPairIncomplete)
	}
	return nil
}

// suppliedName is the filename as the client actually wrote it.
//
// `multipart.Part.FileName` cannot be used: it returns `filepath.Base` of what was sent, which
// is the right default for a server that wants one file and wrong here, because the directory
// it removes is a 1.0 world's name. The raw `Content-Disposition` still carries it. Nothing
// downstream trusts the value — stagedName rebuilds a path from at most two components it has
// taken the base of — so reading it back costs no safety (B5).
func suppliedName(part *multipart.Part) string {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil {
		return part.FileName()
	}
	if name := params["filename"]; name != "" {
		return name
	}
	return part.FileName()
}

// stagedName is where an uploaded entry lands, and it is where the structural safety of the
// whole import lives (B5).
//
// At most **two** components survive, each of them a `filepath.Base` of what the upload said:
// the file, and the one directory above it. No supplied path is ever joined onto the staging
// root, so neither a zip entry nor a browser's directory upload can climb out of it — the
// property the old flatten-to-basename rule had, kept.
//
// The directory has to survive because a 1.0 world *is* a directory and its name is the
// world's name: flattening `Worild1/_main.14.db2` to `_main.14.db2` throws away the only
// record of which world it is, and collides the moment two worlds are in one upload
// (ADR-180). A file with no directory over it stages at the root, which is what a pair
// uploaded as two files has always done.
func stagedName(staging, supplied string) string {
	clean := filepath.FromSlash(strings.ReplaceAll(supplied, `\`, "/"))
	base := filepath.Base(clean)
	parent := filepath.Base(filepath.Dir(clean))
	if parent == "." || parent == string(filepath.Separator) || parent == ".." {
		return filepath.Join(staging, base)
	}
	return filepath.Join(staging, parent, base)
}

// stagePart writes one uploaded file, expanding a zip in place, and returns how many files
// landed. Paths are rebuilt by stagedName, never joined, which makes zip-slip structurally
// impossible rather than merely checked for (B5).
func stagePart(part *multipart.Part, staging, name string, budget *uploadBudget) (int, error) {
	if !strings.EqualFold(filepath.Ext(name), ".zip") {
		if err := budget.write(part, stagedName(staging, name)); err != nil {
			return 0, err
		}
		return 1, nil
	}

	tmp := filepath.Join(staging, ".upload.zip")
	if err := budget.write(part, tmp); err != nil {
		return 0, err
	}
	defer func() { _ = os.Remove(tmp) }()

	zr, err := zip.OpenReader(tmp)
	if err != nil {
		return 0, apierr.New(errcode.InvalidParameter).With("parameter", "file").
			Wrap(fmt.Errorf("the uploaded zip could not be read: %w", err))
	}
	defer func() { _ = zr.Close() }()

	wrote := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !wantedInUpload(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return 0, apierr.New(errcode.Internal).Wrap(err)
		}
		err = budget.write(rc, stagedName(staging, f.Name))
		_ = rc.Close()
		if err != nil {
			return 0, err
		}
		wrote++
	}
	return wrote, nil
}

// wantedInUpload keeps a zip's world files and drops the rest, so a user who zips their whole
// save folder does not spend the upload budget on screenshots. It takes both halves of both
// layouts plus the chunk files and per-save markers a 1.0 world needs to load at all: a world
// directory missing its chunks is a world missing most of itself (ADR-180).
func wantedInUpload(name string) bool {
	base := strings.ToLower(filepath.Base(filepath.FromSlash(name)))
	switch filepath.Ext(base) {
	case worldDBExt, worldFWLExt, ".db2", ".fwl2", ".chunk", ".chunks", ".ok":
		return true
	}
	return false
}

// writeStaged copies src to path with a hard byte cap, so a body that lies about its length
// still cannot fill the disk. It reads one byte past limit, because a reader truncated at the
// cap reports EOF rather than an error and an oversized upload would otherwise land as a
// prefix that validation cannot tell from a whole world.
func writeStaged(src io.Reader, path string, limit int64) error {
	_, err := copyStaged(src, path, limit)
	return err
}

func copyStaged(src io.Reader, path string, limit int64) (int64, error) {
	// A 1.0 world stages one directory deep, and that directory is a name stagedName rebuilt
	// rather than one the upload supplied.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return 0, apierr.New(errcode.Internal).Wrap(err)
	}
	// path is the staging dir plus at most a directory and a basename, both rebuilt by
	// stagedName; no caller-supplied path reaches it.
	f, err := os.Create(path) //nolint:gosec // see above
	if err != nil {
		return 0, apierr.New(errcode.Internal).Wrap(err)
	}
	defer func() { _ = f.Close() }()

	n, err := io.CopyN(f, src, limit+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, apierr.New(errcode.Internal).Wrap(err)
	}
	if n > limit {
		return n, apierr.New(errcode.PayloadTooLarge).With("limit_bytes", limit)
	}
	if err := f.Close(); err != nil {
		return n, apierr.New(errcode.Internal).Wrap(err)
	}
	return n, nil
}
