package api

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"slices"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/store"
)

// Jobs serves the two kind-agnostic endpoints of 12 §7 and §8. Every endpoint that
// *creates* a job (POST /instances/{id}/start, and the rest as their own work packages
// land) owns its own resource and calls Engine.Submit directly; this file only reads and
// cancels the row generically.
type Jobs struct {
	Engine *jobs.Engine
	Authz  *authz.Authz
	DB     *store.DB
}

// Routes registers the job endpoints behind the middleware chain.
func jobRoutes(rt *routeTable, j *Jobs) {
	rt.Handle("GET /api/v1/jobs/{id}", http.HandlerFunc(j.get))
	rt.Handle("POST /api/v1/jobs/{id}/cancel", http.HandlerFunc(j.cancel))
}

// jobView is the wire shape of a job row (04 §3's stub, grown into the full resource
// GET /jobs/{id} returns).
type jobView struct {
	JobID      string  `json:"job_id"`
	Kind       string  `json:"kind"`
	Status     string  `json:"status"`
	InstanceID *string `json:"instance_id,omitempty"`
	Progress   int     `json:"progress"`
	Message    *string `json:"message,omitempty"`
	ErrorCode  *string `json:"error_code,omitempty"`
	Error      *string `json:"error,omitempty"`
	Clean      *bool   `json:"clean,omitempty"`
	CreatedAt  string  `json:"created_at"`
	StartedAt  *string `json:"started_at,omitempty"`
	FinishedAt *string `json:"finished_at,omitempty"`
	// RequestedByName is the username of the person who started the job. It is left out for
	// system work and for a user who has since been deleted.
	RequestedByName *string `json:"requested_by_name,omitempty"`
	// Scheduled is true when a schedule started the job.
	Scheduled bool `json:"scheduled"`
	// Changes lists the packages a mod job was asked to change.
	Changes []jobChange `json:"changes,omitempty"`
}

// jobChange is one package a mod job installs, updates, removes, enables or disables. The
// versions are set only where the job's stored arguments carry them.
type jobChange struct {
	Action      string `json:"action"`
	FullName    string `json:"full_name"`
	FromVersion string `json:"from_version,omitempty"`
	ToVersion   string `json:"to_version,omitempty"`
}

// jobChanges reads the packages a mod job was asked to change from its payload. Any other
// kind, or a payload that does not decode, has none.
func jobChanges(j *store.Job) []jobChange {
	switch j.Kind {
	case jobs.KindModInstall.String():
		return installChanges(j.Payload)
	case jobs.KindModUninstall.String():
		var p manager.UninstallPayload
		if json.Unmarshal([]byte(j.Payload), &p) != nil {
			return nil
		}
		out := make([]jobChange, len(p.FullNames))
		for i, name := range p.FullNames {
			out[i] = jobChange{Action: "uninstall", FullName: name}
		}
		return out
	case jobs.KindModToggle.String():
		var p manager.TogglePayload
		if json.Unmarshal([]byte(j.Payload), &p) != nil || p.FullName == "" {
			return nil
		}
		action := "disable"
		if p.Enable {
			action = "enable"
		}
		return []jobChange{{Action: action, FullName: p.FullName}}
	}
	return nil
}

// installChanges is the changes of a mod_install payload: every package of an update-all, or the
// one package of a single install.
func installChanges(payload string) []jobChange {
	var p manager.InstallPayload
	if json.Unmarshal([]byte(payload), &p) != nil {
		return nil
	}
	if len(p.Updates) > 0 {
		out := make([]jobChange, len(p.Updates))
		for i, u := range p.Updates {
			out[i] = jobChange{Action: "update", FullName: u.FullName, FromVersion: u.FromVersion, ToVersion: u.Version}
		}
		return out
	}
	if p.FullName == "" {
		return nil
	}
	change := jobChange{Action: "install", FullName: p.FullName}
	// A minimum is a floor: the version installed may be higher than the one recorded.
	if !p.Minimum {
		change.ToVersion = p.Version
	}
	return []jobChange{change}
}

func toJobView(j *store.Job) jobView {
	v := jobView{
		JobID: j.ID, Kind: j.Kind, Status: j.Status, InstanceID: j.InstanceID,
		Progress: j.Progress, Message: j.Message, ErrorCode: j.ErrorCode, Error: j.Error, Clean: j.Clean,
		CreatedAt: j.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		Scheduled: j.Scheduled, Changes: jobChanges(j),
	}
	if j.StartedAt != nil {
		s := j.StartedAt.UTC().Format("2006-01-02T15:04:05Z")
		v.StartedAt = &s
	}
	if j.FinishedAt != nil {
		s := j.FinishedAt.UTC().Format("2006-01-02T15:04:05Z")
		v.FinishedAt = &s
	}
	return v
}

// jobViewsNamed renders rows with the usernames of the people who started them, resolved in
// one query for the whole set.
func jobViewsNamed(ctx context.Context, db *store.DB, rows []store.Job) ([]jobView, error) {
	var ids []string
	for i := range rows {
		if id := rows[i].RequestedBy; id != nil && !slices.Contains(ids, *id) {
			ids = append(ids, *id)
		}
	}
	names, err := db.UsernamesByID(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("name job requesters: %w", err)
	}
	views := make([]jobView, len(rows))
	for i := range rows {
		views[i] = toJobView(&rows[i])
		if id := rows[i].RequestedBy; id != nil {
			if name, ok := names[*id]; ok {
				views[i].RequestedByName = &name
			}
		}
	}
	return views, nil
}

