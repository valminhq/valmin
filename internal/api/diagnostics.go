package api

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/diag"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
	"github.com/valminhq/valmin/internal/version"
)

// kv keys holding the outcome of a container-based self-check, written by the startup
// gate and by the diagnose job.
const (
	kvHostRootCheck    = "diag_host_data_root"
	kvDataRootCheck    = "diag_data_root"
	kvGameNetworkCheck = "diag_game_network"
)

// deepCheckTimeout bounds the diagnose job. Each of its probes spawns a container.
const deepCheckTimeout = 5 * time.Minute

// Diagnostics serves the panel-wide health report and the support bundle. Both are
// read-only and admin-only (ADR-193).
type Diagnostics struct {
	// Instances carries the database, runtime, config and log readers the report reads.
	Instances *Instances
	// Packages probes the mod index. Nil leaves that check unknown.
	Packages diag.PackageIndex
	Hexium   diag.PackageIndex
	// StartedAt is when this daemon came up.
	StartedAt time.Time
}

func (d *Diagnostics) Routes(rt *Router) {
	rt.Handle("GET /api/v1/admin/diagnostics", http.HandlerFunc(d.report))
	rt.Handle("GET /api/v1/admin/diagnostics/bundle", http.HandlerFunc(d.bundle))
	rt.Handle("POST /api/v1/admin/diagnostics/run", http.HandlerFunc(d.runDeep))
}

