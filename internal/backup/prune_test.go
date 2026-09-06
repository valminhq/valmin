package backup

import (
	"os"
	"path/filepath"
	"testing"
)

// catalogue builds newest-first rows from a class sequence, "c" cold and "h" hot.
func catalogue(classes string) []Entry {
	out := make([]Entry, 0, len(classes))
	for i, c := range classes {
		out = append(out, Entry{
			ID:         string(c) + string(rune('0'+i)),
			Path:       "/backups/" + string(c) + string(rune('0'+i)) + ".tar.gz",
			Consistent: c == 'c',
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
		{"an empty catalogue prunes nothing", "", Policy{2, 5}, nil},
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
