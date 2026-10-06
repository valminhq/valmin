package api

import (
	"context"
	"fmt"

	"github.com/valminhq/valmin/internal/alerts/scan"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

func (h *Instances) alertScanner() *scan.Scanner {
	s := &scan.Scanner{DB: h.DB, Engine: h.Engine, DataRoot: h.Cfg.Data.Root, AlarmFloor: h.reportedAlarmFloor}
	if h.Notify != nil {
		s.Dispatcher = h.Notify
	}
	return s
}

func (h *Instances) scanAlerts(ctx context.Context) (store.ConditionDiff, error) {
	diff, err := h.alertScanner().Scan(ctx)
	if err != nil {
		return store.ConditionDiff{}, fmt.Errorf("scan alerts: %w", err)
	}
	return diff, nil
}

// reportedAlarmFloor is the highest floor any running server reports, or the configured one.
// Highest, because a panel alarming below the point a server stopped saving is worse than none.
func (h *Instances) reportedAlarmFloor(instances []store.Instance) uint64 {
	var highest *instance.DiskThresholds
	for i := range instances {
		reader := h.Streams.Reader(instances[i].ID)
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
	return alarmFloor(h.Cfg.Data.FreeSpaceFloorBytes, highest)
}
