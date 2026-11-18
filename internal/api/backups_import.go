package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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
	"github.com/valminhq/valmin/internal/store"
)

// backupEntryLimit bounds the files one backup archive may expand to. It is far above
// uploadEntryLimit because the archive is the panel's own, and a world's chunk files are one
// file each.
const backupEntryLimit = 1 << 16

// archiveStagingDir is where importBackup expands a whole archive before it picks one world out
// of it. It sits inside the staging directory, so a failure removes it with the rest.
const archiveStagingDir = ".archive"

// orphanedBackup is a world archive left under backups/ by a server that has been deleted.
type orphanedBackup struct {
	InstanceID string `json:"instance_id"`
	Filename   string `json:"filename"`
	SizeBytes  int64  `json:"size_bytes"`
	ModifiedAt string `json:"modified_at"`

	path    string
	modTime int64
}

// listOrphanedBackups lists the world archives under backups/ whose directory names no server,
// newest first. A server that is still being deleted still has its row, so its archives are not
// orphaned yet. A `.part` file is an archive still being written and is not listed.
func (h *Instances) listOrphanedBackups(ctx context.Context) ([]orphanedBackup, error) {
	root := instance.BackupsDir(h.Cfg.Data.Root)
	dirs, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	var found []orphanedBackup
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		inst, err := h.DB.InstanceByID(ctx, dir.Name())
		if err != nil {
			return nil, fmt.Errorf("look up server %s: %w", dir.Name(), err)
		}
		if inst != nil {
			continue
		}
		archives, err := orphanedArchives(filepath.Join(root, dir.Name()), dir.Name())
		if err != nil {
			return nil, err
		}
		found = append(found, archives...)
	}
	slices.SortFunc(found, func(a, b orphanedBackup) int { return cmp.Compare(b.modTime, a.modTime) })
	return found, nil
}

// orphanedArchives lists the finished world archives directly inside dir, the backup directory
// of the deleted server instanceID.
func orphanedArchives(dir, instanceID string) ([]orphanedBackup, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []orphanedBackup
	for _, f := range files {
		if !f.Type().IsRegular() || !strings.HasSuffix(f.Name(), ".tar.gz") {
			continue
		}
		info, err := f.Info()
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", f.Name(), err)
		}
		out = append(out, orphanedBackup{
			InstanceID: instanceID, Filename: f.Name(), SizeBytes: info.Size(),
			ModifiedAt: store.FormatTime(info.ModTime()),
			path:       filepath.Join(dir, f.Name()), modTime: info.ModTime().UnixNano(),
		})
	}
	return out, nil
}

// orphanedBackups is GET /backups/orphaned: the archives of deleted servers, for the import
// dialog. It is administrator-only, like the listing of containers no server claims.
func (h *Instances) orphanedBackups(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceAdopt, "") {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	found, err := h.listOrphanedBackups(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, NewPage(found, nil))
}

// importBackupRequest names the archive to take a world from: a backup in a live server's
// catalogue, or a leftover archive of a deleted server. World picks one when the archive holds
// several.
type importBackupRequest struct {
	SourceInstanceID string `json:"source_instance_id"`
	BackupID         string `json:"backup_id"`
	Filename         string `json:"filename"`
	World            string `json:"world"`
}

// validate checks that the request names a source server and exactly one way to find the
// archive in it.
func (req *importBackupRequest) validate() error {
	var val apierr.Validation
	if strings.TrimSpace(req.SourceInstanceID) == "" {
		val.Add("source_instance_id", apierr.FieldRequired, "source_instance_id is required.")
	}
	if (req.BackupID == "") == (req.Filename == "") {
		val.Add("backup_id", apierr.FieldInvalid, "Send exactly one of backup_id and filename.")
	}
	return val.Err()
}

// backupSource is an archive importBackup will read, and the world its catalogue row says it
// holds, when it has a row.
type backupSource struct {
	path  string
	world string
}

// resolveBackupSource finds the archive req names, after checking that u may take a world out
// of it. A backup of a live server needs that server visible and backups.download on it. A
// leftover archive needs the administrator's adopt action, and is found by matching the
// listing rather than by joining the request's strings onto a path.
func (h *Instances) resolveBackupSource(
	r *http.Request,
	u *store.User,
	req *importBackupRequest,
) (backupSource, error) {
	ctx := r.Context()
	if req.BackupID != "" {
		if !h.Authz.Can(ctx, u, authz.InstanceView, req.SourceInstanceID) {
			return backupSource{}, apierr.New(errcode.NotFound)
		}
		if !h.Authz.Can(ctx, u, authz.BackupsDownload, req.SourceInstanceID) {
			return backupSource{}, apierr.New(errcode.Forbidden)
		}
		b, err := h.DB.BackupByID(ctx, req.SourceInstanceID, req.BackupID)
		if err != nil {
			return backupSource{}, apierr.New(errcode.Internal).Wrap(err)
		}
		if b == nil {
			return backupSource{}, apierr.New(errcode.NotFound)
		}
		return backupSource{path: b.Path, world: b.WorldName}, nil
	}

	if !h.Authz.Can(ctx, u, authz.InstanceAdopt, "") {
		return backupSource{}, apierr.New(errcode.NotFound)
	}
	orphans, err := h.listOrphanedBackups(ctx)
	if err != nil {
		return backupSource{}, apierr.New(errcode.Internal).Wrap(err)
	}
	for i := range orphans {
		if orphans[i].InstanceID == req.SourceInstanceID && orphans[i].Filename == req.Filename {
			return backupSource{path: orphans[i].path}, nil
		}
	}
	return backupSource{}, apierr.New(errcode.NotFound)
}