// The diagnostics routes answer 403 rather than 404 for a caller without panel.settings:
// the never-grantable list is the case 11 §2.3 reserves 403 for, and no instance's
// existence is disclosed by the answer.
func (d *Diagnostics) report(w http.ResponseWriter, r *http.Request) {
	caller, ok := caller(w, r)
	if !ok {
		return
	}
	if !d.Instances.Authz.Can(r.Context(), caller, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	report, err := d.collect(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, report)
}

// bundle serves the report and the allowlisted settings as a zip. The archive is built in
// memory: it is a few kilobytes of JSON, and nothing about it belongs on disk.
func (d *Diagnostics) bundle(w http.ResponseWriter, r *http.Request) {
	caller, ok := caller(w, r)
	if !ok {
		return
	}
	if !d.Instances.Authz.Can(r.Context(), caller, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	report, err := d.collect(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	var archive bytes.Buffer
	if err := diag.WriteBundle(&archive, &report, diag.NewConfigView(d.Instances.Cfg)); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	name := diag.BundleName(report.GeneratedAt)
	d.audit(r, caller.ID, name)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeContent(w, r, name, report.GeneratedAt, bytes.NewReader(archive.Bytes()))
}

// runDeep submits the job that re-runs the container-based self-checks.
func (d *Diagnostics) runDeep(w http.ResponseWriter, r *http.Request) {
	caller, ok := caller(w, r)
	if !ok {
		return
	}
	if !d.Instances.Authz.Can(r.Context(), caller, authz.PanelSettings, "") {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	job, err := d.Instances.Engine.Submit(r.Context(), diagnoseSpec(), d.runDiagnose)
	if err != nil {
		var conflict *store.JobConflict
		if errors.As(err, &conflict) {
			apierr.Write(w, r, apierr.New(apierr.JobInProgress).With("job_id", conflict.JobID))
			return
		}
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// audit records who took a copy of the panel's state.
func (d *Diagnostics) audit(r *http.Request, userID, name string) {
	if err := d.Instances.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: userID, Action: authz.PanelSettings.String(),
		Detail: fmt.Sprintf(`{"operation":"support_bundle","file":%q}`, name),
		IP:     middleware.ClientIPFrom(r.Context()).String(),
	}); err != nil {
		slog.ErrorContext(r.Context(), "audit support bundle", slog.Any("error", err))
	}
}

// collect gathers the stored facts and runs the live probes. It opens no transaction and
// holds no lock (C1, C3).
func (d *Diagnostics) collect(ctx context.Context) (diag.Report, error) {
	h := d.Instances

	instances, err := h.DB.ListInstances(ctx, nil)
	if err != nil {
		return diag.Report{}, fmt.Errorf("read instances: %w", err)
	}
	free, err := instance.FreeSpace(h.Cfg.Data.Root)
	if err != nil {
		return diag.Report{}, fmt.Errorf("read free space: %w", err)
	}
	recorded, err := d.recordedChecks(ctx)
	if err != nil {
		return diag.Report{}, err
	}

	var (
		hexiumSynced time.Time
		etag         string
		syncedAt     time.Time
		build        publicBuild
		fsType       string
	)
	for _, read := range []struct {
		key string
		out any
	}{
		{kvETag(source.Thunderstore), &etag},
		{kvSyncedAt(source.Thunderstore), &syncedAt},
		{publicBuildKey, &build},
		{kvSyncedAt(source.Hexium), &hexiumSynced},
		{"data_fs_type", &fsType},
	} {
		if _, err := h.DB.KVGet(ctx, read.key, read.out); err != nil {
			return diag.Report{}, fmt.Errorf("read %s: %w", read.key, err)
		}
	}

	syncs := make(map[string]diag.RegistrySync)
	for _, src := range source.All() {
		var result diag.RegistrySync
		if found, err := h.DB.KVGet(ctx, kvSyncResult(src), &result); err != nil {
			return diag.Report{}, fmt.Errorf("read registry refresh: %w", err)
		} else if found {
			syncs["network."+src.String()] = result
		}
	}
	latest, err := h.DB.LatestTerminalJobPerInstanceKind(ctx)
	if err != nil {
		return diag.Report{}, fmt.Errorf("read latest jobs: %w", err)
	}
	report := diag.Collect(ctx, &diag.Input{
		Config:             h.Cfg,
		Runtime:            h.Runtime,
		Packages:           d.Packages,
		Hexium:             d.Hexium,
		HexiumSynced:       hexiumSynced,
		RegistrySyncs:      syncs,
		Now:                time.Now().UTC(),
		Build:              version.Current(),
		StartedAt:          d.StartedAt,
		UID:                os.Getuid(),
		GID:                os.Getgid(),
		FSType:             fsType,
		FreeBytes:          free,
		AlarmBytes:         h.reportedAlarmFloor(instances),
		Migrations:         migrationNames(),
		Recorded:           recorded,
		ThunderstoreETag:   etag,
		ThunderstoreSynced: syncedAt,
		SteamBuildID:       build.BuildID,
		SteamObservedAt:    build.ObservedAt,
		Instances:          d.summarise(ctx, instances),
	})
	report.FailedJobs = make([]diag.FailedJob, 0)
	for i := range latest {
		j := &latest[i]
		if j.Status != jobs.StatusFailed {
			continue
		}
		item := diag.FailedJob{ID: j.ID, Kind: j.Kind}
		if j.InstanceID != nil {
			item.InstanceID = *j.InstanceID
		}
		report.FailedJobs = append(report.FailedJobs, item)
	}
	return report, nil
}

// recordedChecks reads the outcomes the startup gate and the diagnose job stamped.
func (d *Diagnostics) recordedChecks(ctx context.Context) (map[string]diag.Observation, error) {
	out := make(map[string]diag.Observation, 3)
	for key, id := range map[string]string{
		kvHostRootCheck:    diag.CheckHostRoot,
		kvDataRootCheck:    diag.CheckDataRootWrite,
		kvGameNetworkCheck: diag.CheckGameNetwork,
	} {
		var obs diag.Observation
		found, err := d.Instances.DB.KVGet(ctx, key, &obs)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", key, err)
		}
		if found {
			out[id] = obs
		}
	}
	return out, nil
}

// summarise collects each server independently so a failed read remains visible.
func (d *Diagnostics) summarise(ctx context.Context, instances []store.Instance) []diag.Instance {
	h := d.Instances
	out := make([]diag.Instance, 0, len(instances))

	for i := range instances {
		inst := &instances[i]
		row := diag.Instance{
			ID: inst.ID, Name: inst.Name, State: inst.State, Image: h.Cfg.Game.Image,
			BasePort:        inst.BasePort,
			ExpectedPorts:   []int{inst.BasePort, inst.BasePort + 1},
			RestartRequired: inst.RestartRequired,
			Running:         inst.State == string(instance.StateRunning),
		}
		if mods, err := h.DB.InstanceMods(ctx, inst.ID); err == nil {
			row.Mods = len(mods)
		} else {
			row.ModsError = err.Error()
		}
		if reader := h.Streams.Reader(inst.ID); reader != nil {
			row.LogReaderAttached = true
			if disk := reader.Disk(); disk != nil {
				available := disk.AvailableBytes
				row.ServerFreeBytes = &available
			}
		}
		d.inspectContainer(ctx, inst.ContainerID, &row)
		out = append(out, row)
	}
	return out
}

func (d *Diagnostics) inspectContainer(ctx context.Context, id *string, row *diag.Instance) {
	if id == nil {
		if row.Running {
			row.InspectionError = "No container is recorded for this running server."
		}
		return
	}
	c, err := d.Instances.Runtime.Inspect(ctx, *id)
	if err != nil {
		row.InspectionError = err.Error()
		return
	}
	row.RestartCount = &c.RestartCount
	if !c.FinishedAt.IsZero() {
		row.ExitCode, row.OOMKilled, row.FinishedAt = &c.ExitCode, &c.OOMKilled, &c.FinishedAt
	}
	for _, p := range c.Spec.Ports {
		if p.Proto == "udp" {
			row.BoundPorts = append(row.BoundPorts, p.HostPort)
		}
	}
}

func diagnoseSpec() *jobs.Spec {
	return &jobs.Spec{
		Kind:    jobs.KindDiagnose,
		LockKey: jobs.GlobalLockKey(jobs.KindDiagnose),
		Payload: struct{}{},
	}
}

// runDiagnose re-runs the self-checks that need a container, which is why they are a job
// and not part of the report. The probes run in the work phase and their outcomes are
// written in the finish transaction (C1, C2).
func (d *Diagnostics) runDiagnose(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
	ctx, cancel := context.WithTimeout(ctx, deepCheckTimeout)
	defer cancel()

	h := d.Instances
	observed := map[string]diag.Observation{}
	record := func(key string, err error) {
		obs := diag.Observation{OK: err == nil, CheckedAt: time.Now().UTC()}
		if err != nil {
			obs.Detail = err.Error()
			jh.Log(err.Error())
		}
		observed[key] = obs
	}

	jh.Progress(ctx, 10, "Verifying host_data_root")
	record(kvHostRootCheck, config.VerifyHostRoot(ctx, h.Runtime, h.Cfg))

	jh.Progress(ctx, 35, "Verifying the data root")
	record(kvDataRootCheck, config.VerifyDataRoot(ctx, h.Cfg))

	jh.Progress(ctx, 55, "Verifying the game network")
	record(kvGameNetworkCheck, config.VerifyGameNetwork(ctx, h.Runtime, h.Cfg, command.DefaultRCONPort))

	jh.Progress(ctx, 75, "Asking Steam for the public build")
	steamBuild, steamErr := instance.QueryPublicBuild(ctx, h.Runtime, h.Cfg.Game.SteamCMDImage)
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
			return store.TxKVSet(ctx, tx, publicBuildKey,
				publicBuild{BuildID: steamBuild, ObservedAt: time.Now().UTC()})
		}
		return nil
	}}
}

// summariseDeep is the job's closing progress line: how many checks passed, out of how
// many that ran.
func summariseDeep(observed map[string]diag.Observation, steamErr error) string {
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
	obs := diag.Observation{OK: true, CheckedAt: at}
	for _, key := range []string{kvHostRootCheck, kvDataRootCheck, kvGameNetworkCheck} {
		if err := db.KVSet(ctx, key, obs); err != nil {
			return fmt.Errorf("record %s: %w", key, err)
		}
	}
	return nil
}

// migrationNames is the applied schema history, for a bundle that has to explain which
// database shape produced it.
func migrationNames() []string {
	applied, err := store.Migrations()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(applied))
	for _, m := range applied {
		out = append(out, strconv.Itoa(m.Version)+"_"+m.Name)
	}
	return out
}
