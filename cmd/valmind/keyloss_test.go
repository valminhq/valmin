package main

import (
	"bytes"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

func keyEnv(b byte) func(string) string {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, crypto.MasterKeyLen))
	return func(k string) string {
		if k == crypto.EnvMasterKey {
			return key
		}
		return ""
	}
}

func seedSealedInstance(t *testing.T, db *store.DB, k *crypto.Keeper, id string, port int) {
	t.Helper()
	sealed, err := k.Encrypt(crypto.PurposeInstancePassword, crypto.InstancePasswordLocation(id), []byte("old-pass"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer.ExecContext(t.Context(), `
		INSERT INTO instances (
			id, name, state, data_dir, base_port, server_name, world_name, password,
			crossplay_instance_id, created_at, updated_at
		) VALUES (?, ?, 'stopped', ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "inst-"+id, "/srv/valmin/instances/"+id, port, "Server "+id, "World"+id, sealed,
		"cp-"+id, store.Now(), store.Now()); err != nil {
		t.Fatal(err)
	}
}

// TestAcceptNewKey asserts the recovery resets an unreadable instance password, clears an
// unreadable RCON password, disables an unreadable webhook, keeps readable values, rewrites
// the key check, and changes nothing on a second run.
func TestAcceptNewKey(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, "sqlite", "file:"+filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db.Writer); err != nil {
		t.Fatal(err)
	}

	lost, err := crypto.Open(ctx, db, "", keyEnv(1))
	if err != nil {
		t.Fatal(err)
	}
	seedSealedInstance(t, db, lost, "lost", 2456)
	rcon, err := lost.Encrypt(crypto.PurposeRCONPassword, crypto.RCONPasswordLocation("lost"), []byte("r"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetInstanceRCON(ctx, "lost", 2455, rcon); err != nil {
		t.Fatal(err)
	}
	url, err := lost.Encrypt(crypto.PurposeWebhookURL, crypto.WebhookURLLocation("hook"), []byte("https://x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateWebhook(ctx, &store.Webhook{
		ID: "hook", Name: "Discord", Kind: "discord", URL: url, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	branch, err := lost.Encrypt(crypto.PurposeBranchPassword, crypto.BranchPasswordLocation("lost"), []byte("beta"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateInstanceBranch(ctx, "lost", "public-test", branch); err != nil {
		t.Fatal(err)
	}
	credentials, err := lost.Encrypt(crypto.PurposeRemoteBackup,
		crypto.Location{Table: "remote_backup_destinations", Column: "credentials", RowID: "dest"}, []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveRemoteDestination(ctx, &store.RemoteDestination{
		ID: "dest", Kind: "webdav", Enabled: true, Endpoint: "https://dav.example", Username: "u",
		Credentials: credentials,
	}, nil); err != nil {
		t.Fatal(err)
	}

	k, err := crypto.OpenForRecovery(ctx, db, "", keyEnv(2))
	if err != nil {
		t.Fatal(err)
	}
	seedSealedInstance(t, db, k, "kept", 2466)
	keptBefore, err := db.InstancePassword(ctx, "kept")
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := acceptNewKey(ctx, db, k, &out); err != nil {
		t.Fatalf("acceptNewKey: %v", err)
	}

	inst, err := db.InstanceByID(ctx, "lost")
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := db.InstancePassword(ctx, "lost")
	if err != nil {
		t.Fatal(err)
	}
	password, err := k.Decrypt(crypto.PurposeInstancePassword, crypto.InstancePasswordLocation("lost"), envelope)
	if err != nil {
		t.Fatalf("reset password does not open: %v", err)
	}
	if v := instance.ValidateLaunch(inst.ServerName, inst.WorldName, string(password)); len(v) > 0 {
		t.Errorf("reset password breaks launch rules: %v", v)
	}
	if !inst.RestartRequired {
		t.Error("restart_required not set after a password reset")
	}
	for _, s := range []string{"inst-lost", string(password), "Discord"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output does not mention %q:\n%s", s, out.String())
		}
	}
	if strings.Contains(out.String(), "inst-kept") {
		t.Errorf("output lists a readable server:\n%s", out.String())
	}
	if after, _ := db.InstancePassword(ctx, "kept"); after != keptBefore {
		t.Error("a readable password was rewritten")
	}
	if _, _, ok, err := db.InstanceRCON(ctx, "lost"); err != nil || ok {
		t.Errorf("unreadable RCON password kept: ok=%v err=%v", ok, err)
	}
	hook, err := db.WebhookByID(ctx, "hook")
	if err != nil {
		t.Fatal(err)
	}
	if hook.Enabled || hook.URL != "" {
		t.Errorf("webhook enabled=%v url=%q, want disabled and cleared", hook.Enabled, hook.URL)
	}
	if after, err := db.InstanceBranchPassword(ctx, "lost"); err != nil || after != "" {
		t.Errorf("unreadable branch password = %q (err %v), want cleared", after, err)
	}
	dest, err := db.RemoteDestinationByID(ctx, "dest")
	if err != nil {
		t.Fatal(err)
	}
	if dest.Enabled || dest.Credentials != "" {
		t.Errorf("destination enabled=%v credentials=%q, want disabled and cleared", dest.Enabled, dest.Credentials)
	}
	for _, s := range []string{"Steam branch passwords cleared", "Remote backup destination disabled"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output does not say %q:\n%s", s, out.String())
		}
	}
	if found, err := k.CheckKey(ctx, db); err != nil || !found {
		t.Errorf("key check after recovery: found=%v err=%v", found, err)
	}
	if _, err := crypto.Open(ctx, db, "", keyEnv(2)); err != nil {
		t.Errorf("Open under the accepted key: %v", err)
	}

	out.Reset()
	if err := acceptNewKey(ctx, db, k, &out); err != nil {
		t.Fatalf("second acceptNewKey: %v", err)
	}
	if again, _ := db.InstancePassword(ctx, "lost"); again != envelope {
		t.Error("a second run rewrote the password")
	}
	if !strings.Contains(out.String(), "already matches") {
		t.Errorf("second run output:\n%s", out.String())
	}
}

// TestEveryEncryptedColumnHasARecoveryRule asserts that key-loss recovery knows what to do with
// every sealed column, so a new one cannot be left unreadable after recovery.
func TestEveryEncryptedColumnHasARecoveryRule(t *testing.T) {
	for _, column := range store.EncryptedColumns() {
		if recoveries[column] == nil {
			t.Errorf("%s has no recovery rule", column)
		}
	}
}

// TestAStartWithoutAKeyCheckNeedsTheKeyToOpenAStoredSecret asserts that a database holding
// secrets but no key check refuses a key that cannot open them, and accepts the one that can.
func TestAStartWithoutAKeyCheckNeedsTheKeyToOpenAStoredSecret(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, "sqlite", "file:"+filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db.Writer); err != nil {
		t.Fatal(err)
	}
	right, err := crypto.Open(ctx, db, "", keyEnv(1))
	if err != nil {
		t.Fatal(err)
	}
	seedSealedInstance(t, db, right, "sealed", 2456)
	wrong, err := crypto.OpenForRecovery(ctx, db, "", keyEnv(2))
	if err != nil {
		t.Fatal(err)
	}

	if err := opensAStoredSecret(db)(ctx, wrong); err == nil {
		t.Error("a key that cannot open the stored secret was accepted")
	}
	if err := opensAStoredSecret(db)(ctx, right); err != nil {
		t.Errorf("the key that sealed the secret was refused: %v", err)
	}
}
