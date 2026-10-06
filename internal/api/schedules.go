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
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
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
	JSON(w, r, http.StatusOK, struct {
		Page[scheduleView]
		Timezone string `json:"timezone"`
	}{NewPage(views, nil), scheduleTimezone})
}

// scheduleRequest is the create and update body. Kind and instance_id are read only on create:
// changing either makes it a different schedule, and the job rows already pointing at this one
// would then describe something it never was.
type scheduleRequest struct {
	InstanceID *string          `json:"instance_id"`
	Kind       string           `json:"kind"`
	Cron       string           `json:"cron"`
	Timezone   *string          `json:"timezone"`
	Payload    *json.RawMessage `json:"payload"`
	Enabled    *bool            `json:"enabled"`

	WaitForEmpty       *bool   `json:"wait_for_empty"`
	MaxDeferralSeconds *int64  `json:"max_deferral_seconds"`
	UnknownPlayers     *string `json:"unknown_players"`
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
		writeFieldError(w, r, "wait_for_empty", apierr.FieldInvalid,
			"Only restart and backup schedules stop a running server, so only they can wait for players.")
		return false
	}
	return true
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
			apierr.Write(w, r, apierr.New(errcode.NotFound))
			return
		}
	} else {
		if !s.Authz.Can(r.Context(), u, authz.InstanceView, scope) {
			apierr.Write(w, r, apierr.New(errcode.NotFound))
			return
		}
		if !s.Authz.Can(r.Context(), u, spec.action, scope) {
			apierr.Write(w, r, apierr.New(errcode.Forbidden))
			return
		}
	}

	zone, next, ok := scheduleStart(w, r, &body)
	if !ok {
		return
	}

	row := &store.Schedule{
		ID: store.NewID(), Kind: spec.kind.String(), Cron: strings.TrimSpace(body.Cron), Timezone: zone,
		Payload: payloadOf(body.Payload), Enabled: body.Enabled == nil || *body.Enabled,
		NextRunAt: &next, CreatedBy: &u.ID,
		MaxDeferral: store.DefaultMaxDeferral, UnknownPlayers: store.UnknownPlayersWait,
	}
	if !applyPlayerPolicy(w, r, &body, spec, row) {
		return
	}
	if !spec.global {
		row.InstanceID = body.InstanceID
	}
	if err := s.insert(r.Context(), u, row); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusCreated, toScheduleView(row, map[string]string{u.ID: u.Username}))
}

// insert stores a new schedule and records who created it.
func (s *Schedules) insert(ctx context.Context, u *store.User, row *store.Schedule) error {
	if err := s.DB.CreateSchedule(ctx, row); err != nil {
		return fmt.Errorf("insert schedule: %w", err)
	}
	return s.audit(ctx, u, row, "schedules.create", map[string]any{
		"kind": row.Kind, "cron": row.Cron, "enabled": row.Enabled,
		"timezone": row.Timezone,
	})
}

