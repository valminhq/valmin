package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

const discordPath = "/api/v1/admin/discord"

// discordWorld is provisionWorld, whose body limit fits a settings document, with inst-a.
func discordWorld(t *testing.T) (rt *Server, db *store.DB, admin, member *store.User) {
	t.Helper()
	rt, db, admin, member = provisionWorld(t)
	seed(t, db, `INSERT INTO instances (
		id, name, state, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES ('inst-a', 'inst-a', 'stopped', '/srv/inst-a', 2456, 'Server', 'World', 'env', 'cp-a', ?, ?)`,
		store.Now(), store.Now())
	return rt, db, admin, member
}

func putDiscord(t *testing.T, rt *Server, u *store.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, discordPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return as(rt, u, req)
}

// TestTheDiscordTokenNeverLeavesThePanel asserts the token is stored sealed, appears in no
// response, and that a save without a token keeps the stored one.
func TestTheDiscordTokenNeverLeavesThePanel(t *testing.T) {
	rt, db, admin, _ := discordWorld(t)
	const secret = "MTIz.s3cr3t-bot-token"
	rec := putDiscord(t, rt, admin, `{"token":"`+secret+`","enabled":true,"links":[
		{"guild_id":"123456789012345678","channel_id":"","allow_start":true,"instance_ids":["inst-a"]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "s3cr3t") {
		t.Errorf("response carries the token: %s", rec.Body)
	}
	first, err := db.DiscordBot(t.Context())
	if err != nil || first == nil || !strings.HasPrefix(first.Token, "v1.") || strings.Contains(first.Token, "s3cr3t") {
		t.Fatalf("stored bot = %+v, %v", first, err)
	}

	rec = putDiscord(t, rt, admin, `{"enabled":true,"links":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("second put = %d %s", rec.Code, rec.Body)
	}
	second, err := db.DiscordBot(t.Context())
	if err != nil || second.Token != first.Token || second.ID != first.ID {
		t.Errorf("a save without a token changed it: %+v", second)
	}

	get := as(rt, admin, httptest.NewRequest(http.MethodGet, discordPath, http.NoBody))
	if get.Code != http.StatusOK || strings.Contains(get.Body.String(), "s3cr3t") ||
		!strings.Contains(get.Body.String(), `"configured":true`) {
		t.Errorf("get = %d %s", get.Code, get.Body)
	}
	if rows := auditRecordsFor(t, db, "panel.settings"); len(rows) != 2 {
		t.Errorf("audit rows = %d, want 2", len(rows))
	}
}

// TestDiscordSettingsRejectBadLinks asserts each malformed link names its field and stores nothing.
func TestDiscordSettingsRejectBadLinks(t *testing.T) {
	const guild = "123456789012345678"
	tests := []struct {
		name, body, field string
	}{
		{"enabled without a token", `{"enabled":true,"links":[]}`, "token"},
		{
			"a guild id that is not one", `{"token":"t","enabled":true,"links":[{"guild_id":"abc","instance_ids":[]}]}`,
			"links[0].guild_id",
		},
		{"a channel id that is not one", `{"token":"t","enabled":true,"links":[{"guild_id":"` + guild +
			`","channel_id":"12","instance_ids":[]}]}`, "links[0].channel_id"},
		{"the same channel twice", `{"token":"t","enabled":true,"links":[{"guild_id":"` + guild +
			`","instance_ids":[]},{"guild_id":"` + guild + `","instance_ids":[]}]}`, "links[1]"},
		{"a server that does not exist", `{"token":"t","enabled":true,"links":[{"guild_id":"` + guild +
			`","instance_ids":["nope"]}]}`, "links[0].instance_ids"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, db, admin, _ := discordWorld(t)
			rec := putDiscord(t, rt, admin, tt.body)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"`+tt.field+`"`) {
				t.Fatalf("put = %d %s, want 422 naming %s", rec.Code, rec.Body, tt.field)
			}
			if bot, err := db.DiscordBot(t.Context()); err != nil || bot != nil {
				t.Errorf("a refused save stored %+v", bot)
			}
		})
	}
}

// TestDiscordSettingsAreInvisibleToAMember asserts every route answers a member 404.
func TestDiscordSettingsAreInvisibleToAMember(t *testing.T) {
	rt, _, _, member := discordWorld(t)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, discordPath, strings.NewReader(`{"enabled":false,"links":[]}`))
		req.Header.Set("Content-Type", "application/json")
		if rec := as(rt, member, req); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", method, rec.Code)
		}
	}
}

// TestDeletingTheDiscordBotRemovesItsLinks asserts DELETE clears the token and every link.
func TestDeletingTheDiscordBotRemovesItsLinks(t *testing.T) {
	rt, db, admin, _ := discordWorld(t)
	if rec := putDiscord(t, rt, admin, `{"token":"t","enabled":false,"links":[
		{"guild_id":"123456789012345678","instance_ids":["inst-a"]}]}`); rec.Code != http.StatusOK {
		t.Fatalf("put = %d %s", rec.Code, rec.Body)
	}
	if rec := as(
		rt,
		admin,
		httptest.NewRequest(http.MethodDelete, discordPath, http.NoBody),
	); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	bot, err := db.DiscordBot(t.Context())
	links, lerr := db.DiscordLinks(t.Context())
	if err != nil || lerr != nil || bot != nil || len(links) != 0 {
		t.Errorf("after delete: bot %+v, links %+v", bot, links)
	}
}
