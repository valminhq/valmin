package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
	"github.com/valminhq/valmin/internal/ws"
)

// scheduleKind is one kind a schedule may enqueue, with the action that authorizes writing
// such a schedule. There is no `schedules.instance` capability, and minting one would say that
// scheduling a backup is a different authority from taking one (ADR-131).
type scheduleKind struct {
	kind   jobs.Kind
	action authz.Action
	// global is a kind that takes no instance: its schedule carries instance_id NULL.
	global bool
	// playerAware is a kind that stops a running server, so it may wait for players to leave.
	playerAware bool
	// chain is a kind made of steps. It needs action plus the action of each step, and is
	// player aware when any step stops the server, which forSteps settles.
	chain bool
}

// forSteps returns k as it applies to a chain with these steps.
func (k scheduleKind) forSteps(steps []control.ChainStep) scheduleKind {
	if k.chain {
		k.playerAware = control.StopsServer(steps)
	}
	return k
}

// actions lists what writing a schedule of kind k needs. A chain also needs the action of every
// step.
func (k scheduleKind) actions(steps []control.ChainStep) []authz.Action {
	out := []authz.Action{k.action}
	if !k.chain {
		return out
	}
	add := func(a authz.Action) {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	for _, st := range steps {
		switch st.Type {
		case control.StepBackup:
			add(authz.BackupsCreate)
		case control.StepStop:
			add(authz.InstanceStop)
		case control.StepStart:
			add(authz.InstanceStart)
		case control.StepRestart:
			add(authz.InstanceRestart)
		case control.StepGameUpdate:
			add(authz.InstanceUpdate)
		case control.StepCommand:
			add(authz.CommandsSend)
			if _, err := command.Validate(st.Command, command.ScopeOperator); err != nil {
				add(authz.CommandsUnrestricted)
			}
		}
	}
	return out
}

// scheduleKinds is the closed set. A kind lands here when its runner exists, not before: an
// unknown kind on a schedule row is a job nothing can execute, which is the shape 12 §3.1's
// typed constants exist to make impossible.
var scheduleKinds = map[string]scheduleKind{
	jobs.KindUpdateCheck.String(): {kind: jobs.KindUpdateCheck, action: authz.SchedulesGlobal, global: true},
	jobs.KindBackup.String():      {kind: jobs.KindBackup, action: authz.BackupsCreate, playerAware: true},
	jobs.KindRestart.String():     {kind: jobs.KindRestart, action: authz.InstanceRestart, playerAware: true},
	jobs.KindGameUpdate.String():  {kind: jobs.KindGameUpdate, action: authz.InstanceUpdate},
	jobs.KindPrune.String():       {kind: jobs.KindPrune, action: authz.SchedulesGlobal, global: true},
	jobs.KindAlertScan.String():   {kind: jobs.KindAlertScan, action: authz.SchedulesGlobal, global: true},
	jobs.KindChain.String():       {kind: jobs.KindChain, action: authz.InstanceStop, chain: true},
}

// scheduledSteps is what an instance schedule runs: the one step of a backup, restart or game
// update, or a chain's own steps.
func scheduledSteps(sc *store.Schedule) ([]control.ChainStep, error) {
	switch sc.Kind {
	case jobs.KindBackup.String():
		return []control.ChainStep{{Type: control.StepBackup}}, nil
	case jobs.KindRestart.String():
		return []control.ChainStep{{Type: control.StepRestart}}, nil
	case jobs.KindGameUpdate.String():
		return []control.ChainStep{{Type: control.StepGameUpdate}}, nil
	case jobs.KindChain.String():
		steps, err := control.ParseChainPayload(sc.Payload)
		if err != nil {
			return nil, fmt.Errorf("read the chain's steps: %w", err)
		}
		return steps, nil
	}
	return nil, fmt.Errorf("schedule %s of kind %s runs no steps", sc.ID, sc.Kind)
}

// Schedules serves /schedules and is the clock's enqueuer: internal/scheduler decides what is
// due, this decides what that means.
type Schedules struct {
	DB        *store.DB
	Authz     *authz.Authz
	Instances *Instances
	// Hub carries the maintenance signal. Nil sends nothing.
	Hub *ws.Hub
}

func scheduleRoutes(rt *routeTable, s *Schedules) {
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
	// Timezone is the location the expression is evaluated in.
	Timezone string `json:"timezone"`
	// WaitForEmpty, MaxDeferralSeconds and UnknownPlayers are the player policy of a restart
	// or backup schedule.
	WaitForEmpty       bool   `json:"wait_for_empty"`
	MaxDeferralSeconds int64  `json:"max_deferral_seconds"`
	UnknownPlayers     string `json:"unknown_players"`
	// DeferredSince is when the clock began holding a due run, null unless one is held.
	// DeferredUntil is the latest the held run starts.
	DeferredSince *time.Time `json:"deferred_since"`
	DeferredUntil *time.Time `json:"deferred_until"`
	// UpcomingRuns is the next few times the schedule fires, empty while it is disabled.
	UpcomingRuns []time.Time `json:"upcoming_runs"`
	// Steps is a chain schedule's step list, null for every other kind.
	Steps []control.ChainStep `json:"steps"`
}

// upcomingRunCount is how many fire times a schedule view lists.
const upcomingRunCount = 5

// upcomingRuns lists the next upcomingRunCount times s fires after now, starting from its stored
// next_run_at while that is still ahead. A disabled schedule or an unreadable expression has none.
func upcomingRuns(s *store.Schedule, now time.Time) []time.Time {
	runs := []time.Time{}
	if !s.Enabled {
		return runs
	}
	next := now.UTC()
	if s.NextRunAt != nil && s.NextRunAt.After(now) {
		next = s.NextRunAt.UTC()
		runs = append(runs, next)
	}
	for len(runs) < upcomingRunCount {
		t, err := scheduler.NextIn(s.Cron, s.Timezone, next)
		if err != nil {
			return []time.Time{}
		}
		next = t
		runs = append(runs, next)
	}
	return runs
}

// toScheduleView renders one schedule. usernames maps user ids to names; a nil map, or an id
// missing from it, leaves the name null rather than inventing one.
func toScheduleView(s *store.Schedule, usernames map[string]string) scheduleView {
	v := scheduleView{
		ID: s.ID, InstanceID: s.InstanceID, Kind: s.Kind, Cron: s.Cron, Enabled: s.Enabled,
		LastRunAt: s.LastRunAt, NextRunAt: s.NextRunAt, CreatedBy: s.CreatedBy,
		Timezone: s.Timezone, WaitForEmpty: s.WaitForEmpty,
		MaxDeferralSeconds: int64(s.MaxDeferral / time.Second), UnknownPlayers: s.UnknownPlayers,
		DeferredSince: s.DeferredSince, UpcomingRuns: upcomingRuns(s, time.Now().UTC()),
	}
	if s.DeferredSince != nil {
		until := s.DeferredSince.Add(s.MaxDeferral)
		v.DeferredUntil = &until
	}
	if s.Kind == jobs.KindChain.String() {
		if steps, err := control.ParseChainPayload(s.Payload); err == nil {
			v.Steps = steps
		}
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
func authorNames(ctx context.Context, db *store.DB) map[string]string {
	users, err := db.ListUsers(ctx)
	if err != nil {
		// A name is decoration on an audit field. Losing it must not cost the listing.
		slog.WarnContext(ctx, "authors could not be named", slog.Any("error", err))
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

// scheduleTimezone is the default for clients that omit a zone and for the list metadata.
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
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}

	// A schedule is visible to whoever can see what it acts on: the panel for a global kind,
	// the instance for the rest. Filtered here rather than by a Can() over the collection,
	// the same precedent as instances.go:list.
	usernames := authorNames(r.Context(), s.DB)
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
	JSON(w, r, http.StatusOK, struct {
		Page[scheduleView]
		Timezone string `json:"timezone"`
	}{NewPage(views, nil), scheduleTimezone})
}

// scheduleRequest is the create and update body. Kind and instance_id are read only on create:
// changing either makes it a different schedule, and the job rows already pointing at this one
// would then describe something it never was.
type scheduleRequest struct {
	InstanceID *string `json:"instance_id"`
	Kind       string  `json:"kind"`
	Cron       string  `json:"cron"`
	Timezone   *string `json:"timezone"`
	Enabled    *bool   `json:"enabled"`

	WaitForEmpty       *bool   `json:"wait_for_empty"`
	MaxDeferralSeconds *int64  `json:"max_deferral_seconds"`
	UnknownPlayers     *string `json:"unknown_players"`

	// Steps is a chain's step list. On an edit it replaces the whole list.
	Steps *[]control.ChainStep `json:"steps"`
}

// Bounds of max_deferral_seconds.
const (
	minDeferral = time.Minute
	maxDeferral = 24 * time.Hour
)

// applyPlayerPolicy copies the policy fields present in body onto row and validates the
// result. It reports false after writing the 422.
func applyPlayerPolicy(
	w http.ResponseWriter, r *http.Request, body *scheduleRequest, spec scheduleKind, row *store.Schedule,
) bool {
	if body.WaitForEmpty != nil {
		row.WaitForEmpty = *body.WaitForEmpty
	}
	if body.MaxDeferralSeconds != nil {
		secs := *body.MaxDeferralSeconds
		if secs < int64(minDeferral/time.Second) || secs > int64(maxDeferral/time.Second) {
			writeFieldError(w, r, "max_deferral_seconds", apierr.FieldOutOfRange,
				"Between 60 seconds and 24 hours.")
			return false
		}
		row.MaxDeferral = time.Duration(secs) * time.Second
	}
	if body.UnknownPlayers != nil {
		row.UnknownPlayers = *body.UnknownPlayers
	}
	if row.UnknownPlayers != store.UnknownPlayersWait && row.UnknownPlayers != store.UnknownPlayersRun {
		writeFieldError(w, r, "unknown_players", apierr.FieldInvalid, `Either "wait" or "run".`)
		return false
	}
	if row.WaitForEmpty && !spec.playerAware {
		message := "Only restart and backup schedules stop a running server, so only they can wait for players."
		if spec.chain {
			message = "Only a chain with a step that stops the server can wait for players."
		}
		writeFieldError(w, r, "wait_for_empty", apierr.FieldInvalid, message)
		return false
	}
	return true
}

// requestSteps reads and validates the steps a request carries. A chain needs them and any other
// kind refuses them. It reports false after writing the 422.
func requestSteps(
	w http.ResponseWriter, r *http.Request, spec scheduleKind, body *scheduleRequest,
) ([]control.ChainStep, bool) {
	if !spec.chain {
		if body.Steps != nil {
			writeFieldError(w, r, "steps", apierr.FieldInvalid, "Only a chain schedule has steps.")
			return nil, false
		}
		return nil, true
	}
	var steps []control.ChainStep
	if body.Steps != nil {
		steps = *body.Steps
	}
	problems := control.ValidateSteps(steps)
	if len(problems) == 0 {
		return steps, true
	}
	var val apierr.Validation
	for _, p := range problems {
		code := apierr.FieldInvalid
		switch p.Reason {
		case control.ProblemRequired:
			code = apierr.FieldRequired
		case control.ProblemOutOfRange:
			code = apierr.FieldOutOfRange
		}
		val.Add(p.Field, code, p.Message)
	}
	apierr.Write(w, r, val.Err())
	return nil, false
}

// storedSteps reads the steps of a chain row, and none for any other kind.
func storedSteps(spec scheduleKind, row *store.Schedule) ([]control.ChainStep, error) {
	if !spec.chain {
		return nil, nil
	}
	steps, err := control.ParseChainPayload(row.Payload)
	if err != nil {
		return nil, fmt.Errorf("read steps of schedule %s: %w", row.ID, err)
	}
	return steps, nil
}

func encodeSteps(steps []control.ChainStep) (string, error) {
	raw, err := json.Marshal(control.ChainPayload{Steps: steps})
	if err != nil {
		return "", fmt.Errorf("encode chain steps: %w", err)
	}
	return string(raw), nil
}

// lacksAction answers 403 and reports true when u lacks the action a schedule of kind spec
// needs, or the action of any of a chain's steps.
func (s *Schedules) lacksAction(
	w http.ResponseWriter, r *http.Request, u *store.User, spec scheduleKind, instanceID string,
	steps []control.ChainStep,
) bool {
	for _, action := range spec.actions(steps) {
		if !s.Authz.Can(r.Context(), u, action, instanceID) {
			apierr.Write(w, r, apierr.New(errcode.Forbidden))
			return true
		}
	}
	return false
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

	spec, scope, ok := s.scheduleTarget(w, r, &body)
	if !ok {
		return
	}
	if spec.global {
		if !s.Authz.Can(r.Context(), u, authz.SchedulesGlobal, "") {
			apierr.Write(w, r, apierr.New(errcode.NotFound))
			return
		}
	} else if !s.Authz.Can(r.Context(), u, authz.InstanceView, scope) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	steps, ok := requestSteps(w, r, spec, &body)
	if !ok {
		return
	}
	spec = spec.forSteps(steps)
	if s.lacksAction(w, r, u, spec, scope, steps) {
		return
	}
	row, ok := newScheduleRow(w, r, u, spec, &body, steps)
	if !ok {
		return
	}
	if err := s.insert(r.Context(), u, row); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusCreated, toScheduleView(row, map[string]string{u.ID: u.Username}))
}

// newScheduleRow validates the timing and player policy of a create body and builds the row. It
// reports false after writing the error.
func newScheduleRow(
	w http.ResponseWriter, r *http.Request, u *store.User, spec scheduleKind, body *scheduleRequest,
	steps []control.ChainStep,
) (*store.Schedule, bool) {
	zone, next, ok := scheduleStart(w, r, body)
	if !ok || !checkInterval(w, r, spec, body.Cron, zone) {
		return nil, false
	}
	payload := "{}"
	if spec.chain {
		var err error
		if payload, err = encodeSteps(steps); err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return nil, false
		}
	}
	row := &store.Schedule{
		ID: store.NewID(), Kind: spec.kind.String(), Cron: strings.TrimSpace(body.Cron), Timezone: zone,
		Payload: payload, Enabled: body.Enabled == nil || *body.Enabled,
		NextRunAt: &next, CreatedBy: &u.ID,
		MaxDeferral: store.DefaultMaxDeferral, UnknownPlayers: store.UnknownPlayersWait,
	}
	if !applyPlayerPolicy(w, r, body, spec, row) {
		return nil, false
	}
	if !spec.global {
		row.InstanceID = body.InstanceID
	}
	return row, true
}

// scheduleTarget reads the kind a new schedule runs and the instance it runs on, empty for a
// global kind, writing the field error when either is missing or unknown, and 404 when the
// instance does not exist.
func (s *Schedules) scheduleTarget(
	w http.ResponseWriter,
	r *http.Request,
	body *scheduleRequest,
) (scheduleKind, string, bool) {
	spec, ok := scheduleKinds[strings.TrimSpace(body.Kind)]
	if !ok {
		writeFieldError(w, r, "kind", apierr.FieldInvalid,
			"This is not a kind the panel can put on a schedule.")
		return scheduleKind{}, "", false
	}
	scope := deref(body.InstanceID)
	if !spec.global && scope == "" {
		writeFieldError(w, r, "instance_id", apierr.FieldRequired,
			"This kind runs against one instance, so it needs one.")
		return scheduleKind{}, "", false
	}
	if spec.global {
		return spec, "", true
	}
	inst, err := s.DB.InstanceByID(r.Context(), scope)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return scheduleKind{}, "", false
	}
	if inst == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return scheduleKind{}, "", false
	}
	return spec, scope, true
}