// audit records a schedule change against the instance the schedule runs on, or against none
// for a schedule of a global kind.
func (s *Schedules) audit(
	ctx context.Context, u *store.User, row *store.Schedule, action string, detail any,
) error {
	if err := s.DB.WriteAuditLog(ctx, &store.AuditEntry{
		UserID: u.ID, InstanceID: deref(row.InstanceID), Action: action,
		Detail: detailJSON(detail), IP: clientIP(ctx),
	}); err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
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
	return fieldChange(out, "payload", before.Payload, after.Payload)
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
	} else {
		if !s.Authz.Can(r.Context(), u, authz.InstanceView, deref(row.InstanceID)) {
			apierr.Write(w, r, apierr.New(errcode.NotFound))
			return
		}
		if !s.Authz.Can(r.Context(), u, spec.action, deref(row.InstanceID)) {
			apierr.Write(w, r, apierr.New(errcode.Forbidden))
			return
		}
	}
	var body scheduleRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}

	wasHeld := row.DeferredSince != nil
	before := *row
	released, ok := applyEdit(w, r, &body, spec, row)
	if !ok {
		return
	}
	if err := s.DB.UpdateSchedule(r.Context(), row, released); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if wasHeld {
		s.announce(row)
	}
	if changes := scheduleChanges(&before, row); len(changes) > 0 {
		if err := s.audit(r.Context(), u, row, "schedules.update", map[string]any{
			"kind": row.Kind, "changes": changes,
		}); err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return
		}
	}
	JSON(w, r, http.StatusOK, toScheduleView(row, s.authorNames(r.Context())))
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
	if body.Payload != nil {
		row.Payload = payloadOf(body.Payload)
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
	} else {
		if !s.Authz.Can(r.Context(), u, authz.InstanceView, deref(row.InstanceID)) {
			apierr.Write(w, r, apierr.New(errcode.NotFound))
			return
		}
		if !s.Authz.Can(r.Context(), u, spec.action, deref(row.InstanceID)) {
			apierr.Write(w, r, apierr.New(errcode.Forbidden))
			return
		}
	}
	if err := s.DB.DeleteSchedule(r.Context(), row.ID); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	if row.DeferredSince != nil {
		s.announce(row)
	}
	if err := s.audit(r.Context(), u, row, "schedules.delete", map[string]any{
		"kind": row.Kind, "cron": row.Cron,
	}); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
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
	if strings.HasPrefix(expr, "TZ=") || strings.HasPrefix(expr, "CRON_TZ=") {
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

func payloadOf(raw *json.RawMessage) string {
	if raw == nil || len(*raw) == 0 {
		return "{}"
	}
	return string(*raw)
}

// announce tells the instance's subscribers that a schedule started or stopped holding a run.
func (s *Schedules) announce(sc *store.Schedule) {
	if s.Hub != nil && sc.InstanceID != nil {
		s.Hub.PublishMaintenance(*sc.InstanceID)
	}
}

// Occupied is the scheduler's Occupied hook: whether players may be connected to the server a
// due run of sc would stop. A failed read answers true, so it never stops a server over players.
func (s *Schedules) Occupied(ctx context.Context, sc *store.Schedule) bool {
	if spec, ok := scheduleKinds[sc.Kind]; !ok || !spec.playerAware || sc.InstanceID == nil {
		return false
	}
	inst, err := s.DB.InstanceByID(ctx, *sc.InstanceID)
	if err != nil {
		slog.WarnContext(ctx, "scheduled run held: instance could not be read",
			slog.String("schedule_id", sc.ID), slog.Any("error", err))
		return true
	}
	if inst == nil {
		return false
	}
	var players *int
	if reader := s.Instances.Streams.Reader(inst.ID); reader != nil {
		players = reader.Players()
	}
	return occupied(instance.State(inst.State), players, sc.UnknownPlayers)
}

// Warn is the scheduler's Warn hook: it tells the players of sc's server in chat, with the RCON
// plugin's say command, that a held run stops the server within left at the latest. A server
// without the plugin is skipped. It reports false when the line should be sent again.
func (s *Schedules) Warn(ctx context.Context, sc *store.Schedule, left time.Duration) bool {
	if sc.InstanceID == nil || s.Instances.Commands == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, warnTimeout)
	defer cancel()
	inst, err := s.DB.InstanceByID(ctx, *sc.InstanceID)
	if err != nil {
		slog.WarnContext(ctx, "players not warned: instance could not be read",
			slog.String("schedule_id", sc.ID), slog.String("instance_id", *sc.InstanceID), slog.Any("error", err))
		return false
	}
	if inst == nil {
		return true
	}
	_, err = s.Instances.Commands.Send(ctx, inst, "say "+playerWarning(sc.Kind, left), true)
	switch {
	case err == nil:
		slog.InfoContext(ctx, "players warned of a held run",
			slog.String("schedule_id", sc.ID), slog.String("instance_id", inst.ID))
	case errors.Is(err, command.ErrUnsupported), errors.Is(err, command.ErrInvalidState):
		// No plugin, or the server is no longer running: nobody to warn.
	default:
		slog.WarnContext(ctx, "players could not be warned of a held run",
			slog.String("schedule_id", sc.ID), slog.String("instance_id", inst.ID), slog.Any("error", err))
		return false
	}
	return true
}

