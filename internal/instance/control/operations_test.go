package control

import (
	"testing"

	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/mods/manager"
	"github.com/valminhq/valmin/internal/store"
)

func TestOperationStepErrorsPreserveMessages(t *testing.T) {
	operations := &Operations{}
	inst := &store.Instance{ID: "inst"}
	plan := &OperationPlan{Mods: []manager.PackageRequest{{FullName: "A-B"}}}
	cases := []struct {
		step OperationStep
		want string
	}{
		{OperationStep{Kind: jobs.KindModInstall.String(), Ref: "A-B"}, "mods requested but no mod engine is wired"},
		{OperationStep{Kind: jobs.KindStart.String()}, "instance inst has no container to start"},
		{OperationStep{Kind: "unknown"}, "no chain step defined for kind unknown"},
	}
	for _, tc := range cases {
		_, err := operations.SubmitStep(t.Context(), inst, tc.step, plan, "")
		if err == nil || err.Error() != tc.want {
			t.Errorf("SubmitStep(%q) error = %v, want %q", tc.step.Kind, err, tc.want)
		}
	}
}