// jobInstanceID is 09 §4.1's job-topic rule read back out of the row: a global job
// (instance_id NULL) resolves to "", which Can already treats as never satisfiable by a
// member, so it is admin-only for free rather than by a special case here.
func jobInstanceID(job *store.Job) string {
	if job.InstanceID != nil {
		return *job.InstanceID
	}
	return ""
}

// cancelActions maps each job kind to the action that authorizes cancelling it: the same
// action that initiates the kind (12 §8), so cancellation is never a lesser-privileged
// path than starting the same work. A kind absent here — including one this build does not
// recognise — denies cancellation rather than inheriting viewer access.
var cancelActions = map[jobs.Kind]authz.Action{
	jobs.KindProvision:        authz.InstanceCreate,
	jobs.KindStart:            authz.InstanceStart,
	jobs.KindStop:             authz.InstanceStop,
	jobs.KindRestart:          authz.InstanceRestart,
	jobs.KindDelete:           authz.InstanceDelete,
	jobs.KindWorldImport:      authz.WorldImport,
	jobs.KindWorldDelete:      authz.WorldImport,
	jobs.KindThunderstoreSync: authz.PanelSettings,
	jobs.KindModInstall:       authz.ModsManage,
	jobs.KindModUninstall:     authz.ModsManage,
	jobs.KindModToggle:        authz.ModsManage,
	jobs.KindBackup:           authz.BackupsCreate,
	jobs.KindRemoteCopy:       authz.BackupsCreate,
	jobs.KindRemoteTest:       authz.PanelSettings,
	jobs.KindRemotePrune:      authz.PanelSettings,
	jobs.KindRestore:          authz.BackupsRestore,
	jobs.KindPrune:            authz.SchedulesGlobal,
	jobs.KindDiagnose:         authz.PanelSettings,
	jobs.KindUpdateCheck:      authz.SchedulesGlobal,
	jobs.KindGameUpdate:       authz.InstanceUpdate,
	jobs.KindClone:            authz.InstanceClone,
	jobs.KindConfigApply:      authz.InstanceCreate,
	jobs.KindKeyRotate:        authz.PanelSettings,
	jobs.KindWebhookDeliver:   authz.PanelSettings,
	jobs.KindAdopt:            authz.InstanceAdopt,
	jobs.KindAlertScan:        authz.SchedulesGlobal,
}

// canCancel reports whether u may cancel job, checking the action cancelActions requires for
// its kind. An unmapped or unrecognised kind is not cancellable by anyone but the fix that adds
// its row here (12 §8).
func (j *Jobs) canCancel(ctx context.Context, u *store.User, job *store.Job) bool {
	kind, ok := jobs.ByName(job.Kind)
	if !ok {
		return false
	}
	act, ok := cancelActions[kind]
	if !ok {
		return false
	}
	return j.Authz.Can(ctx, u, act, jobInstanceID(job))
}

// get is GET /jobs/{id} (12 §7, 11 §3): 200 even when status=failed — the read succeeded,
// the work is what failed.
func (j *Jobs) get(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	job, err := j.Engine.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	// A resource the caller cannot see does not exist (D2, ADR-038) — the same envelope
	// for "no such job" and "not yours to see", or the endpoint is an existence oracle.
	if job == nil || !j.Authz.Can(r.Context(), u, authz.InstanceView, jobInstanceID(job)) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	views, err := jobViewsNamed(r.Context(), j.DB, []store.Job{*job})
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, views[0])
}

// cancel is POST /jobs/{id}/cancel (12 §8): cooperative, and 409 job_not_cancellable past
// the kind's declared point of no return, with the phase named.
func (j *Jobs) cancel(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	job, err := j.Engine.Get(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	if job == nil || !j.Authz.Can(r.Context(), u, authz.InstanceView, jobInstanceID(job)) {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return
	}
	if !j.canCancel(r.Context(), u, job) {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}

	if err := j.Engine.Cancel(r.Context(), id); err != nil {
		var notCancellable *jobs.ErrNotCancellable
		switch {
		case stderrors.Is(err, jobs.ErrJobNotFound):
			apierr.Write(w, r, apierr.New(apierr.NotFound))
		case stderrors.Is(err, jobs.ErrJobTerminal):
			apierr.Write(w, r, apierr.New(apierr.JobNotCancellable).Msg("This job has already finished."))
		case stderrors.As(err, &notCancellable):
			apierr.Write(w, r, apierr.New(apierr.JobNotCancellable).With("phase", notCancellable.Phase))
		default:
			apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		}
		return
	}
	if err := j.DB.WriteAuditLog(r.Context(), &store.AuditEntry{
		UserID: u.ID, InstanceID: jobInstanceID(job), Action: "jobs.cancel",
		Detail: detailJSON(map[string]string{"job_id": job.ID, "kind": job.Kind}),
		IP:     clientIP(r.Context()),
	}); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
