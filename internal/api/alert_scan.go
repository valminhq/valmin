package api

import (
	"context"
	"fmt"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

func (h *Instances) scanAlerts(ctx context.Context) (store.ConditionDiff, error) {
	diff, err := h.alerts.Scan(ctx)
	if err != nil {
		return store.ConditionDiff{}, fmt.Errorf("scan alerts: %w", err)
	}
	return diff, nil
}

// reportedAlarmFloor returns the alarm floor for a scan: the highest floor any running server
// reports, or the configured one. Highest, because a panel alarming below the point a server
// stopped saving is worse than none.
func reportedAlarmFloor(streams *instance.Streams, configured int64) func([]store.Instance) uint64 {
	return func(instances []store.Instance) uint64 {
		var highest *instance.DiskThresholds
		for i := range instances {
			reader := streams.Reader(instances[i].ID)
			if reader == nil {
				continue
			}
			reported := reader.Disk()
			if reported == nil {
				continue
			}
			if highest == nil || reported.BlockedBelowBytes > highest.BlockedBelowBytes {
				highest = reported
			}
		}
		return alarmFloor(configured, highest)
	}
}
