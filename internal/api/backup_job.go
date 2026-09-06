package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
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

// backupMode is what the caller asked for. It is not what the row records: consistency is
// decided by whether the server was actually running while the archive was written, so a hot
// copy of a stopped instance is recorded as the consistent archive it is.
type backupMode string

const (
	modeQuiesced backupMode = "quiesced"
	modeHot      backupMode = "hot"
)

// backupPayload is the job's persisted arguments (12 §4.1).
type backupPayload struct {
	Mode backupMode `json:"mode"`
	// Dest is the archive this job will write, recorded so crash recovery can delete the
	// `.part` an interrupted run left behind (12 §9.4).
	Dest string `json:"dest"`
}

// parseBackupMode reads the one query parameter. Absent means quiesced: the safe archive is
// what an operator who did not choose gets.
func parseBackupMode(r *http.Request) (backupMode, error) {
	switch raw := r.URL.Query().Get("mode"); raw {
	case "", string(modeQuiesced):
		return modeQuiesced, nil
	case string(modeHot):
		return modeHot, nil
	default:
		return "", apierr.New(apierr.InvalidParameter).With("parameter", "mode")
	}
}

// createBackup is POST /instances/{id}/backups (04 §3, 12 §3.1).
//
// A quiesced backup of a running server stops it, archives, and starts it again — the job
// sequence of 12 §2.3, one of the two kinds permitted to stop a server implicitly (12 §3.2).
// A hot copy never stops anything and enters no transient state (B12).
func (h *Instances) createBackup(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.BackupsCreate, id) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	mode, err := parseBackupMode(r)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	if !checkInstanceState(w, r, inst, jobs.KindBackup) {
		return
	}

	wasRunning := inst.State == string(instance.StateRunning)
	// A hot copy of a running server needs the container; a quiesced one needs it to stop it.
	containerID := ""
	if wasRunning {
		if containerID, ok = h.mustHaveContainer(w, r, inst); !ok {
			return
		}
	}

	backupID := store.NewID()
	dest := archivePath(h.Cfg.Data.Root, inst, backupID)
	quiescing := mode == modeQuiesced && wasRunning

	job, err := h.Engine.Submit(r.Context(), &jobs.Spec{
		Kind: jobs.KindBackup, LockKey: jobs.InstanceLockKey(id),
		InstanceID: &id, InstanceName: inst.Name, RequestedBy: u.ID,
		Payload: backupPayload{Mode: mode, Dest: dest},
		// The durable answer to "this server was running and owes the user a restart",
		// written before the stop rather than after it (12 §9.3).
		ResumeAfter: quiescing,
		OnClaim: func(ctx context.Context, tx *sql.Tx) error {
			return claimBackup(ctx, tx, id, quiescing)
		},
	}, h.runBackup(inst, containerID, mode, backupID, dest, wasRunning))
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// claimBackup makes the transition the mode implies: a quiesced backup of a running server
// enters `stopping` as a stop would, and every other case holds the lock without touching
// state (12 §2.3).
func claimBackup(ctx context.Context, tx *sql.Tx, instanceID string, quiescing bool) error {
	if !quiescing {
		return nil
	}
	ok, err := store.TxUpdateInstanceState(
		ctx, tx, instanceID, string(instance.StateRunning), string(instance.StateStopping))
	if err != nil {
		return fmt.Errorf("claim backup for instance %s: %w", instanceID, err)
	}
	if !ok {
		return fmt.Errorf("instance %s not in running state at claim", instanceID)
	}
	return nil
}

