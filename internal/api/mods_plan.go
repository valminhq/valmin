package api

import (
	"github.com/valminhq/valmin/internal/mods/manager"
	modresolver "github.com/valminhq/valmin/internal/mods/resolver"
)

const (
	changeNone      = "none"
	changeInstall   = "install"
	changeUpgrade   = "upgrade"
	changeDowngrade = "downgrade"
)

// removalView is an installed package a change uninstalls.
type removalView struct {
	FullName string `json:"full_name"`
	Source   string `json:"source"`
	Version  string `json:"version"`
}

// conflictView is a dependency a change would leave unmet: FullName at Version needs Dependency
// at Requires or newer, and the change leaves it at Have, or removes it when Have is empty.
type conflictView struct {
	FullName   string `json:"full_name"`
	Version    string `json:"version"`
	Dependency string `json:"dependency"`
	Requires   string `json:"requires"`
	Have       string `json:"have"`
	// Locked is true when Dependency is locked, so unlocking it is one way through.
	Locked bool `json:"locked"`
}

// changeOf names what a preview node does to the installed version.
func changeOf(from, to string, noOp bool) string {
	switch {
	case noOp || from == to:
		return changeNone
	case from == "":
		return changeInstall
	case manager.Newer(from, to):
		return changeDowngrade
	default:
		return changeUpgrade
	}
}

// toConflictViews renders conflicts for a preview, marking a locked dependency.
func toConflictViews(conflicts []modresolver.Conflict, idx *manager.Index) []conflictView {
	out := make([]conflictView, 0, len(conflicts))
	for _, c := range conflicts {
		row, ok := idx.Have[c.Dependency]
		out = append(out, conflictView{
			FullName: c.FullName, Version: c.Version, Dependency: c.Dependency,
			Requires: c.Requires, Have: c.Have, Locked: ok && row.Locked,
		})
	}
	return out
}

// removalViews renders the packages a plan uninstalls.
func removalViews(names []string, idx *manager.Index) []removalView {
	out := make([]removalView, 0, len(names))
	for _, name := range names {
		row := idx.Have[name]
		out = append(out, removalView{FullName: name, Source: row.Source.String(), Version: row.Version})
	}
	return out
}
