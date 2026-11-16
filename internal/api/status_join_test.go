package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/store"
)

// TestThePublicPageShowsHowToJoinOnlyWhenAsked asserts the join address and its Steam link
// reach the public page only with status_show_join on, and never on a crossplay server.
func TestThePublicPageShowsHowToJoinOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	rt, db, admin := statusWorld(t)
	publishWith(t, rt, admin, map[string]any{"join_host": "play.example.com"})
	if got := publicFields(t, rt); got["join_address"] != nil || got["steam_join_url"] != nil {
		t.Fatalf("payload = %v, want no join details while the switch is off", got)
	}

	publishWith(t, rt, admin, map[string]any{"status_show_join": true})
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	addr := "play.example.com:" + strconv.Itoa(inst.BasePort)
	got := publicFields(t, rt)
	if got["join_address"] != addr {
		t.Errorf("join_address = %#v, want %q", got["join_address"], addr)
	}
	if got["steam_join_url"] != "steam://run/892970//+connect%20"+addr {
		t.Errorf("steam_join_url = %#v, want the link to %s", got["steam_join_url"], addr)
	}

	seed(t, db, `UPDATE instances SET crossplay = TRUE WHERE id = 'inst-a'`)
	if got := publicFields(t, rt); got["join_address"] != nil || got["steam_join_url"] != nil {
		t.Errorf("payload = %v, want no address on a crossplay server", got)
	}
}

// TestThePublicPageShowsTheCurrentProfileCode asserts the stored profile code reaches the
// public page only with status_show_modpack on and only while it matches the server's mods.
func TestThePublicPageShowsTheCurrentProfileCode(t *testing.T) {
	t.Parallel()
	rt, db, admin := statusWorld(t)
	save := func(digest string) {
		t.Helper()
		if err := db.SaveModpackCode(t.Context(), &store.ModpackCode{
			InstanceID: "inst-a", Code: profileKey, Registry: "thunderstore",
			ManifestSHA256: digest, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	save(manifestDigest(nil))
	publishWith(t, rt, admin, nil)
	if got := publicFields(t, rt); got["modpack_code"] != nil {
		t.Fatalf("modpack_code = %#v while the switch is off, want none", got["modpack_code"])
	}

	publishWith(t, rt, admin, map[string]any{"status_show_modpack": true})
	got := publicFields(t, rt)
	if got["modpack_code"] != profileKey || got["modpack_registry"] != "thunderstore" {
		t.Errorf("modpack = %#v from %#v, want the stored code", got["modpack_code"], got["modpack_registry"])
	}

	save("a digest of some other mod list")
	if got := publicFields(t, rt); got["modpack_code"] != nil {
		t.Errorf("modpack_code = %#v for mods that changed, want none", got["modpack_code"])
	}
}

// TestTheSwitchesDoNotPublish asserts an unpublished server with both switches on still answers
// the same 404 as one that does not exist.
func TestTheSwitchesDoNotPublish(t *testing.T) {
	t.Parallel()
	rt, _, admin := statusWorld(t)
	body := map[string]any{"join_host": "play.example.com", "status_show_join": true, "status_show_modpack": true}
	if rec := patchInstance(t, rt, admin, body); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	hidden, missing := public(rt, statusPath("inst-a")), public(rt, statusPath("inst-nope"))
	if hidden.Code != http.StatusNotFound || hidden.Code != missing.Code ||
		!reflect.DeepEqual(errorBody(t, hidden), errorBody(t, missing)) {
		t.Errorf("unpublished = %d %s, nonexistent = %d %s; want the same 404",
			hidden.Code, hidden.Body, missing.Code, missing.Body)
	}
}

// TestStatusShowSwitchesAreSettingsAndAudited asserts the switches need instance.settings and
// are recorded with the status page's other changes.
func TestStatusShowSwitchesAreSettingsAndAudited(t *testing.T) {
	t.Parallel()
	rt, db, admin, member := world(t)
	seed(t, db, `UPDATE instance_grants SET role = 'viewer', perms = '[]'
		WHERE user_id = 'u-member' AND instance_id = 'inst-a'`)
	if rec := patchInstance(t, rt, member, map[string]any{"status_show_join": true}); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer patch = %d, want 403 (%s)", rec.Code, rec.Body)
	}

	body := map[string]any{"status_show_join": true, "status_show_modpack": true}
	if rec := patchInstance(t, rt, admin, body); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	inst, err := db.InstanceByID(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if !inst.StatusShowJoin || !inst.StatusShowModpack || inst.RestartRequired {
		t.Errorf("show join %v, modpack %v, restart %v; want both on and no restart",
			inst.StatusShowJoin, inst.StatusShowModpack, inst.RestartRequired)
	}
	rows := auditRecordsFor(t, db, "instances.status.update")
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	want := `{"changes":[{"field":"status_show_join","from":false,"to":true},` +
		`{"field":"status_show_modpack","from":false,"to":true}]}`
	if got := deref(rows[0].Detail); got != want {
		t.Errorf("detail = %s\nwant     %s", got, want)
	}
}

// TestTheCurrentCodeFollowsTheMods asserts an uploaded profile code is served back until the
// client mod list changes, and that reading it never uploads.
func TestTheCurrentCodeFollowsTheMods(t *testing.T) {
	t.Parallel()
	rt, _, admin := exportWorld(t)
	ts := newProfileHostFake(t, http.StatusOK, `{"key":"`+profileKey+`"}`)
	rt.mods.Profiles = profileHosts{source.Thunderstore: ts.host}

	if current := currentCodeOf(t, rt, admin); current != nil {
		t.Fatalf("current = %+v before any upload, want null", current)
	}
	if rec := postExportCode(t, rt, admin); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	current := currentCodeOf(t, rt, admin)
	if current == nil || current.Code != profileKey || current.Registry != "thunderstore" {
		t.Fatalf("current = %+v, want the uploaded code", current)
	}

	if rec := patchMod(t, rt, admin, "OdinPlus-OdinArchitect",
		map[string]any{"side": "server_only"}); rec.Code != http.StatusOK {
		t.Fatalf("retagging = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if current := currentCodeOf(t, rt, admin); current != nil {
		t.Errorf("current = %+v after the client mods changed, want null", current)
	}
	if len(ts.uploads) != 1 {
		t.Errorf("registry uploads = %d, want 1", len(ts.uploads))
	}
}

// currentCodeOf is GET /instances/inst-a/mods/export/code as u.
func currentCodeOf(t *testing.T, rt *Server, u *store.User) *exportCodeView {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-a/mods/export/code", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("current code = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var view currentCodeView
	decodeInto(t, rec, &view)
	return view.Current
}
