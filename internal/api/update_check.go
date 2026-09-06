package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

const publicBuildKey = "steam_public_build"

type publicBuild struct {
	BuildID    string    `json:"build_id"`
	ObservedAt time.Time `json:"observed_at"`
}

type updateStatusView struct {
	InstalledBuildID *string    `json:"installed_build_id"`
	PublicBuildID    *string    `json:"public_build_id"`
	ObservedAt       *time.Time `json:"observed_at"`
	UpdateAvailable  *bool      `json:"update_available"`
}

// updateStatus reports the last successful observation; failed checks never replace it.
func (h *Instances) updateStatus(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	var observed publicBuild
	found, err := h.DB.KVGet(r.Context(), publicBuildKey, &observed)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	v := updateStatusView{}
	// The manifest under server/ is what the instance actually runs; the column is a cache of
	// it, and a game update whose recovery completed the swap without reaching its Finish
	// transaction leaves the two disagreeing. Legacy rows carry a cache alias rather than a
	// version, which the column could not answer either way.
	installed, err := instance.InstalledBuildID(inst.DataDir)
	if err != nil {
		installed = deref(inst.GameBuildID)
	}
	if knownBuildID(installed) {
		v.InstalledBuildID = &installed
	}
	if found && knownBuildID(observed.BuildID) && !observed.ObservedAt.IsZero() {
		v.PublicBuildID, v.ObservedAt = &observed.BuildID, &observed.ObservedAt
		if v.InstalledBuildID != nil {
			available := installed != observed.BuildID
			v.UpdateAvailable = &available
		}
	}
	JSON(w, r, http.StatusOK, v)
}

func knownBuildID(id string) bool {
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n > 0
}

// updateCheckCancelPolicy: an update check queries Steam and writes what it saw. There is no
// half of that worth protecting, so it is cancellable throughout (12 §8).
func updateCheckCancelPolicy(string) (cancellable bool, phase string) { return true, "" }

func (h *Instances) submitUpdateCheck(ctx context.Context, scheduleID string) (*store.Job, error) {
	j, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindUpdateCheck, LockKey: jobs.GlobalLockKey(jobs.KindUpdateCheck),
		Payload: struct{}{}, ScheduleID: scheduleID,
	}, h.runUpdateCheck)
	if err != nil {
		return nil, fmt.Errorf("submit update check: %w", err)
	}
	return j, nil
}

func (h *Instances) runUpdateCheck(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		if ctx.Err() != nil || jh.CancelRequested(ctx) {
			return jobs.Outcome{Status: "cancelled"}
		}
		jh.Progress(ctx, (attempt-1)*30, fmt.Sprintf("Checking Steam public build (attempt %d of 3)", attempt))
		queryCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		id, err := instance.QueryPublicBuild(queryCtx, h.Runtime, h.Cfg.Game.SteamCMDImage)
		cancel()
		if ctx.Err() != nil || jh.CancelRequested(ctx) {
			return jobs.Outcome{Status: "cancelled"}
		}
		if err == nil {
			observed := publicBuild{BuildID: id, ObservedAt: time.Now().UTC()}
			jh.Progress(ctx, 100, "Steam public build is "+id)
			return jobs.Outcome{Status: "succeeded", OnFinish: func(ctx context.Context, tx *sql.Tx) error {
				return store.TxKVSet(ctx, tx, publicBuildKey, observed)
			}}
		}
		last = err
		jh.Log(err.Error())
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return jobs.Outcome{Status: "cancelled"}
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	return jobs.Outcome{
		Status:    "failed",
		ErrorCode: apierr.Unavailable.String(),
		Error:     "Steam build check failed: " + last.Error(),
	}
}
