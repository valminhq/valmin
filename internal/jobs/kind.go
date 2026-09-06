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

// The kind register. New kinds are added to this file by
// the work package that implements them, not declared speculatively here.
var (
	KindProvision   = Kind{"provision"}
	KindStart       = Kind{"start"}
	KindStop        = Kind{"stop"}
	KindRestart     = Kind{"restart"}
	KindDelete      = Kind{"delete"}
	KindWorldImport = Kind{"world_import"}
	// KindThunderstoreSync is the first global-scoped kind: it takes no
	// instance lock, is idempotent, and is one of the three kinds 12 §9.4 allows automatic
	// retry with backoff — a bare download-and-upsert touches no world and no container.
	KindThunderstoreSync = Kind{"thunderstore_sync"}
	// KindModInstall is instance-scoped and 12 §9.4's one "not resumed" kind: a crash is
	// rolled back from the file manifest rather than continued, because the manifest is
	// written before files move and is therefore exact where a half-applied tree is not.
	KindModInstall = Kind{"mod_install"}
	// KindModUninstall is instance-scoped and not cancellable at all (12 §3.1): it is seconds
	// of file removal driven by a manifest, and a crash rolls it back from what it saved before
	// removing anything.
	KindModUninstall = Kind{"mod_uninstall"}
	// KindBackup is instance-scoped and the one kind that may stop a running server as a step
	// (12 §3.2). Its quiesced path is the sequence in 12 §2.3, not a compound state; its hot
	// path enters no transient state at all (B12).
	KindBackup = Kind{"backup"}
	// KindRestore is instance-scoped, never cancellable (12 §8) and never resumed: it replaces
	// a world, so an interrupted run leaves on-disk state unproven and a human decides
	// (12 §9.3, ADR-032).
	KindRestore = Kind{"restore"}
	// KindPrune is global and idempotent: it applies each instance's retention to the archives
	// that already exist and holds no policy of its own (02 §4.4 step 7, 12 §9.4).
	KindPrune = Kind{"prune"}
)

// resumeIntentHonoured is ADR-032 / 12 §9.3: a resume intent is honoured only for kinds whose
// failure cannot leave world data half-written. backup qualifies, its archive being
// discardable and the world never touched; restore and game_update never will, since
// auto-starting a server whose world may be half-swapped turns a recoverable situation into
// an unrecoverable one.
var resumeIntentHonoured = map[Kind]bool{KindBackup: true}

// ResumeIntentHonoured reports whether a job of this kind may have its resume_after intent
// acted on after a crash (12 §9.1 step 4).
func ResumeIntentHonoured(k Kind) bool { return resumeIntentHonoured[k] }

// ByName resolves job_runs.kind back to the typed constant. A row whose kind no build
// recognises is reported as unknown rather than silently treated as one of the known kinds —
// the same closed-registry discipline the constants themselves enforce.
func ByName(name string) (Kind, bool) {
	for _, k := range []Kind{
		KindProvision, KindStart, KindStop, KindRestart, KindDelete, KindWorldImport,
		KindThunderstoreSync, KindModInstall, KindModUninstall, KindBackup, KindRestore, KindPrune,
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