// runBackup is the backup job's Runner (02 §4.4).
func (h *Instances) runBackup(
	inst *store.Instance, containerID string, mode backupMode, backupID, dest string, wasRunning bool,
) jobs.Runner {
	return func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		quiescing := mode == modeQuiesced && wasRunning
		// resume is what the server is owed once this job lets go of the lock. It is attached
		// to every outcome below, not only the successful one: the operator asked for a
		// backup, not a shutdown, and a failed 3 a.m. backup that leaves the server down until
		// morning has done more harm than the missing archive.
		resume := h.resumeAfterBackup(inst, containerID, quiescing)
		if quiescing {
			out, parked, ok := h.quiesce(ctx, jh, inst.ID, containerID)
			if !ok {
				// Parked in `error`: on-disk state is not the question, but 12 §2.4 permits no
				// start from `error`, and a stop that failed or was force-killed is exactly the
				// case a human should look at.
				if !parked {
					out.AfterFinish = resume
				}
				return out
			}
		}

		// A hot copy of a running server is the one archive taken over a live world, so it is
		// the one that cannot claim consistency (B12).
		consistent := mode != modeHot || !wasRunning
		if jh.CancelRequested(ctx) {
			return h.abandonBackup(ctx, inst.ID, quiescing, jobs.Outcome{
				Status: "cancelled", AfterFinish: resume,
			})
		}

		jh.Progress(ctx, 55, "archiving the world")
		row, err := h.archiveAndVerify(inst, backupID, dest, consistent)
		if err != nil {
			code := apierr.Internal.String()
			if isUnverifiable(err) {
				code = apierr.BackupUnverifiable.String()
			}
			return h.abandonBackup(ctx, inst.ID, quiescing, jobs.Outcome{
				Status: "failed", ErrorCode: code, Error: err.Error(), AfterFinish: resume,
			})
		}

		jh.Progress(ctx, 90, "pruning old archives")
		pruned, err := h.pruneArchives(ctx, inst, row)
		if err != nil {
			// The archive is written and verified; failing the job now would discard a good
			// backup over housekeeping. Say so and succeed.
			jh.Log("could not prune old archives: " + err.Error())
		}

		jh.Progress(ctx, 100, backupMessage(consistent))
		return jobs.Outcome{
			Status:   "succeeded",
			OnFinish: finishBackup(inst.ID, quiescing, row, pruned),
			// The chained start of 12 §2.3, after the lock is released (12 §9.3).
			AfterFinish: resume,
		}
	}
}

// quiesce is 02 §4.4 steps 2 and 3: stop the server and require the save-complete line.
// Where `stop` records clean=false and carries on, backup refuses to archive — a catalogue
// holding a file marked consistent that is not is worse than no backup (12 §3.4).
// The second return reports whether the instance was parked in `error`, which decides whether
// the server may be started again afterwards.
func (h *Instances) quiesce(
	ctx context.Context, jh *jobs.Handle, instanceID, containerID string,
) (out jobs.Outcome, parked, ok bool) {
	jh.Progress(ctx, 20, "stopping the server")
	clean, timedOut, err := h.stopContainer(ctx, containerID)
	switch {
	case err != nil:
		return jobs.Outcome{
			Status: "failed", ErrorCode: apierr.Internal.String(), Error: err.Error(),
			OnFinish: finishToError(instanceID, instance.StateStopping),
		}, true, false
	case timedOut:
		return jobs.Outcome{
			Status: "failed", ErrorCode: apierr.Internal.String(),
			Error:    "the server did not stop within the timeout and was force-killed",
			OnFinish: finishToError(instanceID, instance.StateStopping),
		}, true, false
	}

	if _, err := h.DB.UpdateInstanceState(ctx, instanceID,
		string(instance.StateStopping), string(instance.StateStopped)); err != nil {
		return jobs.Outcome{
			Status: "failed", ErrorCode: apierr.Internal.String(),
			Error: fmt.Sprintf("move instance %s to stopped: %v", instanceID, err),
		}, false, false
	}
	if !clean {
		no := false
		return jobs.Outcome{
			Status: "failed", ErrorCode: apierr.BackupUnverifiable.String(), Clean: &no,
			Error: "the server stopped without confirming it had written the world, " +
				"so no archive was taken",
		}, false, false
	}

	if _, err := h.DB.UpdateInstanceState(ctx, instanceID,
		string(instance.StateStopped), string(instance.StateBackingUp)); err != nil {
		return jobs.Outcome{
			Status: "failed", ErrorCode: apierr.Internal.String(),
			Error: fmt.Sprintf("move instance %s to backing_up: %v", instanceID, err),
		}, false, false
	}
	return jobs.Outcome{}, false, true
}

