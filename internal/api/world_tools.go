package api

import (
	"errors"
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
)

// runWorldTool is POST /instances/{id}/world-tools: back up the stopped world, start the server
// and run a world-maintenance mod's command over RCON. It answers 202 with the job.
func (h *Instances) runWorldTool(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !h.Authz.Can(r.Context(), u, authz.InstanceView, id) {
		apierr.Write(w, r, apierr.New(errcode.NotFound))
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.BackupsRestore, id) {
		apierr.Write(w, r, apierr.New(errcode.Forbidden))
		return
	}
	var tool control.WorldTool
	if err := Decode(r, &tool); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if _, err := tool.Command(); err != nil {
		apierr.Write(w, r, apierr.New(errcode.ValidationFailed).Msg(err.Error()))
		return
	}
	inst, ok := h.mustLoadInstance(w, r, id)
	if !ok {
		return
	}
	if !checkInstanceState(w, r, inst, jobs.KindWorldTool) {
		return
	}
	if !operationSettled(w, r, h.DB, id) {
		return
	}
	for _, pkg := range []string{tool.Package(), command.ValheimRCONPackage} {
		_, _, installed, err := h.DB.InstanceModVersion(r.Context(), id, pkg)
		if err != nil {
			apierr.Write(w, r, apierr.New(errcode.Internal).Wrap(err))
			return
		}
		if !installed {
			apierr.Write(w, r, apierr.New(errcode.Unsupported).Msg("Install "+pkg+" on this server first."))
			return
		}
	}
	containerID, ok := h.mustHaveContainer(w, r, inst)
	if !ok {
		return
	}

	job, err := h.ctl.WorldTooler.Submit(r.Context(), &control.WorldToolSubmission{
		Instance: inst, ContainerID: containerID, Tool: tool, RequestedBy: u.ID,
		Audit: jobAudit(r.Context(), u.ID, id, "instances.world_tools.run", tool),
	})
	if errors.Is(err, control.ErrInvalidWorldTool) {
		apierr.Write(w, r, apierr.New(errcode.ValidationFailed).Msg(err.Error()))
		return
	}
	if err != nil {
		writeJobSubmitError(w, r, err)
		return
	}
	Accepted(w, r, job.ID, toJobView(job))
}