// importBackup is POST /instances/{id}/worlds/import-backup: put a world from another server's
// backup, or from a deleted server's leftover archive, on this stopped server. It expands the
// archive, stages the one world asked for, and submits the same job as a world upload, so the
// world is validated, the current one is backed up first, and the swap is atomic.
func (h *Instances) importBackup(w http.ResponseWriter, r *http.Request) {
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
	if instance.State(inst.State) != instance.StateStopped {
		apierr.Write(w, r, apierr.New(errcode.InstanceMustBeStopped).With("state", inst.State))
		return
	}
	var req importBackupRequest
	if err := Decode(r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if err := req.validate(); err != nil {
		apierr.Write(w, r, err)
		return
	}
	source, err := h.resolveBackupSource(r, u, &req)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}

	budget, release, ok := h.reserveUpload(w, r, id)
	if !ok {
		return
	}
	defer release()
	staging, err := instance.NewImportStaging(h.Cfg.Data.Root, "backup-*")
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

	world, err := stageBackupWorld(source, req.World, staging, budget)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	audit := map[string]string{
		"source_instance_id": req.SourceInstanceID, "backup_id": req.BackupID,
		"filename": req.Filename, "world": world,
	}
	job, err := h.submitWorldImport(r, u, inst, staging, instance.RefuseBackupVariants,
		"instances.worlds.import_backup", audit)
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	submitted = true
	Accepted(w, r, job.ID, toJobView(job))
}

// stageBackupWorld expands the archive in source into staging and leaves exactly one world of
// it there, returning that world's name. want names the world; without it the world the
// archive's catalogue row names is taken if the archive holds it, and otherwise the only world
// there is. An archive that holds several and is not told which is a validation failure
// listing them.
func stageBackupWorld(source backupSource, want, staging string, budget int64) (string, error) {
	f, err := os.Open(source.path)
	if errors.Is(err, os.ErrNotExist) {
		return "", apierr.New(errcode.NotFound)
	}
	if err != nil {
		return "", apierr.New(errcode.Internal).Wrap(err)
	}
	defer func() { _ = f.Close() }()

	expanded := filepath.Join(staging, archiveStagingDir)
	if _, err := stageArchive(f, expanded, newUploadBudget(budget, backupEntryLimit)); err != nil {
		return "", storedArchiveError(diskLimited(err, budget))
	}
	scan, err := backup.ScanWorlds(expanded)
	if err != nil {
		return "", apierr.New(errcode.Internal).Wrap(err)
	}
	name, err := chooseWorld(scan, want, source.world)
	if err != nil {
		return "", err
	}
	found, err := completeWorld(expanded, name)
	if err != nil {
		return "", err
	}
	for _, rel := range found.Files {
		dst := filepath.Join(staging, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return "", apierr.New(errcode.Internal).Wrap(err)
		}
		if err := os.Rename(filepath.Join(expanded, rel), dst); err != nil {
			return "", apierr.New(errcode.Internal).Wrap(err)
		}
	}
	// The job scans staging, and the worlds left in the expanded copy would read as several.
	if err := os.RemoveAll(expanded); err != nil {
		return "", apierr.New(errcode.Internal).Wrap(err)
	}
	return name, nil
}

// storedArchiveError answers a damaged stored archive. stageArchive reports damage as the
// client's bad upload, which this is not: the archive is on the panel's own disk.
func storedArchiveError(err error) error {
	var apiErr *apierr.Error
	if errors.As(err, &apiErr) && apiErr.Code == errcode.InvalidParameter {
		return apierr.New(errcode.ValidationFailed).
			With("reason", "the backup archive could not be read").Wrap(err)
	}
	return err
}

// chooseWorld picks the world to import out of scan. want is the caller's choice and is
// required to exist; preferred is the world the archive's row names and is used when it is
// there. Otherwise the archive must hold exactly one world.
func chooseWorld(scan backup.WorldScan, want, preferred string) (string, error) {
	names := make([]string, 0, len(scan))
	for name := range scan {
		names = append(names, name)
	}
	slices.Sort(names)

	switch {
	case want != "":
		if _, ok := scan[want]; !ok {
			return "", apierr.New(errcode.ValidationFailed).
				With("reason", "the backup holds no world with that name").With("worlds", names)
		}
		return want, nil
	case len(names) == 0:
		return "", apierr.New(errcode.ValidationFailed).With("reason", "the backup holds no world")
	}
	if _, ok := scan[preferred]; ok {
		return preferred, nil
	}
	if len(names) > 1 {
		return "", apierr.New(errcode.ValidationFailed).
			With("reason", "the backup holds several worlds; name the one to import").With("worlds", names)
	}
	return names[0], nil
}
