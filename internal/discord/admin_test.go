package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// slash builds a /name interaction from member u-1 in the general channel, whose link covers
// Alpha (stopped) and Bravo (running).
func slash(name string, opts ...map[string]any) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"id": "i-1", "application_id": "app-1", "type": interactionCommand, "token": "itok",
		"guild_id": "g1", "channel_id": "general",
		"member": map[string]any{"user": map[string]any{"id": "u-1", "username": "den", "global_name": "Den"}},
		"data":   map[string]any{"name": name, "options": opts},
	})
	return raw
}

// opt is one option of an interaction.
func opt(name string, value any) map[string]any { return map[string]any{"name": name, "value": value} }

// sub is a subcommand option carrying opts.
func sub(name string, opts ...map[string]any) map[string]any {
	return map[string]any{"name": name, "type": 1, "options": opts}
}

// addJob records a job that has ended with status.
func (w *world) addJob(t *testing.T, id, status string) {
	t.Helper()
	if _, err := w.db.Writer.ExecContext(t.Context(), `INSERT INTO job_runs
		(id, kind, status, lock_key, instance_id, instance_name, created_at)
		VALUES (?, 'chain', ?, 'k', 'inst-b', 'inst-b', ?)`, id, status, store.Now()); err != nil {
		t.Fatal(err)
	}
}

// addTestCopy adds Bravo's update test copy in state.
func (w *world) addTestCopy(t *testing.T, state string) {
	t.Helper()
	if _, err := w.db.Writer.ExecContext(t.Context(), `INSERT INTO instances (
		id, name, state, container_id, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, test_of, created_at, updated_at
	) VALUES ('copy-b', 'bravo-test', ?, 'c-copy', '/srv/copy', 2600, 'Bravo test', 'World', 'env',
		'cp-copy', 'inst-b', ?, ?)`, state, store.Now(), store.Now()); err != nil {
		t.Fatal(err)
	}
}

// submitted counts every job the admin commands submitted.
func (w *world) submitted() int {
	return len(w.chainer.chains) + len(w.chainer.steps) + len(w.tester.tests) + len(w.tester.applies) +
		len(w.deleter.got)
}

// TestAdminCommandsAreForBotAdminsOnly asserts every admin command refuses a member who is not a
// bot admin and submits nothing.
func TestAdminCommandsAreForBotAdminsOnly(t *testing.T) {
	for _, in := range []json.RawMessage{
		slash("restart", opt("server", "Bravo")),
		slash("stop", opt("server", "Bravo")),
		slash("backup", opt("server", "Bravo")),
		slash("update", sub("test", opt("server", "Bravo"))),
		slash("update", sub("apply", opt("server", "Bravo"))),
		slash("update", sub("discard", opt("server", "Bravo"))),
	} {
		w := newWorld(t, testLinks)
		w.withAdmins(t, `["u-2"]`)
		w.bot.handle(t.Context(), in)
		got := w.api.answer(t)
		if content, _ := got.body["content"].(string); !strings.Contains(content, "Only the bot's admins") {
			t.Errorf("%s: content = %q", in, content)
		}
		if n := w.submitted(); n != 0 {
			t.Errorf("%s: %d jobs submitted", in, n)
		}
	}
}

