package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

type updateTestRequest struct {
	Game bool `json:"game"`
	Mods bool `json:"mods"`
}

// canTryUpdates reports whether u may run the work of an update test on the server: it clones
// the server, updates the game and the mods, and starts the copy. The actions in more are what
// the caller's own job adds. The copy has no grants of its own, so what its jobs do is checked
// against the server it was made from.
func (h *Instances) canTryUpdates(
	r *http.Request, u *store.User, id string, more ...authz.Action,
) bool {
	base := []authz.Action{authz.InstanceClone, authz.InstanceUpdate, authz.ModsManage, authz.InstanceStart}
	for _, action := range append(base, more...) {
		if !h.Authz.Can(r.Context(), u, action, id) {
			return false
		}
	}
	return true
}

// startUpdateTest is POST /instances/{id}/update-test: copy the server, put the pending game
// build and mod updates on the copy and boot it. The server may be running.
func (h *Instances) startUpdateTest(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.canTryUpdates(r, u, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok || !checkInstanceState(w, r, inst, jobs.KindUpdateTest) {
		return
	}
	var body updateTestRequest
	if err := Decode(r, &body); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if !body.Game && !body.Mods {
		writeFieldError(w, r, "game", apierr.FieldRequired, "Choose the game, the mods or both.")
		return
	}
	job, err := h.ctl.UpdateTester.Submit(r.Context(), &control.UpdateTestSubmission{
		Instance: inst, Game: body.Game, Mods: body.Mods, RequestedBy: u.ID,
		Audit: jobAudit(r.Context(), u.ID, inst.ID, "instances.update_test.start", body),
	})
	if err != nil {
		writeUpdateTestError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// applyUpdateTest is POST /instances/{id}/update-test/apply: install what the copy runs onto
// the server, restart it if it was running and delete the copy.
func (h *Instances) applyUpdateTest(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.canTryUpdates(r, u, id, authz.InstanceStop, authz.InstanceDelete) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok || !checkInstanceState(w, r, inst, jobs.KindUpdateApply) {
		return
	}
	job, err := h.ctl.UpdateTester.SubmitApply(r.Context(), &control.UpdateApplySubmission{
		Instance: inst, RequestedBy: u.ID,
		Audit: jobAudit(r.Context(), u.ID, inst.ID, "instances.update_test.apply", struct{}{}),
	})
	if err != nil {
		writeUpdateTestError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}

// writeUpdateTestError answers the refusals of an update test or its apply.
func writeUpdateTestError(w http.ResponseWriter, r *http.Request, err error) {
	var exists *control.UpdateTestExistsError
	var lowMemory *control.LowMemoryError
	switch {
	case errors.As(err, &exists):
		apierr.Write(w, r, apierr.New(errcode.InvalidState).With("test_copy_id", exists.CopyID).
			Msg("This server already has an update test copy. Apply it or discard it first."))
	case errors.Is(err, control.ErrNestedUpdateTest):
		apierr.Write(w, r, apierr.New(errcode.InvalidState).
			Msg("An update test copy cannot itself be tested."))
	case errors.Is(err, control.ErrNoUpdateTest):
		apierr.Write(w, r, apierr.New(errcode.NotFound))
	case errors.Is(err, control.ErrUpdateTestUnsettled):
		apierr.Write(w, r, apierr.New(errcode.InvalidState).
			Msg("The update test copy is not ready to be applied. Wait for its test to finish, or "+
				"discard the copy and test again."))
	case errors.As(err, &lowMemory):
		apierr.Write(w, r, apierr.New(errcode.InsufficientMemory).
			With("available_mb", lowMemory.AvailableMB).With("needed_mb", lowMemory.NeededMB))
	default:
		writeJobSubmitError(w, r, err)
	}
}

// testModChange is a package a copy runs at a newer version than its server, with the registry
// its files came from.
type testModChange struct {
	FullName string `json:"full_name"`
	From     string `json:"from,omitempty"`
	To       string `json:"to"`
	Source   string `json:"source"`
}

// failedModView is a mod the copy's loader said it could not load.
type failedModView struct {
	FullName  string `json:"full_name"`
	LoadError string `json:"load_error"`
}

// updateTestView is what a server's update-test copy runs and how it booted.
type updateTestView struct {
	CopyID        string   `json:"copy_id"`
	CopyName      string   `json:"copy_name"`
	CopyState     string   `json:"copy_state"`
	CopyPort      int      `json:"copy_port"`
	CopyMemLimit  int      `json:"copy_mem_limit_mb"`
	CopyCPULimit  *float64 `json:"copy_cpu_limit"`
	Branch        string   `json:"branch"`
	SourceBuildID string   `json:"source_build_id"`
	CopyBuildID   string   `json:"copy_build_id"`
	// GameNewer is whether the copy runs a newer game build than the server, which is when an
	// apply installs a build.
	GameNewer bool `json:"game_newer"`
	// Applicable is whether the copy can be applied now: it is settled and its test ran to the
	// end.
	Applicable bool `json:"applicable"`
	// ModChanges are the packages the copy runs at a newer version than the server.
	ModChanges []testModChange `json:"mod_changes"`
	// Failed and NotSeen are the copy's load verification, empty until it has booted.
	Failed     []failedModView `json:"failed"`
	NotSeen    []string        `json:"not_seen"`
	PluginLoad *pluginLoadView `json:"plugin_load"`
}

// updateTestReport is GET /instances/{id}/update-test: {"test": ...} for the server's update-test
// copy, or {"test": null} when it has none.
func (h *Instances) updateTestReport(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	testCopy, err := h.DB.InstanceTestCopy(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	// The copy has no grants of its own, so only someone who can see it, an administrator,
	// is told it exists.
	if testCopy == nil || !h.Authz.Can(r.Context(), u, authz.InstanceView, testCopy.ID) {
		JSON(w, r, http.StatusOK, map[string]*updateTestView{"test": nil})
		return
	}
	view, err := h.updateTestView(r, inst, testCopy)
	if err != nil {
		apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
		return
	}
	JSON(w, r, http.StatusOK, map[string]*updateTestView{"test": view})
}

// updateTestView compares a server with its copy and reads the copy's load verification. A copy
// whose game files are not in place yet, as while it is being created, reports no changes.
func (h *Instances) updateTestView(r *http.Request, inst, testCopy *store.Instance) (*updateTestView, error) {
	view := &updateTestView{
		CopyID: testCopy.ID, CopyName: testCopy.Name, CopyState: testCopy.State, CopyPort: testCopy.BasePort,
		CopyMemLimit: testCopy.MemLimitMB, CopyCPULimit: testCopy.CPULimit, Branch: inst.GameBranch,
		ModChanges: []testModChange{}, Failed: []failedModView{}, NotSeen: []string{},
	}
	if instance.State(testCopy.State) == instance.StateProvisioning {
		return view, nil
	}
	view.Applicable = h.ctl.UpdateTester.ApplyBlocker(r.Context(), inst, testCopy) == nil
	changes, err := h.ctl.UpdateTester.Changes(r.Context(), inst, testCopy)
	if err != nil {
		// A copy whose creation failed may have no game files. It is still reported, so it can
		// be discarded.
		slog.WarnContext(r.Context(), "could not compare the update test copy with its server",
			slog.String("instance_id", testCopy.ID), slog.Any("error", err))
		return view, nil
	}
	view.SourceBuildID, view.CopyBuildID = changes.SourceBuildID, changes.CopyBuildID
	view.GameNewer = changes.GameNewer()
	for _, m := range changes.Mods {
		view.ModChanges = append(
			view.ModChanges,
			testModChange{FullName: m.FullName, From: m.FromVersion, To: m.Version, Source: m.Source},
		)
	}
	load, err := instance.ReadPluginLoad(testCopy.DataDir, h.Streams.Patterns())
	if err != nil {
		slog.WarnContext(r.Context(), "could not read the update test copy's BepInEx log",
			slog.String("instance_id", testCopy.ID), slog.Any("error", err))
	}
	view.PluginLoad = toPluginLoadView(load)
	mods, err := h.DB.InstanceMods(r.Context(), testCopy.ID)
	if err != nil {
		return nil, err //nolint:wrapcheck // the handler wraps it
	}
	for i := range mods {
		if !mods[i].Enabled {
			continue
		}
		var manifest []installer.ManifestEntry
		_ = json.Unmarshal([]byte(mods[i].FileManifest), &manifest)
		status, reason := loadStatus(mods[i].FullName, manifest, load)
		switch {
		case status == nil:
		case *status == LoadFailed:
			view.Failed = append(view.Failed, failedModView{FullName: mods[i].FullName, LoadError: *reason})
		case *status == LoadNotSeen:
			view.NotSeen = append(view.NotSeen, mods[i].FullName)
		}
	}
	return view, nil
}