// warnTimeout bounds one player warning, which runs on the scheduler's tick.
const warnTimeout = 15 * time.Second

// playerWarning is the chat line for a held run of kind that starts within left at the latest.
func playerWarning(kind string, left time.Duration) string {
	action := "restarts"
	if kind == jobs.KindBackup.String() {
		action = "restarts for a backup"
	}
	return fmt.Sprintf("The server %s when all players have left, or in %s at the latest.",
		action, spokenDuration(left))
}

// spokenDuration writes d rounded up to whole minutes, or in hours when it is whole hours.
func spokenDuration(d time.Duration) string {
	n, unit := int((d+time.Minute-1)/time.Minute), "minute"
	if n%60 == 0 {
		n, unit = n/60, "hour"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}

// occupied decides whether a server may have players connected. Only a running server can; an
// unknown count counts as occupied unless the schedule says to run.
func occupied(state instance.State, players *int, unknown string) bool {
	if state != instance.StateRunning {
		return false
	}
	if players == nil {
		return unknown != store.UnknownPlayersRun
	}
	return *players > 0
}

// Enqueue submits one due schedule through the scheduler's execution policy.
func (s *Schedules) Enqueue(ctx context.Context, sc *store.Schedule) error {
	spec, ok := scheduleKinds[sc.Kind]
	if !ok {
		return fmt.Errorf("schedule %s names kind %q, which this build cannot run", sc.ID, sc.Kind)
	}
	if err := s.submitter().Enqueue(ctx, sc, spec.kind, spec.global); err != nil {
		return fmt.Errorf("enqueue schedule %s: %w", sc.ID, err)
	}
	return nil
}

func (s *Schedules) submitter() *scheduler.Submitter {
	return &scheduler.Submitter{
		DB:             s.DB,
		SubmitGlobal:   s.submitScheduledGlobal,
		SubmitInstance: s.submitScheduledInstance,
	}
}

func (s *Schedules) submitScheduledGlobal(ctx context.Context, sc *store.Schedule, kind jobs.Kind) error {
	switch kind {
	case jobs.KindPrune:
		_, err := (&control.Pruner{DB: s.DB, Engine: s.Instances.Engine}).Submit(ctx, sc.ID)
		if err != nil {
			return fmt.Errorf("submit scheduled prune: %w", err)
		}
		return nil
	case jobs.KindUpdateCheck:
		_, err := s.Instances.updateChecker().Submit(ctx, sc.ID)
		if err != nil {
			return fmt.Errorf("submit scheduled update check: %w", err)
		}
		return nil
	case jobs.KindAlertScan:
		_, err := s.Instances.alertScanner().Submit(ctx, sc.ID)
		if err != nil {
			return fmt.Errorf("submit scheduled alert scan: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("no runner for global kind %s", kind)
	}
}

func (s *Schedules) submitScheduledInstance(
	ctx context.Context, sc *store.Schedule, kind jobs.Kind, inst *store.Instance, containerID string,
) error {
	switch kind {
	case jobs.KindBackup:
		_, err := s.Instances.backupper().Submit(ctx, &control.BackupSubmission{
			Instance: inst, ContainerID: containerID, Mode: control.BackupQuiesced, ScheduleID: sc.ID,
		})
		return err //nolint:wrapcheck // keep the original skip reason
	case jobs.KindRestart:
		if containerID == "" {
			return errors.New("the instance has no container")
		}
		_, err := s.Instances.restarter().Submit(ctx, &control.RestartSubmission{
			Instance: inst, ContainerID: containerID, ScheduleID: sc.ID,
		})
		return err //nolint:wrapcheck // keep the original skip reason
	case jobs.KindGameUpdate:
		_, err := s.Instances.gameUpdater().Submit(ctx, &control.GameUpdateSubmission{
			Instance: inst, ScheduleID: sc.ID,
		})
		return err //nolint:wrapcheck // keep the original skip reason
	default:
		return fmt.Errorf("no runner for instance kind %s", kind)
	}
}