// TestRestartAndStopWarnPlayersThenWait asserts a wait warns the players and waits for them to
// leave before the step, as one chain, no wait submits the step alone, and a backup waits only
// on a running server.
func TestRestartAndStopWarnPlayersThenWait(t *testing.T) {
	w := newWorld(t, testLinks)
	w.withAdmins(t, `["u-1"]`)
	w.bot.handle(t.Context(), slash("restart", opt("server", "Bravo")))
	if content := w.api.answer(t).body["content"]; content != "Restarting **Bravo** once all players have left, or in "+
		"5 minutes at the latest… Players get an in-game warning if the server has an RCON mod." {
		t.Errorf("reply = %v", content)
	}
	if len(w.chainer.chains) != 1 {
		t.Fatalf("chains = %d, want 1", len(w.chainer.chains))
	}
	chain := w.chainer.chains[0]
	types := make([]string, 0, len(chain.Steps))
	for _, s := range chain.Steps {
		types = append(types, s.Type)
	}
	if !slices.Equal(types, []string{control.StepCommand, control.StepWaitFor, control.StepRestart}) ||
		!strings.HasPrefix(chain.Steps[0].Command, "say The server restarts") ||
		chain.Steps[1].Condition != control.ConditionEmpty || chain.Steps[1].Seconds != 300 {
		t.Errorf("steps = %+v", chain.Steps)
	}
	if chain.Kind != jobs.KindChain || chain.From != instance.StateRunning || chain.Instance.ID != "inst-b" {
		t.Errorf("chain = %+v", chain)
	}
	if chain.Audit == nil || chain.Audit.Action != "instances.restart" ||
		chain.Audit.ActorName != "Discord: Den (u-1)" {
		t.Errorf("audit = %+v", chain.Audit)
	}

	w = newWorld(t, testLinks)
	w.withAdmins(t, `["u-1"]`)
	w.bot.handle(t.Context(), slash("stop", opt("server", "Bravo"), opt("wait", 0)))
	if content := w.api.answer(t).body["content"]; content != "Stopping **Bravo**…" {
		t.Errorf("reply = %v", content)
	}
	if !slices.Equal(w.chainer.steps, []string{"inst-b:stop"}) || len(w.chainer.chains) != 0 {
		t.Errorf("steps = %v, chains = %d", w.chainer.steps, len(w.chainer.chains))
	}

	w = newWorld(t, testLinks)
	w.withAdmins(t, `["u-1"]`)
	w.bot.handle(t.Context(), slash("backup", opt("server", "Bravo")))
	w.api.answer(t)
	if len(w.chainer.chains) != 1 {
		t.Fatalf("a running server's backup submitted %d chains, want 1", len(w.chainer.chains))
	}
	steps := w.chainer.chains[0].Steps
	if last := steps[len(steps)-1]; last.Type != control.StepBackup ||
		!strings.Contains(steps[0].Command, "restarts for a backup") ||
		w.chainer.chains[0].Audit.Action != "instances.backups.create" {
		t.Errorf("backup chain = %+v, audit %+v", steps, w.chainer.chains[0].Audit)
	}

	w = newWorld(t, testLinks)
	w.withAdmins(t, `["u-1"]`)
	w.bot.handle(t.Context(), slash("backup", opt("server", "Alpha")))
	if content := w.api.answer(t).body["content"]; content != "Backing up **Alpha**…" {
		t.Errorf("reply = %v", content)
	}
	if !slices.Equal(w.chainer.steps, []string{"inst-a:backup"}) {
		t.Errorf("a stopped server's backup: steps = %v", w.chainer.steps)
	}
}

// TestAdminCommandsRefuseWhatTheServerCannotDo asserts the state checks and the refusals of
// the update test reach Discord without a job.
func TestAdminCommandsRefuseWhatTheServerCannotDo(t *testing.T) {
	tests := []struct {
		name string
		in   json.RawMessage
		fail error
		want string
	}{
		{"restart a stopped server", slash("restart", opt("server", "Alpha")), nil, "**Alpha** isn't running."},
		{
			"a second test copy", slash("update", sub("test", opt("server", "Bravo"))),
			&control.UpdateTestExistsError{CopyID: "c"}, "already has an update test copy",
		},
		{
			"apply with no copy", slash("update", sub("apply", opt("server", "Bravo"))),
			control.ErrNoUpdateTest, "has no update test copy",
		},
		{
			"a busy server", slash("backup", opt("server", "Bravo")),
			&store.JobConflict{JobID: "j"}, "busy with another task",
		},
		{
			"discard with no copy",
			slash("update", sub("discard", opt("server", "Bravo"))),
			nil,
			"has no update test copy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, testLinks)
			w.withAdmins(t, `["u-1"]`)
			w.chainer.fail, w.tester.fail = tt.fail, tt.fail
			w.bot.handle(t.Context(), tt.in)
			got := w.api.answer(t)
			if content, _ := got.body["content"].(string); !strings.Contains(content, tt.want) {
				t.Errorf("content = %q, want it to contain %q", content, tt.want)
			}
			if _, ephemeral := got.body["flags"]; !ephemeral {
				t.Error("a refusal is not ephemeral")
			}
		})
	}
}

