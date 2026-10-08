package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

// shutdownTickInterval is how often the power cut clock looks. It bounds how late a warning or
// a stop can start after its lead time.
const shutdownTickInterval = 10 * time.Second

// Shutdowns serves /admin/shutdowns, the planned power cuts, and is the power cut clock's
// hooks. A planned power cut stops every server, so only an admin may plan one.
type Shutdowns struct {
	DB        *store.DB
	Authz     *authz.Authz
	Instances *Instances
}

func shutdownRoutes(rt *routeTable, h *Shutdowns) {
	rt.Handle("GET /api/v1/admin/shutdowns", http.HandlerFunc(h.list))
	rt.Handle("POST /api/v1/admin/shutdowns", http.HandlerFunc(h.create))
	rt.Handle("DELETE /api/v1/admin/shutdowns/{id}", http.HandlerFunc(h.remove))
}

// shutdownView is one planned power cut on the wire. CreatedByUsername names the panel account
// that planned it, CreatedByName anyone else, such as a Discord admin.
type shutdownView struct {
	ID                string    `json:"id"`
	PowerOffAt        time.Time `json:"power_off_at"`
	CreatedByUsername *string   `json:"created_by_username"`
	CreatedByName     string    `json:"created_by_name"`
}

type shutdownRequest struct {
	// LocalTime is a wall-clock time in Timezone: "2006-01-02T15:04", "2006-01-02 15:04", or
	// "15:04" for the next such time of day.
	LocalTime string `json:"local_time"`
	Timezone  string `json:"timezone"`
}

func (h *Shutdowns) list(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SchedulesGlobal, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	planned, err := h.DB.UpcomingShutdowns(r.Context(), time.Now().UTC())
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	names := authorNames(r.Context(), h.DB)
	views := make([]shutdownView, 0, len(planned))
	for i := range planned {
		views = append(views, toShutdownView(&planned[i], names))
	}
	JSON(w, r, http.StatusOK, NewPage(views, nil))
}

func (h *Shutdowns) create(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SchedulesGlobal, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	var body shutdownRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	zone := strings.TrimSpace(body.Timezone)
	if !validScheduleTimezone(zone) {
		writeFieldError(w, r, "timezone", apierr.FieldInvalid, "Use a valid IANA timezone.")
		return
	}
	at, err := scheduler.PowerOffTime(body.LocalTime, zone, time.Now().UTC())
	switch {
	case errors.Is(err, scheduler.ErrPastPowerOffTime):
		writeFieldError(w, r, "local_time", apierr.FieldOutOfRange, "That time has already passed.")
		return
	case err != nil:
		writeFieldError(w, r, "local_time", apierr.FieldInvalid, "Enter a date and a time.")
		return
	}
	row := &store.PlannedShutdown{ID: store.NewID(), PowerOffAt: at, CreatedBy: &u.ID}
	audit := &store.AuditEntry{
		UserID: u.ID, Action: "shutdowns.create", IP: clientIP(r.Context()),
		Detail: detailJSON(map[string]any{"power_off_at": at, "timezone": zone}),
	}
	if err := h.DB.CreatePlannedShutdown(r.Context(), row, audit); err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusCreated, toShutdownView(row, map[string]string{u.ID: u.Username}))
}

func (h *Shutdowns) remove(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.SchedulesGlobal, "") {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	found, err := h.DB.DeletePlannedShutdown(r.Context(), id, &store.AuditEntry{
		UserID: u.ID, Action: "shutdowns.delete", IP: clientIP(r.Context()),
		Detail: detailJSON(map[string]any{"id": id}),
	})
	switch {
	case err != nil:
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
	case !found:
		apierr.Write(w, r, apierr.New(errcode.NotFound))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// toShutdownView renders one planned power cut. usernames maps user ids to names.
func toShutdownView(s *store.PlannedShutdown, usernames map[string]string) shutdownView {
	v := shutdownView{ID: s.ID, PowerOffAt: s.PowerOffAt, CreatedByName: s.CreatedByName}
	if s.CreatedBy != nil {
		if name, ok := usernames[*s.CreatedBy]; ok {
			v.CreatedByUsername = &name
		}
	}
	return v
}

// Stop is the power cut clock's Stop hook: it submits a stop of inst, recorded as the planned
// shutdown's.
func (h *Shutdowns) Stop(ctx context.Context, inst *store.Instance, powerOff time.Time) error {
	_, err := h.Instances.ctl.Stopper.Submit(ctx, &control.StopSubmission{
		Instance: inst, ContainerID: deref(inst.ContainerID),
		Audit: &store.AuditEntry{
			InstanceID: inst.ID, Action: "instances.stop", ActorName: "Planned shutdown",
			Detail: detailJSON(map[string]any{"reason": "planned_shutdown", "power_off_at": powerOff}),
		},
	})
	if err != nil {
		return fmt.Errorf("submit planned shutdown stop: %w", err)
	}
	return nil
}

// Warn is the power cut clock's Warn hook: it tells inst's players in chat that the server
// stops within left. While nobody is known to be online it reports false, so a player who joins
// later is still warned.
func (h *Shutdowns) Warn(ctx context.Context, inst *store.Instance, left time.Duration) bool {
	if reader := h.Instances.Streams.Reader(inst.ID); reader != nil {
		if players := reader.Players(); players != nil && *players == 0 {
			return false
		}
	}
	return h.Instances.say(ctx, inst,
		fmt.Sprintf("Planned power cut: the server shuts down in %s.", spokenDuration(left)))
}
