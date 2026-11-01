package backup

import (
	"os"
	"path/filepath"
	"testing"
)

// catalogue builds newest-first rows from a class sequence, "c" cold, "h" hot and "s" a safety
// snapshot, which is always consistent.
func catalogue(classes string) []Entry {
	out := make([]Entry, 0, len(classes))
	for i, c := range classes {
		out = append(out, Entry{
			ID:         string(c) + string(rune('0'+i)),
			Path:       "/backups/" + string(c) + string(rune('0'+i)) + ".tar.gz",
			Consistent: c != 'h',
			Snapshot:   c == 's',
		})
	}
	return out
}

func doomedIDs(archives []Entry) []string {
	out := make([]string, 0, len(archives))
	for _, a := range archives {
		out = append(out, a.ID)
	}
	return out
}

func TestPrune(t *testing.T) {
	for _, tc := range []struct {
		name    string
		classes string
		policy  Policy
		want    []string
	}{
		{"under the limit keeps everything", "cc", Policy{2, 5}, nil},
		{"the oldest cold archive goes", "cccc", Policy{2, 5}, []string{"c2", "c3"}},
		{"the oldest hot copy goes", "hhh", Policy{2, 2}, []string{"h2"}},
		{"zero keeps everything in that class", "cccc", Policy{0, 5}, nil},
		{"zero hot keeps every hot copy", "hhhh", Policy{2, 0}, nil},
		// Zero is "keep everything", not "the other class decides": a count of 0 in one class
		// must not read as a limit the other one is measured against.
		{"zero in one class does not touch the other", "cchh", Policy{0, 1}, []string{"h3"}},
		{"an empty catalogue prunes nothing", "", Policy{2, 5}, nil},
		{"a snapshot does not count against cold archives", "cscc", Policy{2, 5}, []string{"c3"}},
		{"snapshots are capped at the cold count", "ssscc", Policy{2, 5}, []string{"s2"}},
		{"zero cold keeps every snapshot", "sssc", Policy{0, 5}, nil},
		{"hot copies do not evict a snapshot", "hhs", Policy{2, 1}, []string{"h1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := doomedIDs(Prune(catalogue(tc.classes), tc.policy))
			if len(got) != len(tc.want) {
				t.Fatalf("pruned %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("pruned %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// Asserts hot copies are counted against their own limit and can never evict a cold archive.
// Under one shared count this fails: twenty hot copies would push both cold ones out and
// leave a full catalogue with nothing restorable (B12).
func TestPruneNeverLetsHotCopiesEvictAColdArchive(t *testing.T) {
	classes := "cc"
	for range 20 {
		classes += "h"
	}
	doomed := Prune(catalogue(classes), Policy{KeepCold: 2, KeepHot: 5})

	for _, a := range doomed {
		if a.Consistent {
			t.Errorf("prune removed cold archive %s while hot copies were over their limit", a.ID)
		}
	}
	if len(doomed) != 15 {
		t.Errorf("pruned %d archives, want the 15 hot copies over the limit of 5", len(doomed))
	}
}

// Asserts a class is counted by its own flag, not by position, when the two interleave.
func TestPruneCountsEachClassIndependentlyWhenInterleaved(t *testing.T) {
	// c0 h1 c2 h3 c4 h5, newest first: the third cold and the second and third hot are over.
	got := doomedIDs(Prune(catalogue("chchch"), Policy{KeepCold: 2, KeepHot: 1}))
	want := []string{"h3", "c4", "h5"}
	if len(got) != len(want) {
		t.Fatalf("pruned %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("pruned %v, want %v", got, want)
		}
	}
}

func TestPruneExcludesPinnedArchivesFromRetention(t *testing.T) {
	entries := catalogue("ccchh")
	entries[0].Pinned = true
	entries[3].Pinned = true

	got := doomedIDs(Prune(entries, Policy{KeepCold: 1, KeepHot: 1}))
	want := []string{"c2"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("pruned %v, want %v", got, want)
	}
}

func TestRemoveToleratesAnAbsentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone.tar.gz")
	if err := Remove(Entry{ID: "b-1", Path: path}); err != nil {
		t.Fatalf("Remove of an absent archive: %v", err)
	}

	if err := os.WriteFile(path, []byte("archive"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := Remove(Entry{ID: "b-1", Path: path}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("Remove left the archive on disk")
	}
}

// Asserts a world that broke and keeps being archived cannot push out the last archive whose
// world passed. Without the rule, five daily archives of a broken world at a count of three
// leave only broken worlds to restore.
func TestPruneKeepsTheLastArchiveWhoseWorldPassed(t *testing.T) {
	entries := catalogue("cccccc")
	for i := range 5 {
		entries[i].Flagged = true
	}

	got := doomedIDs(Prune(entries, Policy{KeepCold: 3, KeepHot: 5}))
	want := []string{"c3", "c4"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("pruned %v, want %v: the flagged c3 and c4 go, and c5 stays as the last that passed", got, want)
	}
}

// Asserts each class keeps its own last good archive, and a snapshot never counts as one.
func TestPruneKeepsTheLastGoodArchivePerClass(t *testing.T) {
	entries := catalogue("hhcchcs")
	for _, i := range []int{0, 1, 2, 3} {
		entries[i].Flagged = true
	}

	got := doomedIDs(Prune(entries, Policy{KeepCold: 1, KeepHot: 1}))
	want := []string{"h1", "c3"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("pruned %v, want %v: h4 and c5 are their classes' last good archives", got, want)
	}
}

// Asserts that once an operator acknowledges a flagged archive, it is the one protected.
func TestPruneMovesProtectionToAnAcknowledgedArchive(t *testing.T) {
	entries := catalogue("ccc")
	entries[1].Flagged = true

	got := doomedIDs(Prune(entries, Policy{KeepCold: 1, KeepHot: 5}))
	want := []string{"c1", "c2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("pruned %v, want %v: the unflagged newest archive is the last good one", got, want)
	}
}
