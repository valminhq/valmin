package control

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

// UpdateChecker observes the public build and publishes successful observations.
type UpdateChecker struct {
	DB       *store.DB
	Engine   *jobs.Engine
	Runtime  runtime.Runtime
	Config   *config.Config
	Notifier Notifier
}

// Submit enqueues a global build check using the persisted empty payload.
func (h *UpdateChecker) Submit(ctx context.Context, scheduleID string) (*store.Job, error) {
	j, err := h.Engine.Submit(ctx, &jobs.Spec{
		Kind: jobs.KindUpdateCheck, LockKey: jobs.GlobalLockKey(jobs.KindUpdateCheck),
		Payload: struct{}{}, ScheduleID: scheduleID,
	}, h.Run)
	if err != nil {
		return nil, fmt.Errorf("submit update check: %w", err)
	}
	return j, nil
}

// observed is the successful outcome: the observation is published, and a build the panel has
// not seen before owes a notification, both in the same finish transaction.
func (h *UpdateChecker) observed(ctx context.Context, id string) jobs.Outcome {
	build := instance.PublicBuild{BuildID: id, ObservedAt: time.Now().UTC()}
	// Read before the write, in the work phase: the comparison is what makes an unchanged
	// hourly observation say nothing (05 M6).
	notifyNewBuild := h.newBuildNotification(ctx, id)
	return jobs.Outcome{Status: jobs.StatusSucceeded, OnFinish: func(ctx context.Context, tx *sql.Tx) error {
		if err := store.TxKVSet(ctx, tx, instance.PublicBuildKey, build); err != nil {
			return fmt.Errorf("publish the observed build: %w", err)
		}
		if notifyNewBuild == nil {
			return nil
		}
		return notifyNewBuild(ctx, tx)
	}}
}

func (h *UpdateChecker) Run(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		if ctx.Err() != nil || jh.CancelRequested(ctx) {
			return jobs.Outcome{Status: jobs.StatusCancelled}
		}
		jh.Progress(ctx, (attempt-1)*30, fmt.Sprintf("Checking Steam public build (attempt %d of 3)", attempt))
		queryCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		id, err := instance.QueryPublicBuild(queryCtx, h.Runtime, h.Config.Game.SteamCMDImage, h.Config.Data.HostRoot)
		cancel()
		if ctx.Err() != nil || jh.CancelRequested(ctx) {
			return jobs.Outcome{Status: jobs.StatusCancelled}
		}
		if err == nil {
			jh.Progress(ctx, 100, "Steam public build is "+id)
			return h.observed(ctx, id)
		}
		last = err
		jh.Log(err.Error())
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return jobs.Outcome{Status: jobs.StatusCancelled}
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	return jobs.Outcome{
		Status:    jobs.StatusFailed,
		ErrorCode: errcode.Unavailable.String(),
		Error:     "Steam build check failed: " + last.Error(),
	}
}

// newBuildNotification is the finish step that announces observed as a new public build, or nil
// when it is not new or nothing listens.
func (h *UpdateChecker) newBuildNotification(
	ctx context.Context, observed string,
) func(context.Context, *sql.Tx) error {
	if h.Notifier == nil {
		return nil
	}
	var previous instance.PublicBuild
	if _, err := h.DB.KVGet(ctx, instance.PublicBuildKey, &previous); err != nil {
		slog.WarnContext(ctx, "read the last observed build", slog.Any("error", err))
		return nil
	}
	return h.Notifier.NotifyPublicBuild(ctx, previous.BuildID, observed)
}