// insert stores a new schedule and, in the same transaction, who created it.
func (s *Schedules) insert(ctx context.Context, u *store.User, row *store.Schedule) error {
	detail := map[string]any{
		"kind": row.Kind, "cron": row.Cron, "enabled": row.Enabled,
		"timezone": row.Timezone,
	}
	if row.Kind == jobs.KindChain.String() {
		detail["steps"] = json.RawMessage(row.Payload)
	}
	if err := s.DB.CreateSchedule(ctx, row, scheduleAudit(ctx, u, row, "schedules.create", detail)); err != nil {
		return fmt.Errorf("insert schedule: %w", err)
	}
	return nil
}

// scheduleAudit is the record of a schedule change, against the instance the schedule runs on,
// or against none for a schedule of a global kind.
func scheduleAudit(
	ctx context.Context,
	u *store.User,
	row *store.Schedule,
	action string,
	detail any,
) *store.AuditEntry {
	return &store.AuditEntry{
		UserID: u.ID, InstanceID: deref(row.InstanceID), Action: action,
		Detail: detailJSON(detail), IP: clientIP(ctx),
	}
}

// scheduleChanges lists the editable fields an edit changed, named as the PATCH body names them.
func scheduleChanges(before, after *store.Schedule) []change {
	var out []change
	out = fieldChange(out, "cron", before.Cron, after.Cron)
	out = fieldChange(out, "timezone", before.Timezone, after.Timezone)
	out = fieldChange(out, "enabled", before.Enabled, after.Enabled)
	out = fieldChange(out, "wait_for_empty", before.WaitForEmpty, after.WaitForEmpty)
	out = fieldChange(out, "max_deferral_seconds",
		int64(before.MaxDeferral/time.Second), int64(after.MaxDeferral/time.Second))
	out = fieldChange(out, "unknown_players", before.UnknownPlayers, after.UnknownPlayers)
	return fieldChange(out, "steps", before.Payload, after.Payload)
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
			apierr.Write(w, r, apierr.New(errcode.NotFound))
			return
		}
	} else if !s.Authz.Can(r.Context(), u, authz.InstanceView, deref(row.InstanceID)) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	steps, err := storedSteps(spec, row)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if s.lacksAction(w, r, u, spec, deref(row.InstanceID), steps) {
		return
	}
	var body scheduleRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}

	wasHeld := row.DeferredSince != nil
	before := *row
	if steps, ok = s.editSteps(w, r, u, spec, row, &body, steps); !ok {
		return
	}
	spec = spec.forSteps(steps)
	released, ok := applyEdit(w, r, &body, spec, row)
	if !ok {
		return
	}
	var audit *store.AuditEntry
	if changes := scheduleChanges(&before, row); len(changes) > 0 {
		audit = scheduleAudit(r.Context(), u, row, "schedules.update", map[string]any{
			"kind": row.Kind, "changes": changes,
		})
	}
	if err := s.DB.UpdateSchedule(r.Context(), row, released, audit); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if wasHeld {
		s.announce(row)
	}
	JSON(w, r, http.StatusOK, toScheduleView(row, authorNames(r.Context(), s.DB)))
}

