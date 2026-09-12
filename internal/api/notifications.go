package api

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

// The three events v1 emits (05 M6). Everything else the panel does is visible in the UI and
// does not wake anyone at 2 a.m.
//
// A notification never changes the outcome it reports: every failure on these paths is logged
// and swallowed, so a webhook nobody can reach cannot turn a successful backup into a failed
// one, or a failed one into a job that will not finish.

// OnJobFinished is the job engine's second finish hook. It owes a notification for a backup
// that failed, and writes the delivery intents in the same transaction that makes the job
// terminal, so a notification is owed exactly when the failure it reports commits.
//
// The destinations are read through the reader pool rather than the caller's transaction: the
// write is what has to be atomic with the job's outcome, not the lookup of who to tell.
func (h *Webhooks) OnJobFinished(ctx context.Context, tx *sql.Tx, fin *jobs.FinishedJob) error {
	if fin.Kind != jobs.KindBackup || fin.Status != jobs.StatusFailed {
		return nil
	}
	event := &notify.Event{
		ID:         store.NewID(),
		Kind:       notify.KindBackupFailed,
		OccurredAt: time.Now().UTC(),
		InstanceID: deref(fin.InstanceID),
		// Named from the job's own submission, so nothing here reads a row inside the
		// finish transaction.
		InstanceName: fin.InstanceName,
		Detail:       map[string]string{"Job": fin.ID},
	}
	deliveries, err := h.Prepare(ctx, event)
	if err != nil {
		slog.ErrorContext(ctx, "prepare backup-failed notification", slog.Any("error", err))
		return nil
	}
	if err := TxRecordDeliveries(ctx, tx, deliveries); err != nil {
		slog.ErrorContext(ctx, "record backup-failed notification", slog.Any("error", err))
	}
	return nil
}

// NotifyUnexpectedStop is what the observer calls when it finds a server that left `running`
// with no job holding its lock (C14). An operator's own stop holds that lock, so an expected
// stop never reaches here — the quiet is structural rather than a flag this has to read.
//
// The state write has already committed. A crash in the gap costs this one notification, which
// is the trade for keeping the observer's write path free of a notification's transaction.
func (h *Webhooks) NotifyUnexpectedStop(ctx context.Context, inst *store.Instance, to, reason string) {
	h.Emit(ctx, &notify.Event{
		ID:           store.NewID(),
		Kind:         notify.KindInstanceDown,
		OccurredAt:   time.Now().UTC(),
		InstanceID:   inst.ID,
		InstanceName: inst.Name,
		Detail:       map[string]string{"State": to, "Observed": reason},
	})
}

// NotifyPublicBuild owes a notification when the observed public build is one the panel has
// not seen before. An unchanged observation is the common case — the check runs hourly — and
// says nothing, so a receiver is told about a new build once rather than every hour until
// someone updates (05 M6).
func (h *Webhooks) NotifyPublicBuild(
	ctx context.Context, previous, observed string,
) func(context.Context, *sql.Tx) error {
	if observed == "" || observed == previous {
		return nil
	}
	event := &notify.Event{
		ID:         store.NewID(),
		Kind:       notify.KindUpdateAvailable,
		OccurredAt: time.Now().UTC(),
		Detail:     map[string]string{"Build": observed},
	}
	deliveries, err := h.Prepare(ctx, event)
	if err != nil {
		slog.ErrorContext(ctx, "prepare update-available notification", slog.Any("error", err))
		return nil
	}
	return func(ctx context.Context, tx *sql.Tx) error {
		if err := TxRecordDeliveries(ctx, tx, deliveries); err != nil {
			slog.ErrorContext(ctx, "record update-available notification", slog.Any("error", err))
		}
		return nil
	}
}
