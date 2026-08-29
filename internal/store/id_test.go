package store

import (
	"regexp"
	"sort"
	"testing"
	"time"
)

var uuidv7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestNewIDIsAUUIDv7 pins the version and variant nibbles. Getting them wrong produces a
// string that looks right in a log and is not a UUID.
func TestNewIDIsAUUIDv7(t *testing.T) {
	for range 100 {
		if id := NewID(); !uuidv7.MatchString(id) {
			t.Fatalf("NewID() = %q, want a UUIDv7", id)
		}
	}
}

func TestNewIDIsUnique(t *testing.T) {
	seen := make(map[string]bool, 10000)
	for range 10000 {
		id := NewID()
		if seen[id] {
			t.Fatalf("NewID() repeated %q", id)
		}
		seen[id] = true
	}
}

// TestNewIDSortsByTime is the property that makes UUIDv7 worth the arithmetic: ids created
// later sort later, so a cursor over them is a keyset over creation order (11 §4).
func TestNewIDSortsByTime(t *testing.T) {
	ids := make([]string, 0, 5)
	for range 5 {
		ids = append(ids, NewID())
		time.Sleep(2 * time.Millisecond)
	}

	if !sort.StringsAreSorted(ids) {
		t.Errorf("ids are not in creation order: %v", ids)
	}
}