// archiveAndVerify writes the archive and proves it is one before any row names it
// (02 §4.4 steps 4 and 5). A verification failure removes the file: an archive nothing
// vouches for must not be left where a later operator reads it as a backup.
func (h *Instances) archiveAndVerify(
	inst *store.Instance, backupID, dest string, consistent bool,
) (*store.Backup, error) {
	res, err := backup.Archive(instance.WorldsDir(inst.DataDir), dest)
	if err != nil {
		return nil, fmt.Errorf("archive the world: %w", err)
	}
	if _, err := backup.Verify(dest, inst.WorldName); err != nil {
		// dest is panel-built from data.root, the instance id and a timestamp; no request
		// value reaches it (D13).
		_ = os.Remove(dest)
		return nil, fmt.Errorf("verify the archive: %w", err)
	}

	return &store.Backup{
		ID: backupID, InstanceID: inst.ID, Path: res.Path,
		SizeBytes: res.SizeBytes, SHA256: res.SHA256, WorldName: inst.WorldName,
		Trigger: store.TriggerManual, Consistent: consistent,
	}, nil
}

// archivePath is where an archive of inst identified by backupID lands. Built from data.root,
// the instance id and that id, so no request value reaches it (D13).
func archivePath(dataRoot string, inst *store.Instance, backupID string) string {
	return filepath.Join(instance.BackupsDir(dataRoot), inst.ID,
		backup.Name(inst.Name, time.Now().UTC().Format("20060102T150405Z"), backupID))
}

// isUnverifiable reports whether err is the archive refusing to vouch for itself rather than
// the panel failing, so the job carries the registry code an operator can act on.
func isUnverifiable(err error) bool {
	return errors.Is(err, backup.ErrArchiveUnreadable) ||
		errors.Is(err, backup.ErrWorldMissing) ||
		errors.Is(err, backup.ErrWorldImplausible)
}

// pruneArchives is 02 §4.4 step 7, run once the new archive exists so retention is applied
// to the catalogue the operator will actually see. It unlinks the files; the rows go in the
// job's Finish transaction, since a filesystem call never happens inside one (C1).
func (h *Instances) pruneArchives(
	ctx context.Context, inst *store.Instance, fresh *store.Backup,
) ([]backup.Entry, error) {
	rows, err := h.DB.ListBackups(ctx, inst.ID, "", "", pruneScanLimit)
	if err != nil {
		return nil, fmt.Errorf("read the catalogue: %w", err)
	}

	// The archive this job just wrote has no row yet, and it is the newest, so it leads the
	// list retention counts from.
	entries := []backup.Entry{{ID: fresh.ID, Path: fresh.Path, Consistent: fresh.Consistent}}
	for i := range rows {
		entries = append(entries, backup.Entry{
			ID: rows[i].ID, Path: rows[i].Path, Consistent: rows[i].Consistent,
		})
	}

	doomed := backup.Prune(entries, backup.Policy{
		KeepCold: inst.BackupKeepCold, KeepHot: inst.BackupKeepHot,
	})
	for _, a := range doomed {
		if err := backup.Remove(a); err != nil {
			return nil, fmt.Errorf("prune archives for instance %s: %w", inst.ID, err)
		}
	}
	return doomed, nil
}

// pruneScanLimit bounds the catalogue page retention is computed over. Far above any
// plausible keep count, so the page always contains everything a policy could spare.
const pruneScanLimit = 500

// finishBackup writes the new catalogue row and removes the pruned ones, alongside the state
// flip, in the job's own Finish transaction (12 §6).
func finishBackup(
	instanceID string, quiescing bool, row *store.Backup, pruned []backup.Entry,
) func(context.Context, *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		if err := store.TxCreateBackup(ctx, tx, row); err != nil {
			return fmt.Errorf("record backup for instance %s: %w", instanceID, err)
		}
		for _, a := range pruned {
			if err := store.TxDeleteBackup(ctx, tx, instanceID, a.ID); err != nil {
				return fmt.Errorf("prune archive %s: %w", a.ID, err)
			}
		}
		if !quiescing {
			return nil
		}
		ok, err := store.TxUpdateInstanceState(
			ctx, tx, instanceID, string(instance.StateBackingUp), string(instance.StateStopped))
		if err != nil {
			return fmt.Errorf("finish backup for instance %s: %w", instanceID, err)
		}
		if !ok {
			return fmt.Errorf("finish backup for instance %s: not in backing_up state", instanceID)
		}
		return nil
	}
}

