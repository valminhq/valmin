package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

// scheduleKind is one kind a schedule may enqueue, with the action that authorizes writing
// such a schedule. There is no `schedules.instance` capability, and minting one would say that
// scheduling a backup is a different authority from taking one (ADR-131).
type scheduleKind struct {
	kind   jobs.Kind
	action authz.Action
	// global is a kind that takes no instance: its schedule carries instance_id NULL.
	global bool
}

// scheduleKinds is the closed set. A kind lands here when its runner exists, not before: an
// unknown kind on a schedule row is a job nothing can execute, which is the shape 12 §3.1's
// typed constants exist to make impossible.
var scheduleKinds = map[string]scheduleKind{
	jobs.KindUpdateCheck.String(): {kind: jobs.KindUpdateCheck, action: authz.SchedulesGlobal, global: true},
	jobs.KindBackup.String():      {kind: jobs.KindBackup, action: authz.BackupsCreate},
	jobs.KindRestart.String():     {kind: jobs.KindRestart, action: authz.InstanceRestart},
	jobs.KindGameUpdate.String():  {kind: jobs.KindGameUpdate, action: authz.InstanceUpdate},
	jobs.KindPrune.String():       {kind: jobs.KindPrune, action: authz.SchedulesGlobal, global: true},
}

// Schedules serves /schedules and is the clock's enqueuer: internal/scheduler decides what is
// due, this decides what that means.
type Schedules struct {
	DB        *store.DB
	Authz     *authz.Authz
	Instances *Instances
}

func (s *Schedules) Routes(rt *Router) {
	rt.Handle("GET /api/v1/schedules", http.HandlerFunc(s.list))
	rt.Handle("POST /api/v1/schedules", http.HandlerFunc(s.create))
	rt.Handle("PATCH /api/v1/schedules/{id}", http.HandlerFunc(s.patch))
	rt.Handle("DELETE /api/v1/schedules/{id}", http.HandlerFunc(s.delete))
}

// scheduleView is one schedule on the wire.
type scheduleView struct {
	ID         string     `json:"id"`
	InstanceID *string    `json:"instance_id"`
	Kind       string     `json:"kind"`
	Cron       string     `json:"cron"`
	Enabled    bool       `json:"enabled"`
	LastRunAt  *time.Time `json:"last_run_at"`
	NextRunAt  *time.Time `json:"next_run_at"`
	// CreatedBy and CreatedByUsername name whoever set this up, for an operator reading the
	// list. Both are null once that account is gone, and neither is consulted when the
	// schedule fires (ADR-134).
	CreatedBy         *string `json:"created_by"`
	CreatedByUsername *string `json:"created_by_username"`
	// Timezone is the location the expression is evaluated in, sent rather than left to be
	// inferred: an operator who reads "03:00" and thinks in local time is the complaint this
	// field exists to prevent.
	Timezone string `json:"timezone"`
}

// toScheduleView renders one schedule. usernames maps user ids to names; a nil map, or an id
// missing from it, leaves the name null rather than inventing one.
func toScheduleView(s *store.Schedule, usernames map[string]string) scheduleView {
	v := scheduleView{
		ID: s.ID, InstanceID: s.InstanceID, Kind: s.Kind, Cron: s.Cron, Enabled: s.Enabled,
		LastRunAt: s.LastRunAt, NextRunAt: s.NextRunAt, CreatedBy: s.CreatedBy,
		Timezone: scheduleTimezone,
	}
	if s.CreatedBy != nil {
		if name, ok := usernames[*s.CreatedBy]; ok {
			v.CreatedByUsername = &name
		}
	}
	return v
}

// authorNames maps user ids to usernames for a listing. One query for the whole page rather
// than one per row: this panel has a handful of users and a handful of schedules.
func (s *Schedules) authorNames(ctx context.Context) map[string]string {
	users, err := s.DB.ListUsers(ctx)
	if err != nil {
		// A name is decoration on an audit field. Losing it must not cost the listing.
		slog.WarnContext(ctx, "schedule authors could not be named", slog.Any("error", err))
		return nil
	}
	names := make(map[string]string, len(users))
	for i := range users {
		names[users[i].ID] = users[i].Username
	}
	return names
}

