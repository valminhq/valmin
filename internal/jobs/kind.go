package jobs

import "strconv"

// Kind is a job kind — a typed constant, exactly as authz.Action is (09 §4, 12 §3.1): an
// unknown kind is a compile error, not a row that sits queued forever because no worker
// recognises it. The unexported field closes the registry to this file.
type Kind struct{ name string }

// String is the wire form: job_runs.kind and the API's "kind" field.
func (k Kind) String() string { return k.name }

// MarshalJSON renders the kind as its wire name.
func (k Kind) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(k.name)), nil }

// The M1 register (12 §3.1). Kinds landing in later milestones are added to this file by
// the work package that implements them, not declared speculatively here.
var (
	KindProvision   = Kind{"provision"}
	KindStart       = Kind{"start"}
	KindStop        = Kind{"stop"}
	KindRestart     = Kind{"restart"}
	KindDelete      = Kind{"delete"}
	KindWorldImport = Kind{"world_import"}
)

// InstanceLockKey is 12 §4.3's lock_key for an instance-scoped job.
func InstanceLockKey(instanceID string) string { return "instance:" + instanceID }

// GlobalLockKey is 12 §4.3's lock_key for a global job (thunderstore_sync, key_rotate).
func GlobalLockKey(k Kind) string { return "global:" + k.name }
