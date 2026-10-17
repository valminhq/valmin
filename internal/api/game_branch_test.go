package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

// observeBranches records an update check that saw public and default_old.
func observeBranches(t *testing.T, db *store.DB) {
	t.Helper()
	if err := db.KVSet(t.Context(), instance.PublicBuildKey, instance.PublicBuild{
		BuildID: "200",
		Branches: []instance.SteamBranch{
			{Name: "public", BuildID: "200"},
			{Name: "default_old", BuildID: "150", Description: "Previous stable"},
		},
		ObservedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestPatchGameBranch asserts which branch and password pairs PATCH accepts, that a refused
// pair leaves the row alone, and that an accepted password is stored sealed for the instance.
func TestPatchGameBranch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		body         map[string]any
		want         int
		wantBranch   string
		wantPassword string
	}{
		{"listed branch", map[string]any{"game_branch": "default_old"}, http.StatusOK, "default_old", ""},
		{
			"password branch",
			map[string]any{"game_branch": "public-test", "game_branch_password": "letmein"},
			http.StatusOK, "public-test", "letmein",
		},
		{"back to public", map[string]any{"game_branch": "public"}, http.StatusOK, "public", ""},
		{
			"unlisted without a password",
			map[string]any{"game_branch": "public-test"},
			http.StatusUnprocessableEntity,
			"public",
			"",
		},
		{
			"public with a password",
			map[string]any{"game_branch": "public", "game_branch_password": "x"},
			http.StatusUnprocessableEntity, "public", "",
		},
		{
			"a SteamCMD command as the name",
			map[string]any{"game_branch": "+quit"},
			http.StatusUnprocessableEntity,
			"public",
			"",
		},
		{
			"a flag as the password",
			map[string]any{"game_branch": "public-test", "game_branch_password": "-beta"},
			http.StatusUnprocessableEntity, "public", "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt, db, admin, _ := world(t)
			observeBranches(t, db)
			rec := patchInstance(t, rt, admin, tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
			inst, err := db.InstanceByID(t.Context(), "inst-a")
			if err != nil {
				t.Fatal(err)
			}
			if inst.GameBranch != tt.wantBranch {
				t.Errorf("game_branch = %q, want %q", inst.GameBranch, tt.wantBranch)
			}
			sealed, err := db.InstanceBranchPassword(t.Context(), "inst-a")
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if sealed != "" {
				plain, err := rt.instances.Keeper.Decrypt(
					crypto.PurposeBranchPassword, crypto.BranchPasswordLocation("inst-a"), sealed)
				if err != nil {
					t.Fatal(err)
				}
				got = string(plain)
			}
			if got != tt.wantPassword {
				t.Errorf("branch password = %q, want %q", got, tt.wantPassword)
			}
			if rec.Code == http.StatusOK {
				var body map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if _, leaked := body["game_branch_password"]; leaked {
					t.Error("the response carries the branch password")
				}
			}
		})
	}
}

// TestGameBranchNeedsTheRightToUpdate asserts a member with instance.settings cannot choose the
// branch, which decides what the game update installs.
func TestGameBranchNeedsTheRightToUpdate(t *testing.T) {
	t.Parallel()
	rt, db, _, member := world(t)
	observeBranches(t, db)
	seed(t, db, `UPDATE instance_grants SET role = 'viewer', perms = '["instance.settings"]'
		WHERE user_id = 'u-member' AND instance_id = 'inst-a'`)
	if rec := patchInstance(
		t,
		rt,
		member,
		map[string]any{"game_branch": "default_old"},
	); rec.Code != http.StatusForbidden {
		t.Fatalf("patch = %d, want 403 (%s)", rec.Code, rec.Body)
	}
}

// TestPatchGameBranchAuditsNoPassword asserts the audit entry names the branch change and
// whether a password is set, never the password.
func TestPatchGameBranchAuditsNoPassword(t *testing.T) {
	t.Parallel()
	rt, db, admin, _ := world(t)
	if rec := patchInstance(t, rt, admin, map[string]any{
		"game_branch": "public-test", "game_branch_password": "letmein",
	}); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d (%s)", rec.Code, rec.Body)
	}
	var detail string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT detail FROM audit_log WHERE action = 'instances.settings.update'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	want := `{"changes":[{"field":"game_branch","from":"public","to":"public-test"},` +
		`{"field":"game_branch_password_set","from":false,"to":true}]}`
	if detail != want {
		t.Fatalf("detail = %s\nwant %s", detail, want)
	}
}

// TestGameOptionsListObservedBranches asserts /game/options serves the last observed branch
// list, and an empty list before any check.
func TestGameOptionsListObservedBranches(t *testing.T) {
	t.Parallel()
	rt, db, admin, _ := world(t)
	read := func() []instance.SteamBranch {
		rec := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/game/options", http.NoBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("options = %d (%s)", rec.Code, rec.Body)
		}
		var body struct {
			Branches []instance.SteamBranch `json:"branches"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Branches
	}
	if got := read(); got == nil || len(got) != 0 {
		t.Fatalf("before a check: %+v", got)
	}
	observeBranches(t, db)
	if got := read(); len(got) != 2 || got[1].Name != "default_old" {
		t.Fatalf("after a check: %+v", got)
	}
}
