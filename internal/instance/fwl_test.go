package instance

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// buildFWL assembles a `.fwl` header in the layout 03 §4.2 measured, so a test can vary one
// field at a time. Static fixtures could not cover truncation and bad length prefixes
// without shipping a file per case, and every real world's header carries its seed and uid.
func buildFWL(version int32, name string, tail []byte) []byte {
	body := make([]byte, 4, 5+len(name)+len(tail))
	binary.LittleEndian.PutUint32(body, uint32(version))
	body = append(body, byte(len(name)))
	body = append(body, name...)
	body = append(body, tail...)

	out := make([]byte, 4, 4+len(body))
	binary.LittleEndian.PutUint32(out, uint32(len(body)))
	return append(out, body...)
}

// TestParseFWLReadsVersionAndName covers the two fields 03 §4.1 rules 3 and 4 need, at the
// three format versions actually measured.
func TestParseFWLReadsVersionAndName(t *testing.T) {
	for _, version := range []int32{33, 34, 37} {
		got, err := ParseFWL(buildFWL(version, "Dedicated", []byte{1, 2, 3, 4}))
		if err != nil {
			t.Fatalf("version %d: %v", version, err)
		}
		if got.Version != version || got.Name != "Dedicated" {
			t.Errorf("version %d parsed as %+v", version, got)
		}
	}
}

// TestParseFWLIgnoresEverythingAfterTheName is 03 §4.2's rule that the tail is
// version-dependent territory: two files identical up to the name must parse identically,
// however different their remainders.
func TestParseFWLIgnoresEverythingAfterTheName(t *testing.T) {
	a, errA := ParseFWL(buildFWL(37, "Same", []byte{9, 9, 9}))
	b, errB := ParseFWL(buildFWL(37, "Same", []byte(strings.Repeat("\xff", 120))))
	if errA != nil || errB != nil {
		t.Fatalf("errors: %v / %v", errA, errB)
	}
	if a != b {
		t.Errorf("tail changed the parse: %+v vs %+v", a, b)
	}
}

func TestParseFWLRejectsWhatIsNotAWorldFile(t *testing.T) {
	good := buildFWL(37, "Dedicated", []byte{1, 2, 3, 4})

	tests := map[string][]byte{
		"empty":                   {},
		"too short":               {1, 2, 3},
		"truncated mid-name":      good[:10],
		"length header disagrees": append(append([]byte{}, good[:4]...), append([]byte{}, good[4:len(good)-1]...)...),
		"version zero":            buildFWL(0, "Dedicated", nil),
		"negative version":        buildFWL(-1, "Dedicated", nil),
		"a .db file, not a .fwl":  []byte("this is world save data, not a header, and is much longer than nine bytes"),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseFWL(data); !errors.Is(err, ErrNotAWorldFile) {
				t.Errorf("ParseFWL = %v, want ErrNotAWorldFile", err)
			}
		})
	}
}

// TestParseFWLRejectsALengthPrefixThatRunsPastTheEnd is the hostile case: a name length
// that would read beyond the buffer. It must be refused rather than panic, because an
// imported `.fwl` is an untrusted file from a user's disk (03 §4.1 rule 5).
func TestParseFWLRejectsALengthPrefixThatRunsPastTheEnd(t *testing.T) {
	data := make([]byte, 4, 9)
	binary.LittleEndian.PutUint32(data, 5)
	body := make([]byte, 4)
	binary.LittleEndian.PutUint32(body, 37)
	data = append(data, body...)
	data = append(data, 200) // claims a 200-byte name in a file with none left

	if _, err := ParseFWL(data); !errors.Is(err, ErrNotAWorldFile) {
		t.Errorf("ParseFWL = %v, want ErrNotAWorldFile", err)
	}
}