// abandonBackup returns out with the transition a quiesced job still owes: the instance is in
// `backing_up` or `stopping` and must not be left there. The world was never touched, so it
// resolves to `stopped` rather than `error`.
func (h *Instances) abandonBackup(
	ctx context.Context, instanceID string, quiescing bool, out jobs.Outcome,
) jobs.Outcome {
	if !quiescing || out.OnFinish != nil {
		return out
	}
	if _, err := h.DB.UpdateInstanceState(ctx, instanceID,
		string(instance.StateBackingUp), string(instance.StateStopped)); err != nil {
		out.Error += fmt.Sprintf(" (and instance %s could not be returned to stopped: %v)",
			instanceID, err)
	}
	return out
}

// resumeAfterBackup starts the server the job stopped, once the Finish transaction has
// committed and the lock is released — a job cannot submit another on its own lock key while
// holding it (12 §2.3, §9.3).
func (h *Instances) resumeAfterBackup(
	inst *store.Instance, containerID string, quiescing bool,
) func(context.Context) {
	if !quiescing {
		return nil
	}
	return func(ctx context.Context) {
		id := inst.ID
		if _, err := h.Engine.Submit(ctx, &jobs.Spec{
			Kind: jobs.KindStart, LockKey: jobs.InstanceLockKey(id),
			InstanceID: &id, InstanceName: inst.Name,
			Payload: struct{}{},
			OnClaim: func(ctx context.Context, tx *sql.Tx) error {
				ok, err := store.TxUpdateInstanceState(
					ctx, tx, id, string(instance.StateStopped), string(instance.StateStarting))
				if err != nil {
					return fmt.Errorf("claim start after backup for instance %s: %w", id, err)
				}
				if !ok {
					return fmt.Errorf("instance %s not in stopped state after backup", id)
				}
				return nil
			},
		}, h.runStart(id, containerID)); err != nil {
			slog.WarnContext(ctx, "could not restart the server after its backup",
				slog.String("instance_id", id), slog.Any("error", err))
		}
	}
}

func backupMessage(consistent bool) string {
	if consistent {
		return "backup complete"
	}
	return "hot copy complete (taken while the server was running, so it is best-effort)"
}

// archiveOnRestart takes the cold archive a restart can have almost for free: it runs between
// the stop and the start, when the world is already flushed and the container already down.
// Off unless the instance opts in, since it adds its own duration to every restart.
//
// clean is the stop's save-complete signal, and gates the whole thing: an archive taken after
// an unconfirmed save would be recorded consistent when it is not, which is the one thing
// 12 §3.4 forbids. A restart tolerates a missing save line where a backup does not, so here it
// costs the archive rather than the job.
//
// It returns the Finish callback that records the archive, or nil. A failure never fails the
// restart: no row is written, the job's log says why, and the server still starts.
func (h *Instances) archiveOnRestart(
	ctx context.Context, jh *jobs.Handle, inst *store.Instance, clean bool,
) func(context.Context, *sql.Tx) error {
	if !inst.BackupOnRestart {
		return nil
	}
	if !clean {
		jh.Log("no archive was taken on this restart: the server stopped without confirming " +
			"it had written the world")
		return nil
	}

	jh.Progress(ctx, 40, "archiving the world")
	backupID := store.NewID()
	row, err := h.archiveAndVerify(inst, backupID, archivePath(h.Cfg.Data.Root, inst, backupID), true)
	if err != nil {
		jh.Log("no archive was taken on this restart: " + err.Error())
		return nil
	}
	pruned, err := h.pruneArchives(ctx, inst, row)
	if err != nil {
		jh.Log("could not prune old archives: " + err.Error())
	}
	return finishBackup(inst.ID, false, row, pruned)
}
