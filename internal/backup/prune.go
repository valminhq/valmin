package backup

import (
	"fmt"
	"os"
)

// Entry is one catalogue row prune needs to judge, decoupled from store.Backup so this
// package stays free of the store and of any transaction.
type Entry struct {
	ID string
	// Path is the archive on disk, from the catalogue row and never from a request (D13).
	Path string
	// Consistent is false for a hot copy, which is counted against its own retention floor
	// (B12).
	Consistent bool
}

// Policy is one instance's retention, in archives kept per class. Zero keeps everything in
// that class: there is no spelling of "delete them all" (02 §4.4 step 7).
type Policy struct {
	KeepCold int
	KeepHot  int
}

// Prune reports which of archives fall outside policy, newest first being kept. The caller
// passes them in catalogue order — newest first — and deletes the returned rows.
//
// Cold and hot are counted independently. Sharing one count would let a burst of cheap hot
// copies evict every quiesced archive, leaving a full catalogue with nothing restorable.
func Prune(archives []Entry, policy Policy) []Entry {
	var doomed []Entry
	cold, hot := 0, 0
	for _, a := range archives {
		kept, limit := &cold, policy.KeepCold
		if !a.Consistent {
			kept, limit = &hot, policy.KeepHot
		}
		if limit <= 0 {
			continue
		}
		if *kept < limit {
			*kept++
			continue
		}
		doomed = append(doomed, a)
	}
	return doomed
}

// Remove unlinks one pruned archive. An already-absent file is not an error: the row is what
// the caller deletes next, and refusing to remove a row because its file went first would
// leave an entry nothing can clear.
func Remove(a Entry) error {
	if err := os.Remove(a.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove archive %s: %w", a.ID, err)
	}
	return nil
}