// scheduleTickInterval is how often the clock asks what is due. A minute is the resolution a
// five-field cron expression can name, so asking more often would find the same answer.
const scheduleTickInterval = time.Minute

// scheduleTimezone is the location every expression is evaluated in. The daemon runs UTC
// (06 §4) and a schedule carries no location of its own, so this is a statement rather than a
// setting.
const scheduleTimezone = "UTC"

// list is GET /schedules, filtered to what the caller can see: global rows need
// schedules.global, an instance's rows need to be able to see the instance.
func (s *Schedules) list(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	rows, err := s.DB.ListSchedules(r.Context())
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}

	// A schedule is visible to whoever can see what it acts on: the panel for a global kind,
	// the instance for the rest. Filtered here rather than by a Can() over the collection,
	// the same precedent as instances.go:list.
	usernames := s.authorNames(r.Context())
	views := make([]scheduleView, 0, len(rows))
	for i := range rows {
		sc := &rows[i]
		visible := s.Authz.Can(r.Context(), u, authz.SchedulesGlobal, "")
		if sc.InstanceID != nil {
			visible = s.Authz.Can(r.Context(), u, authz.InstanceView, *sc.InstanceID)
		}
		if visible {
			views = append(views, toScheduleView(sc, usernames))
		}
	}
	JSON(w, r, http.StatusOK, NewPage(views, nil))
}

// scheduleRequest is the create and update body. Kind and instance_id are read only on create:
// changing either makes it a different schedule, and the job rows already pointing at this one
// would then describe something it never was.
type scheduleRequest struct {
	InstanceID *string          `json:"instance_id"`
	Kind       string           `json:"kind"`
	Cron       string           `json:"cron"`
	Payload    *json.RawMessage `json:"payload"`
	Enabled    *bool            `json:"enabled"`
}

func (s *Schedules) create(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	var body scheduleRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}

	spec, ok := scheduleKinds[strings.TrimSpace(body.Kind)]
	if !ok {
		writeFieldError(w, r, "kind", apierr.FieldInvalid,
			"This is not a kind the panel can put on a schedule.")
		return
	}
	scope := deref(body.InstanceID)
	if !spec.global && scope == "" {
		writeFieldError(w, r, "instance_id", apierr.FieldRequired,
			"This kind runs against one instance, so it needs one.")
		return
	}
	if spec.global {
		// A schedule nobody may see is 404, not 403 (ADR-038); schedules.global is never
		// grantable (09 §3.3), so this is the admin check.
		if !s.Authz.Can(r.Context(), u, authz.SchedulesGlobal, "") {
			apierr.Write(w, r, apierr.New(apierr.NotFound))
			return
		}
	} else {
		if !s.Authz.Can(r.Context(), u, authz.InstanceView, scope) {
			apierr.Write(w, r, apierr.New(apierr.NotFound))
			return
		}
		if !s.Authz.Can(r.Context(), u, spec.action, scope) {
			apierr.Write(w, r, apierr.New(apierr.Forbidden))
			return
		}
	}

	next, ok := parseCron(body.Cron)
	if !ok {
		writeFieldError(w, r, "cron", apierr.FieldInvalid, cronHelp(body.Cron))
		return
	}

	row := &store.Schedule{
		ID: store.NewID(), Kind: spec.kind.String(), Cron: strings.TrimSpace(body.Cron),
		Payload: payloadOf(body.Payload), Enabled: body.Enabled == nil || *body.Enabled,
		NextRunAt: &next, CreatedBy: &u.ID,
	}
	if !spec.global {
		row.InstanceID = body.InstanceID
	}
	if err := s.DB.CreateSchedule(r.Context(), row); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusCreated, toScheduleView(row, map[string]string{u.ID: u.Username}))
}