// editSteps replaces row's chain steps with those of a PATCH body, and returns the steps the row
// now has. It reports false after writing the error.
func (s *Schedules) editSteps(
	w http.ResponseWriter, r *http.Request, u *store.User, spec scheduleKind, row *store.Schedule,
	body *scheduleRequest, current []control.ChainStep,
) ([]control.ChainStep, bool) {
	if body.Steps == nil {
		return current, true
	}
	steps, ok := requestSteps(w, r, spec, body)
	if !ok || s.lacksAction(w, r, u, spec, deref(row.InstanceID), steps) {
		return nil, false
	}
	payload, err := encodeSteps(steps)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, false
	}
	row.Payload = payload
	return steps, true
}

// applyEdit copies a PATCH body onto row and releases a held run the edit ends: the schedule
// is disabled, its expression changed, or wait_for_empty turned off. It reports whether a
// hold was released, and false for ok after writing the 422.
func applyEdit(
	w http.ResponseWriter, r *http.Request, body *scheduleRequest, spec scheduleKind, row *store.Schedule,
) (released, ok bool) {
	rescheduled, ok := applyScheduleTiming(w, r, body, row)
	if !ok {
		return false, false
	}
	if (rescheduled || body.Steps != nil) && !checkInterval(w, r, spec, row.Cron, row.Timezone) {
		return false, false
	}
	if body.Enabled != nil {
		// A re-enabled schedule resumes at its next occurrence, not one missed while it was off.
		if *body.Enabled && !row.Enabled && !rescheduled {
			if next, ok := parseCron(row.Cron, row.Timezone); ok {
				row.NextRunAt = &next
			}
		}
		row.Enabled = *body.Enabled
	}
	if !applyPlayerPolicy(w, r, body, spec, row) {
		return false, false
	}
	released = row.DeferredSince != nil && (!row.Enabled || rescheduled || !row.WaitForEmpty)
	if released {
		row.DeferredSince = nil
	}
	return released, true
}

