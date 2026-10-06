package api

import (
	"net/http"

	apierr "github.com/valminhq/valmin/internal/api/errors"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/instance/control"
)

// Orphan is the HTTP view of a managed container no instance row claims.
type Orphan struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	InstanceID  string `json:"instance_id"`
	BasePort    int    `json:"base_port"`
	Running     bool   `json:"running"`
}

func (h *Instances) orphans(w http.ResponseWriter, r *http.Request) {
	u, ok := caller(w, r)
	if !ok {
		return
	}
	if !h.Authz.Can(r.Context(), u, authz.InstanceAdopt, "") {
		apierr.Write(w, r, apierr.New(apierr.Forbidden))
		return
	}
	found, err := control.ListOrphans(r.Context(), h.DB, h.Runtime)
	if err != nil {
		apierr.Write(w, r, apierr.New(apierr.Internal).Wrap(err))
		return
	}
	view := make([]Orphan, 0, len(found))
	for _, orphan := range found {
		view = append(view, Orphan{
			ContainerID: orphan.ContainerID, Name: orphan.Name, InstanceID: orphan.InstanceID,
			BasePort: orphan.BasePort, Running: orphan.Running,
		})
	}
	JSON(w, r, http.StatusOK, NewPage(view, nil))
}
