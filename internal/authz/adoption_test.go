package authz

import "testing"

func TestAdoptionActionIsAdminOnly(t *testing.T) {
	action, ok := ParseAction("instance.adopt")
	if !ok {
		t.Fatal("instance.adopt is absent from the action registry")
	}
	if Grantable(action) {
		t.Error("instance.adopt is grantable, want admin-only")
	}
}