func applyScheduleTiming(
	w http.ResponseWriter, r *http.Request, body *scheduleRequest, row *store.Schedule,
) (rescheduled, ok bool) {
	if body.Timezone != nil {
		zone := strings.TrimSpace(*body.Timezone)
		if !validScheduleTimezone(zone) {
			writeFieldError(w, r, "timezone", apierr.FieldInvalid, "Use a valid IANA timezone.")
			return false, false
		}
		if zone != row.Timezone {
			row.Timezone = zone
			rescheduled = true
		}
	}
	if cron := strings.TrimSpace(body.Cron); cron != "" && cron != row.Cron {
		row.Cron = cron
		rescheduled = true
	}
	if !rescheduled {
		return false, true
	}
	next, ok := parseCron(row.Cron, row.Timezone)
	if !ok {
		writeFieldError(w, r, "cron", apierr.FieldInvalid, cronHelp(row.Cron))
		return false, false
	}
	row.NextRunAt = &next
	return true, true
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
			apierr.Write(w, r, apierr.New(errcode.NotFound))
			return
		}
	} else if !s.Authz.Can(r.Context(), u, authz.InstanceView, deref(row.InstanceID)) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	steps, err := storedSteps(spec, row)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if s.lacksAction(w, r, u, spec, deref(row.InstanceID), steps) {
		return
	}
	if err := s.DB.DeleteSchedule(r.Context(), row.ID, scheduleAudit(r.Context(), u, row, "schedules.delete",
		map[string]any{"kind": row.Kind, "cron": row.Cron})); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if row.DeferredSince != nil {
		s.announce(row)
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
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return nil, scheduleKind{}, false
	}
	if row == nil {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return nil, scheduleKind{}, false
	}
	spec, ok := scheduleKinds[row.Kind]
	if !ok {
		apierr.Write(w, r, apierr.New(errcode.Internal).
			Wrap(fmt.Errorf("schedule %s names kind %q, which this build cannot run", row.ID, row.Kind)))
		return nil, scheduleKind{}, false
	}
	return row, spec, true
}

