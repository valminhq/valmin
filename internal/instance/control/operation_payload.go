package control

import "github.com/valminhq/valmin/internal/mods/manager"

// OperationStep is one durable link of an instance definition chain.
type OperationStep struct {
	Kind  string `json:"kind"`
	Ref   string `json:"ref,omitempty"`
	JobID string `json:"job_id,omitempty"`
}

// OperationPlan retains the inputs required by outstanding definition steps. MergeConfigs
// applies Configs as settings over the files the mods placed rather than as whole files.
type OperationPlan struct {
	Mods         []manager.PackageRequest `json:"mods,omitempty"`
	Configs      []ManifestConfig         `json:"configs,omitempty"`
	MergeConfigs bool                     `json:"merge_configs,omitempty"`
	Sides        map[string]string        `json:"sides,omitempty"`
	Start        bool                     `json:"start_after_provision,omitempty"`
}