// TestUpdateTestTakesWhatAndApply asserts the what option chooses the game, the mods or both,
// and apply is passed on.
func TestUpdateTestTakesWhatAndApply(t *testing.T) {
	tests := []struct {
		what       string
		apply      bool
		game, mods bool
	}{
		{"", false, true, true},
		{"both", true, true, true},
		{"game", false, true, false},
		{"mods", true, false, true},
	}
	for _, tt := range tests {
		w := newWorld(t, testLinks)
		w.withAdmins(t, `["u-1"]`)
		opts := []map[string]any{opt("server", "Bravo"), opt("apply", tt.apply)}
		if tt.what != "" {
			opts = append(opts, opt("what", tt.what))
		}
		w.bot.handle(t.Context(), slash("update", sub("test", opts...)))
		w.api.answer(t)
		if len(w.tester.tests) != 1 {
			t.Fatalf("what %q: %d tests submitted", tt.what, len(w.tester.tests))
		}
		got := w.tester.tests[0]
		if got.Game != tt.game || got.Mods != tt.mods || got.Apply != tt.apply || got.Instance.ID != "inst-b" {
			t.Errorf("what %q apply %v: submission = %+v", tt.what, tt.apply, got)
		}
	}
}

// TestAnUpdateTestReportsHowTheModsLoaded asserts the finished test's reply says whether every
// mod on the copy loaded and what comes next.
func TestAnUpdateTestReportsHowTheModsLoaded(t *testing.T) {
	failed := instance.FailedPlugin{LoadedPlugin: instance.LoadedPlugin{Name: "Foo_Bar"}}
	loaded := &control.TestVerdict{Load: &instance.PluginLoad{Declared: -1, Complete: true}}
	newer := control.TestChanges{SourceBuildID: "100", CopyBuildID: "200"}
	tests := []struct {
		name    string
		apply   bool
		changes control.TestChanges
		verdict *control.TestVerdict
		want    string
	}{
		{
			"every mod loaded",
			false,
			newer,
			loaded,
			" Game build 100 → 200. Every mod loaded. `/update apply` installs them.",
		},
		{
			"every mod loaded, applying", true, newer, loaded,
			" Game build 100 → 200. Every mod loaded. The updates are being applied if nobody is playing; " +
				"otherwise `/update apply` installs them later.",
		},
		{
			"a mod failed",
			true,
			newer,
			&control.TestVerdict{
				Load:    &instance.PluginLoad{Declared: -1, Complete: true, Failed: []instance.FailedPlugin{failed}},
				NotSeen: []string{"Some_Mod"},
			},
			" Game build 100 → 200. Not every mod loaded. Failed: `Foo_Bar`. Not mentioned in the mod log: `Some_Mod`. " +
				"The updates were not applied. `/update apply` installs them anyway, or `/update discard` removes the copy.",
		},
		{
			"nothing newer", false,
			control.TestChanges{SourceBuildID: "200", CopyBuildID: "200"},
			loaded,
			" The copy runs nothing newer than the server. `/update discard` removes it.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, testLinks)
			w.withAdmins(t, `["u-1"]`)
			w.addTestCopy(t, "running")
			w.addJob(t, "job-1", jobs.StatusSucceeded)
			w.tester.changes, w.tester.verdict = tt.changes, tt.verdict
			w.bot.handle(t.Context(), slash("update", sub("test", opt("server", "Bravo"), opt("apply", tt.apply))))
			w.api.answer(t)
			edit := w.api.next(t, http.MethodPatch, "/webhooks/app-1/itok/messages/@original")
			if content := edit.body["content"]; content != "The update test of **Bravo** finished."+tt.want {
				t.Errorf("edited reply = %v", content)
			}
		})
	}
}

// TestDiscardingATestCopy asserts a stopped copy is deleted at once, and a running one is stopped
// first and deleted once the stop has finished.
func TestDiscardingATestCopy(t *testing.T) {
	w := newWorld(t, testLinks)
	w.withAdmins(t, `["u-1"]`)
	w.addTestCopy(t, "stopped")
	w.bot.handle(t.Context(), slash("update", sub("discard", opt("server", "Bravo"))))
	w.api.answer(t)
	if len(w.deleter.got) != 1 || w.deleter.got[0].Instance.ID != "copy-b" || w.deleter.got[0].KeepWorlds {
		t.Fatalf("deletes = %+v", w.deleter.got)
	}

	w = newWorld(t, testLinks)
	w.withAdmins(t, `["u-1"]`)
	w.addTestCopy(t, "running")
	w.addJob(t, "job-1", jobs.StatusSucceeded)
	w.addJob(t, "job-del", jobs.StatusSucceeded)
	w.bot.handle(t.Context(), slash("update", sub("discard", opt("server", "Bravo"))))
	if content := w.api.answer(t).body["content"]; content != "Stopping the update test copy of **Bravo** to discard it…" {
		t.Errorf("reply = %v", content)
	}
	edit := w.api.next(t, http.MethodPatch, "/webhooks/app-1/itok/messages/@original")
	if content := edit.body["content"]; content != "The update test copy of **Bravo** is discarded." {
		t.Errorf("edited reply = %v", content)
	}
	if !slices.Equal(w.chainer.steps, []string{"copy-b:stop"}) || len(w.deleter.got) != 1 {
		t.Errorf("steps = %v, deletes = %d", w.chainer.steps, len(w.deleter.got))
	}
}