// parseCron refuses an expression at the moment it is written rather than at tick time, and
// returns the first time it fires. An expression discovered to be unreadable at 03:00 is a
// schedule that silently never ran. A TZ= or CRON_TZ= prefix is refused too: the parser honours
// it, so the row would fire on a clock other than the timezone it is labelled with.
func parseCron(expr, zone string) (time.Time, bool) {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "TZ=") || strings.HasPrefix(expr, "CRON_TZ=") || strings.HasPrefix(expr, "@every") {
		return time.Time{}, false
	}
	next, err := scheduler.NextIn(expr, zone, time.Now().UTC())
	return next, err == nil
}

func scheduleStart(w http.ResponseWriter, r *http.Request, body *scheduleRequest) (string, time.Time, bool) {
	zone := scheduleTimezone
	if body.Timezone != nil {
		zone = strings.TrimSpace(*body.Timezone)
	}
	if !validScheduleTimezone(zone) {
		writeFieldError(w, r, "timezone", apierr.FieldInvalid, "Use a valid IANA timezone.")
		return "", time.Time{}, false
	}
	next, ok := parseCron(body.Cron, zone)
	if !ok {
		writeFieldError(w, r, "cron", apierr.FieldInvalid, cronHelp(body.Cron))
		return "", time.Time{}, false
	}
	return zone, next, true
}

