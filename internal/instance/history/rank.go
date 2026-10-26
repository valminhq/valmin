package history

import (
	"cmp"
	"slices"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// A guessed name is believed once it has this many hits and at least dominance times the hits
// of any other account holding the same name. A bystander who joined alongside the owner
// collects hits too, so the margin is what separates the two.
const (
	minHits   = 3
	dominance = 2
)

// RankedName is one name of an account and how far to trust it.
type RankedName struct {
	Name string
	// Exact is set when the name was read on the same line as the platform id.
	Exact      bool
	Hits       int
	Likely     bool
	LastSeenAt time.Time
}

// Rank groups rows by platform id and marks the names to believe: every exact name, and a
// guessed one with enough hits that no other account comes close for the same name. Within an
// account the exact names come first, then the likely ones, then by hits.
func Rank(rows []store.PlayerName) map[string][]RankedName {
	out := map[string][]RankedName{}
	for i := range rows {
		r := &rows[i]
		out[r.PlatformID] = append(out[r.PlatformID], RankedName{
			Name: r.Name, Exact: r.Exact, Hits: r.Hits, LastSeenAt: r.LastSeenAt,
			Likely: r.Exact || (r.Hits >= minHits && r.Hits >= dominance*otherHits(rows, r)),
		})
	}
	for _, names := range out {
		slices.SortFunc(names, func(a, b RankedName) int {
			return cmp.Or(
				-cmp.Compare(boolInt(a.Exact), boolInt(b.Exact)),
				-cmp.Compare(boolInt(a.Likely), boolInt(b.Likely)),
				-cmp.Compare(a.Hits, b.Hits),
				cmp.Compare(a.Name, b.Name),
			)
		})
	}
	return out
}

// otherHits is the most hits any account other than r's holds for r's name. It scans every row
// per row: a friends' panel has hundreds of accounts, not millions.
func otherHits(rows []store.PlayerName, r *store.PlayerName) int {
	most := 0
	for i := range rows {
		if rows[i].Name == r.Name && rows[i].PlatformID != r.PlatformID {
			most = max(most, rows[i].Hits)
		}
	}
	return most
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
