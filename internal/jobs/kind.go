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
	// KindThunderstoreSync is M2's first global-scoped kind (12 §3.1): it takes no
	// instance lock, is idempotent, and is one of the three kinds 12 §9.4 allows automatic
	// retry with backoff — a bare download-and-upsert touches no world and no container.
	KindThunderstoreSync = Kind{"thunderstore_sync"}
	// KindModInstall is instance-scoped and 12 §9.4's one "not resumed" kind: a crash is
	// rolled back from the file manifest rather than continued, because the manifest is
	// written before files move and is therefore exact where a half-applied tree is not.
	KindModInstall = Kind{"mod_install"}
	// KindModUninstall is instance-scoped and, unlike mod_install, not cancellable at all
	// (12 §3.1): it is seconds of file removal driven by a manifest, and the only
	// interruptible half would be "some of the package is gone". A crash rolls it back
	// from what it saved before it removed anything.
	KindModUninstall = Kind{"mod_uninstall"}
)

// resumeIntentHonoured is ADR-032 / 12 §9.3: resume intent — "this server was running and
// owes the user a restart" — is honoured only for kinds whose failure cannot leave world
// data half-written.
//
// `backup` (M4) is the kind that qualifies: the archive is discardable and the world was
// never touched, and a panel that restarts overnight during a scheduled backup must not
// leave the server down until morning. `restore` and `game_update` never qualify — on-disk
// state is unproven, and auto-starting a server whose world may be half-swapped writes new
// data on top of a corrupt save, turning a recoverable situation into an unrecoverable one.
//
// `↯` Empty at M1, and that is not an oversight: no M1 kind stops a running server as a
// step, so none of them sets resume_after. The map exists because the branch that is never
// written is the branch that is wrong at M4.
var resumeIntentHonoured = map[Kind]bool{}

// ResumeIntentHonoured reports whether a job of this kind may have its resume_after intent
// acted on after a crash (12 §9.1 step 4).
func ResumeIntentHonoured(k Kind) bool { return resumeIntentHonoured[k] }

// ByName resolves job_runs.kind back to the typed constant. A row whose kind no build
// recognises is reported as unknown rather than silently treated as one of the known kinds —
// the same closed-registry discipline the constants themselves enforce.
func ByName(name string) (Kind, bool) {
	for _, k := range []Kind{
		KindProvision, KindStart, KindStop, KindRestart, KindDelete, KindWorldImport,
		KindThunderstoreSync, KindModInstall, KindModUninstall,
	} {
		if k.name == name {
			return k, true
		}
	}
	return Kind{}, false
}

// InstanceLockKey is 12 §4.3's lock_key for an instance-scoped job.
func InstanceLockKey(instanceID string) string { return "instance:" + instanceID }

// GlobalLockKey is 12 §4.3's lock_key for a global job (thunderstore_sync, key_rotate).
func GlobalLockKey(k Kind) string { return "global:" + k.name }