func validScheduleTimezone(zone string) bool {
	if zone == "" || zone == "Local" {
		return false
	}
	_, err := time.LoadLocation(zone)
	return err == nil
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

// minStoppingInterval is the shortest gap between two runs of a kind that stops the server, so
// a schedule cannot take a server down every few minutes.
const minStoppingInterval = time.Hour

// checkInterval refuses an expression that would run a server-stopping kind more often than
// minStoppingInterval, judged over its next two days of runs. It reports false after writing
// the 422.
func checkInterval(w http.ResponseWriter, r *http.Request, spec scheduleKind, expr, zone string) bool {
	if !spec.playerAware && spec.kind != jobs.KindGameUpdate {
		return true
	}
	at := time.Now().UTC()
	prev, err := scheduler.NextIn(strings.TrimSpace(expr), zone, at)
	if err != nil {
		return true
	}
	for horizon := prev.Add(48 * time.Hour); prev.Before(horizon); {
		next, err := scheduler.NextIn(strings.TrimSpace(expr), zone, prev)
		if err != nil {
			return true
		}
		if next.Sub(prev) < minStoppingInterval {
			writeFieldError(w, r, "cron", apierr.FieldInvalid,
				"This kind stops the server, so it may run at most once an hour.")
			return false
		}
		prev = next
	}
	return true
}