// TestJoinSaysHowToJoin asserts /join shows the join code or address and the profile code, and
// says when there is neither.
func TestJoinSaysHowToJoin(t *testing.T) {
	tests := []struct {
		name      string
		crossplay bool
		info      JoinInfo
		want      string
	}{
		{
			"an address",
			false,
			JoinInfo{
				Address:  "play.example.com:2456",
				SteamURL: "steam://run/892970//+connect%20play.example.com:2456",
			},
			"**Alpha** — offline\nAddress: `play.example.com:2456`\nJoin through Steam: steam://run/892970//+connect%20play.example.com:2456",
		},
		{"a crossplay code", true, JoinInfo{Code: "123456"}, "**Alpha** — offline\nJoin code: `123456`"},
		{"crossplay before the code", true, JoinInfo{}, "Join code: shown once the server is online."},
		{"no join host", false, JoinInfo{}, "No join address is set."},
		{
			"a profile code", false,
			JoinInfo{Address: "1.2.3.4:2456", ProfileCode: "0193-abc", ProfileRegistry: "thunderstore"},
			"Mod profile code for Thunderstore: `0193-abc`",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, testLinks)
			if _, err := w.db.Writer.ExecContext(t.Context(),
				`UPDATE instances SET crossplay = ? WHERE id = 'inst-a'`, tt.crossplay); err != nil {
				t.Fatal(err)
			}
			w.bot.Join = func(context.Context, *store.Instance) JoinInfo { return tt.info }
			content, ephemeral := w.answer(t, slash("join", opt("server", "alpha")))
			if !strings.Contains(content, tt.want) || ephemeral {
				t.Errorf("content = %q (ephemeral %v), want it to contain %q", content, ephemeral, tt.want)
			}
		})
	}
}

// TestStatusShowsACrossplayJoinCode asserts a running crossplay server's line carries its code.
func TestStatusShowsACrossplayJoinCode(t *testing.T) {
	w := newWorld(t, testLinks)
	if _, err := w.db.Writer.ExecContext(
		t.Context(),
		`UPDATE instances SET crossplay = 1 WHERE id = 'inst-b'`,
	); err != nil {
		t.Fatal(err)
	}
	w.bot.JoinCode = func(id string) string { return "code-" + id }
	content, _ := w.answer(t, slash("status"))
	if !strings.Contains(content, "🟢 **Bravo** — online, 2 players, join code `code-inst-b`") ||
		strings.Contains(content, "code-inst-a") {
		t.Errorf("content = %q", content)
	}
}

// TestAutocompleteReadsInsideASubcommand asserts a server typed into /update test is completed.
func TestAutocompleteReadsInsideASubcommand(t *testing.T) {
	w := newWorld(t, testLinks)
	raw, _ := json.Marshal(map[string]any{
		"id": "i-1", "application_id": "app-1", "type": interactionAutocomplete, "token": "itok",
		"guild_id": "g1", "channel_id": "general",
		"data": map[string]any{"name": "update", "options": []map[string]any{
			{
				"name":    "test",
				"type":    1,
				"options": []map[string]any{{"name": "server", "value": "br", "focused": true}},
			},
		}},
	})
	w.bot.handle(t.Context(), raw)
	got := w.api.next(t, http.MethodPost, "/interactions/i-1/itok/callback")
	data, _ := got.body["data"].(map[string]any)
	choices, _ := data["choices"].([]any)
	if len(choices) != 1 || choices[0].(map[string]any)["name"] != "Bravo" {
		t.Errorf("choices = %v, want Bravo", choices)
	}
}