func (s *Schedules) patch(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	row, spec, ok := s.mustLoadSchedule(w, r)
	if !ok {
		return
	}
	if spec.global {
		if !s.Authz.Can(r.Context(), u, authz.SchedulesGlobal, "") {
			apierr.Write(w, r, apierr.New(apierr.NotFound))
			return
		}
	} else {
		if !s.Authz.Can(r.Context(), u, authz.InstanceView, deref(row.InstanceID)) {
			apierr.Write(w, r, apierr.New(apierr.NotFound))
			return
		}
		if !s.Authz.Can(r.Context(), u, spec.action, deref(row.InstanceID)) {
			apierr.Write(w, r, apierr.New(apierr.Forbidden))
			return
		}
	}
	var body scheduleRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}

	if strings.TrimSpace(body.Cron) != "" {
		next, ok := parseCron(body.Cron)
		if !ok {
			writeFieldError(w, r, "cron", apierr.FieldInvalid, cronHelp(body.Cron))
			return
		}
		row.Cron, row.NextRunAt = strings.TrimSpace(body.Cron), &next
	}
	if body.Payload != nil {
		row.Payload = payloadOf(body.Payload)
	}
	if body.Enabled != nil {
		row.Enabled = *body.Enabled
	}
	if err := s.DB.UpdateSchedule(r.Context(), row); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, toScheduleView(row, s.authorNames(r.Context())))
}

func (s *Schedules) delete(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	row, spec, ok := s.mustLoadSchedule(w, r)
	if !ok {
		return
	}
	if spec.global {
		if !s.Authz.Can(r.Context(), u, authz.SchedulesGlobal, "") {
			apierr.Write(w, r, apierr.New(apierr.NotFound))
			return
		}
	} else {
		if !s.Authz.Can(r.Context(), u, authz.InstanceView, deref(row.InstanceID)) {
			apierr.Write(w, r, apierr.New(apierr.NotFound))
			return
		}
		if !s.Authz.Can(r.Context(), u, spec.action, deref(row.InstanceID)) {
			apierr.Write(w, r, apierr.New(apierr.Forbidden))
			return
		}
	}
	if err := s.DB.DeleteSchedule(r.Context(), row.ID); err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mustLoadSchedule resolves {id} to a schedule the caller can see. One they cannot see is 404,
// the same as one that never existed (ADR-038).
func (s *Schedules) mustLoadSchedule(
	w http.ResponseWriter, r *http.Request,
) (*store.Schedule, scheduleKind, bool) {
	row, err := s.DB.ScheduleByID(r.Context(), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return nil, scheduleKind{}, false
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(apierr.NotFound))
		return nil, scheduleKind{}, false
	}
	spec, ok := scheduleKinds[row.Kind]
	if !ok {
		apierr.Write(w, r, apierr.New(apierr.Internal).
			Wrap(fmt.Errorf("schedule %s names kind %q, which this build cannot run", row.ID, row.Kind)))
		return nil, scheduleKind{}, false
	}
	return row, spec, true
}

// parseCron refuses an expression at the moment it is written rather than at tick time, and
// returns the first time it fires. An expression discovered to be unreadable at 03:00 is a
// schedule that silently never ran.
func parseCron(expr string) (time.Time, bool) {
	next, err := scheduler.Next(strings.TrimSpace(expr), time.Now().UTC())
	return next, err == nil
}

func cronHelp(expr string) string {
	return fmt.Sprintf(
		"%q is not a schedule the panel can read: five fields (minute hour day month weekday), "+
			"or one of @daily, @hourly, @weekly, @monthly.", strings.TrimSpace(expr))
}

// writeFieldError answers 11 §2.4's 422 for a single field, which is what most of these bodies
// fail on.
func writeFieldError(
	w http.ResponseWriter, r *http.Request, field string, code apierr.FieldCode, message string,
) {
	var val apierr.Validation
	val.Add(field, code, message)
	apierr.Write(w, r, val.Err())
}

func payloadOf(raw *json.RawMessage) string {
	if raw == nil || len(*raw) == 0 {
		return "{}"
	}
	return string(*raw)
}

// Enqueue is internal/scheduler's Enqueuer: it turns one due schedule into one submitted job,
// or into a recorded skip. It never runs anything itself (12 §11).
func (s *Schedules) Enqueue(ctx context.Context, sc *store.Schedule) error {
	spec, ok := scheduleKinds[sc.Kind]
	if !ok {
		return fmt.Errorf("schedule %s names kind %q, which this build cannot run", sc.ID, sc.Kind)
	}
	if spec.global {
		return s.enqueueGlobal(ctx, sc, spec)
	}
	return s.enqueueForInstance(ctx, sc, spec)
}

