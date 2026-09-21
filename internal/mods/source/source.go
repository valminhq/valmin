// Package source names the mod registries the panel reads from (03 §6.1).
//
// It is a leaf with no imports of its own, because store, api, config and the zip cache all
// key on a registry and none of them may import the others.
package source

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Source is a mod registry — a typed constant, exactly as jobs.Kind is: an unknown registry
// is a compile error, not a row that no sync ever refreshes. The unexported field closes the
// registry to this file.
//
// The zero Source is unspecified and means "no preference". It is never a column value: a
// store writer that receives one returns an error rather than persisting an empty name.
type Source struct{ name string }

// String is the wire form: the source column, the API's "source" field and the query
// parameter.
func (s Source) String() string { return s.name }

// MarshalJSON renders the source as its wire name.
func (s Source) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(s.name)), nil }

// UnmarshalJSON resolves a wire name back to its constant. Without it the unexported field
// would decode to the zero Source in silence, wherever a Source round-trips through JSON —
// the crash-recovery record of a replaced install among them — and a row would come back
// naming no registry at all.
//
// A JSON null is the zero Source, since an absent registry is how a record written before
// this field existed reads.
func (s *Source) UnmarshalJSON(b []byte) error {
	var name string
	if string(b) == "null" {
		*s = Source{}
		return nil
	}
	if err := json.Unmarshal(b, &name); err != nil {
		return fmt.Errorf("decode registry: %w", err)
	}
	if name == "" {
		*s = Source{}
		return nil
	}
	resolved, ok := ByName(name)
	if !ok {
		return fmt.Errorf("unknown registry %q", name)
	}
	*s = resolved
	return nil
}

var (
	// Thunderstore serves its community listing under /c/{community}/, and honours ETag.
	Thunderstore = Source{"thunderstore"}
	// Hexium is Thunderstore-compatible in its response shape but serves the listing at the
	// API root, and sends no cache validators at all (03 §6.1).
	Hexium = Source{"hexium"}
)

// order is every registry, in the order search results and sync runs visit them.
var order = []Source{Thunderstore, Hexium}

// All returns every registry the panel knows. The caller decides which of them are enabled.
func All() []Source { return append([]Source(nil), order...) }

// ByName resolves a persisted or requested name back to the typed constant. An empty or
// unrecognised name is reported as such rather than silently resolving to a registry, since
// the value arrives both from a query string and from a column.
func ByName(name string) (Source, bool) {
	for _, s := range order {
		if s.name == name {
			return s, true
		}
	}
	return Source{}, false
}
