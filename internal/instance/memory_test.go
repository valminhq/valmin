package instance

import (
	"strings"
	"testing"
)

func TestParseAvailableMemoryMB(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{"typical", "MemTotal:       16384000 kB\nMemFree:  100 kB\nMemAvailable:    8388608 kB\n", 8192, false},
		{"missing", "MemTotal: 16384000 kB\nMemFree: 100 kB\n", 0, true},
		{"wrong unit", "MemAvailable: 8 MB\n", 0, true},
		{"not a number", "MemAvailable: lots kB\n", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseAvailableMemoryMB(strings.NewReader(tt.input))
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("parseAvailableMemoryMB(%q) = %d, %v; want %d, error %v",
					tt.input, got, err, tt.want, tt.wantErr)
			}
		})
	}
}
