package api

import (
	"context"
	stderrors "errors"
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Jobs serves the two kind-agnostic endpoints of 12 §7 and §8. Every endpoint that
// *creates* a job (POST /instances/{id}/start, and the rest as their own work packages
// land) owns its own resource and calls Engine.Submit directly; this file only reads and
// cancels the row generically.
type Jobs struct {
	Engine *jobs.Engine
	Authz  *authz.Authz
}

// Routes registers the job endpoints behind the middleware chain.
func (j *Jobs) Routes(rt *Router) {
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
}

func toJobView(j *store.Job) jobView {
	v := jobView{
		JobID: j.ID, Kind: j.Kind, Status: j.Status, InstanceID: j.InstanceID,
		Progress: j.Progress, Message: j.Message, ErrorCode: j.ErrorCode, Error: j.Error, Clean: j.Clean,
		CreatedAt: j.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
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
	JSON(w, r, http.StatusOK, toJobView(job))
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
	w.WriteHeader(http.StatusNoContent)
}
