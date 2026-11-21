package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// panelRetention is how long the delivery history and resolved alert conditions are kept.
const panelRetention = 30 * 24 * time.Hour

// Retention prunes terminal jobs, expired panel rows and old incidents, once at start and then
// daily, so a panel that stays up for months keeps its tables bounded.
type Retention struct {
	DB     *store.DB
	Engine *jobs.Engine
}

// Run sweeps until ctx is cancelled. A failed sweep is logged and retried the next day.
func (r *Retention) Run(ctx context.Context) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		r.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Retention) sweep(ctx context.Context) {
	if err := r.Engine.Sweep(ctx); err != nil {
		slog.WarnContext(ctx, "job retention sweep", slog.Any("error", err))
	}
	now := time.Now().UTC()
	if err := r.DB.SweepExpired(ctx, now, panelRetention); err != nil {
		slog.WarnContext(ctx, "panel retention sweep", slog.Any("error", err))
	}
	if _, err := r.DB.SweepIncidents(ctx, now.Add(-alerts.IncidentRetention)); err != nil {
		slog.WarnContext(ctx, "incident retention sweep", slog.Any("error", err))
	}
}