func (s *Schedules) enqueueGlobal(ctx context.Context, sc *store.Schedule, spec scheduleKind) error {
	var err error
	switch spec.kind {
	case jobs.KindPrune:
		_, err = s.Instances.Engine.Submit(ctx, pruneSpec(sc.ID), s.Instances.runPrune)
	case jobs.KindUpdateCheck:
		_, err = s.Instances.submitUpdateCheck(ctx, sc.ID)
	default:
		return fmt.Errorf("no runner for global kind %s", spec.kind)
	}
	if err == nil {
		return nil
	}
	return s.recordSkip(ctx, sc, spec, nil, err)
}

// enqueueForInstance submits the instance-scoped kind a schedule names, or records why it could
// not. Neither a held lock nor a state the kind cannot be claimed from is a failure of the
// clock: both are ordinary and both must leave a trace (ADR-030).
func (s *Schedules) enqueueForInstance(ctx context.Context, sc *store.Schedule, spec scheduleKind) error {
	if sc.InstanceID == nil {
		return fmt.Errorf("schedule %s of kind %s names no instance", sc.ID, sc.Kind)
	}
	inst, err := s.DB.InstanceByID(ctx, *sc.InstanceID)
	if err != nil {
		return fmt.Errorf("read instance %s: %w", *sc.InstanceID, err)
	}
	if inst == nil {
		// The row's foreign key cascades, so this is a race with a delete, not a leak.
		return nil
	}

	if !claimableFrom(spec.kind, instance.State(inst.State)) {
		return s.recordSkip(ctx, sc, spec, inst, fmt.Errorf(
			"the instance was %s, which %s cannot run from", inst.State, spec.kind))
	}
	containerID := ""
	if inst.ContainerID != nil {
		containerID = *inst.ContainerID
	}

	switch spec.kind {
	case jobs.KindBackup:
		_, err = s.Instances.submitBackup(ctx, inst, containerID, modeQuiesced, "", sc.ID)
	case jobs.KindRestart:
		if containerID == "" {
			return s.recordSkip(ctx, sc, spec, inst, errors.New("the instance has no container"))
		}
		_, err = s.Instances.submitRestart(ctx, inst, containerID, "", sc.ID)
	case jobs.KindGameUpdate:
		// A schedule is standing permission, never standing confirmation: 03 §8 wants a person
		// to answer for a modded server every time, so a tick skips one and says why
		// (ADR-137). submitGameUpdate refuses it, and the skip is the record.
		_, err = s.Instances.submitGameUpdate(ctx, inst, false, "", sc.ID)
	default:
		return fmt.Errorf("no runner for instance kind %s", spec.kind)
	}
	if err != nil {
		return s.recordSkip(ctx, sc, spec, inst, err)
	}
	return nil
}

// claimableFrom is checkInstanceState's question without an http.ResponseWriter: a tick has
// nobody to answer 409 to.
func claimableFrom(kind jobs.Kind, state instance.State) bool {
	for _, allowed := range instance.AllowedFrom(kind) {
		if state == allowed {
			return true
		}
	}
	return false
}

// recordSkip writes the terminal job row that makes a tick which enqueued nothing visible in
// the instance's job history (ADR-030, 12 §11). A skip is not an error the clock reports: it is
// the normal outcome of a schedule that came round while something else held the lock.
func (s *Schedules) recordSkip(
	ctx context.Context, sc *store.Schedule, spec scheduleKind, inst *store.Instance, cause error,
) error {
	code := apierr.Internal.String()
	var conflict *store.JobConflict
	if errors.As(cause, &conflict) {
		code = apierr.JobInProgress.String()
	}

	row := &store.Job{
		ID: store.NewID(), Kind: spec.kind.String(), ScheduleID: &sc.ID,
		LockKey: jobs.GlobalLockKey(spec.kind),
	}
	if inst != nil {
		row.InstanceID = &inst.ID
		row.InstanceName = inst.Name
		row.LockKey = jobs.InstanceLockKey(inst.ID)
	}
	if err := s.DB.RecordSkippedRun(ctx, row, code, "This scheduled run was skipped: "+cause.Error()); err != nil {
		return fmt.Errorf("record skipped run of schedule %s: %w", sc.ID, err)
	}
	slog.InfoContext(ctx, "scheduled run skipped",
		slog.String("schedule_id", sc.ID), slog.String("kind", spec.kind.String()),
		slog.String("reason", cause.Error()))
	return nil
}
