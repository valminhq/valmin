package history

import (
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

func TestRankBelievesAnExactNameAndADominantGuess(t *testing.T) {
	tests := []struct {
		name string
		rows []store.PlayerName
		want map[string]bool // "account/name" -> likely
	}{
		{
			name: "an exact name is believed with no hits",
			rows: []store.PlayerName{{PlatformID: "S1", Name: "Troll", Exact: true}},
			want: map[string]bool{"S1/Troll": true},
		},
		{
			name: "too few hits",
			rows: []store.PlayerName{{PlatformID: "S1", Name: "Troll", Hits: 2}},
			want: map[string]bool{"S1/Troll": false},
		},
		{
			name: "enough hits and no rival",
			rows: []store.PlayerName{{PlatformID: "S1", Name: "Troll", Hits: 3}},
			want: map[string]bool{"S1/Troll": true},
		},
		{
			name: "two friends who always joined together stay ambiguous",
			rows: []store.PlayerName{
				{PlatformID: "S1", Name: "Troll", Hits: 5},
				{PlatformID: "S2", Name: "Troll", Hits: 5},
			},
			want: map[string]bool{"S1/Troll": false, "S2/Troll": false},
		},
		{
			name: "the owner outgrows a bystander",
			rows: []store.PlayerName{
				{PlatformID: "S1", Name: "Troll", Hits: 8},
				{PlatformID: "S2", Name: "Troll", Hits: 4},
			},
			want: map[string]bool{"S1/Troll": true, "S2/Troll": false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := map[string]bool{}
			for id, names := range Rank(tt.rows) {
				for _, n := range names {
					got[id+"/"+n.Name] = n.Likely
				}
			}
			for k, want := range tt.want {
				if got[k] != want {
					t.Errorf("%s likely = %v, want %v", k, got[k], want)
				}
			}
		})
	}
}

func TestRankListsTheNamesToBelieveFirst(t *testing.T) {
	ranked := Rank([]store.PlayerName{
		{PlatformID: "S1", Name: "Alt", Hits: 2},
		{PlatformID: "S1", Name: "Troll", Hits: 4},
		{PlatformID: "S1", Name: "Real", Exact: true},
	})["S1"]
	order := make([]string, 0, len(ranked))
	for _, n := range ranked {
		order = append(order, n.Name)
	}
	if want := []string{
		"Real",
		"Troll",
		"Alt",
	}; len(order) != 3 || order[0] != want[0] || order[1] != want[1] ||
		order[2] != want[2] {
		t.Errorf("order = %v, want %v", order, want)
	}
}
