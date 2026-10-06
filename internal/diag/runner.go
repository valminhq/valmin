package diag

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

const (
	HostRootKey      = "diag_host_data_root"
	DataRootKey      = "diag_data_root"
	GameNetworkKey   = "diag_game_network"
	PublicBuildKey   = "steam_public_build"
	DeepCheckTimeout = 5 * time.Minute
)

type PublicBuild struct {
	BuildID    string    `json:"build_id"`
	ObservedAt time.Time `json:"observed_at"`
}

// SubmitDeep queues the container-backed diagnostic checks.
func SubmitDeep(ctx context.Context, engine *jobs.Engine, cfg *config.Config, rt runtime.Runtime) (*store.Job, error) {
	return engine.Submit(ctx, &jobs.Spec{ //nolint:wrapcheck // preserve typed job conflicts
		Kind: jobs.KindDiagnose, LockKey: jobs.GlobalLockKey(jobs.KindDiagnose), Payload: struct{}{},
	}, func(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
		return RunDeep(ctx, jh, cfg, rt)
	})
}

// runDiagnose re-runs the self-checks that need a container, which is why they are a job
// and not part of the report. The probes run in the work phase and their outcomes are
// written in the finish transaction (C1, C2).
func RunDeep(ctx context.Context, jh *jobs.Handle, cfg *config.Config, rt runtime.Runtime) jobs.Outcome {
	ctx, cancel := context.WithTimeout(ctx, DeepCheckTimeout)
	defer cancel()

	observed := map[string]Observation{}
	record := func(key string, err error) {
		obs := Observation{OK: err == nil, CheckedAt: time.Now().UTC()}
		if err != nil {
			obs.Detail = err.Error()
			jh.Log(err.Error())
		}
		observed[key] = obs
	}

	jh.Progress(ctx, 10, "Verifying host_data_root")
	record(HostRootKey, config.VerifyHostRoot(ctx, rt, cfg))

	jh.Progress(ctx, 35, "Verifying the data root")
	record(DataRootKey, config.VerifyDataRoot(ctx, cfg))

	jh.Progress(ctx, 55, "Verifying the game network")
	record(GameNetworkKey, config.VerifyGameNetwork(ctx, rt, cfg, command.DefaultRCONPort))

	jh.Progress(ctx, 75, "Asking Steam for the public build")
	steamBuild, steamErr := instance.QueryPublicBuild(ctx, rt, cfg.Game.SteamCMDImage, cfg.Data.HostRoot)
	if steamErr != nil {
		jh.Log(steamErr.Error())
	}

	jh.Progress(ctx, 100, summariseDeep(observed, steamErr))
	return jobs.Outcome{Status: jobs.StatusSucceeded, OnFinish: func(ctx context.Context, tx *sql.Tx) error {
		for key, obs := range observed {
			if err := store.TxKVSet(ctx, tx, key, obs); err != nil {
				return fmt.Errorf("record %s: %w", key, err)
			}
		}
		if steamErr == nil {
			return store.TxKVSet(ctx, tx, PublicBuildKey,
				PublicBuild{BuildID: steamBuild, ObservedAt: time.Now().UTC()})
		}
		return nil
	}}
}

// summariseDeep is the job's closing progress line: how many checks passed, out of how
// many that ran.
func summariseDeep(observed map[string]Observation, steamErr error) string {
	passed, total := 0, len(observed)+1
	for _, obs := range observed {
		if obs.OK {
			passed++
		}
	}
	if steamErr == nil {
		passed++
	}
	return strconv.Itoa(passed) + " of " + strconv.Itoa(total) + " deep checks passed"
}

// RecordGateChecks stamps the startup gate's outcomes so the report can show them without
// spawning a container of its own.
func RecordGateChecks(ctx context.Context, db *store.DB, at time.Time) error {
	obs := Observation{OK: true, CheckedAt: at}
	for _, key := range []string{HostRootKey, DataRootKey, GameNetworkKey} {
		if err := db.KVSet(ctx, key, obs); err != nil {
			return fmt.Errorf("record %s: %w", key, err)
		}
	}
	return nil
}
