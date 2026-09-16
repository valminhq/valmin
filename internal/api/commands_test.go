package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/command"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
)

func prepareRCONInstance(t *testing.T, rt *Router, db *store.DB) {
	t.Helper()
	fake := rt.Supervisor().inst.Runtime.(*runtime.Fake)
	containerID, err := fake.Create(t.Context(), &runtime.ContainerSpec{
		Name: "rcon-test", Image: "test", User: "10000:10000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Start(t.Context(), containerID); err != nil {
		t.Fatal(err)
	}
	fake.Get(containerID).NetworkAddresses = []string{"127.0.0.1"}
	dataDir := t.TempDir()
	configDir := filepath.Join(dataDir, "server", "BepInEx", "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "org.tristan.rcon.cfg"), []byte(
		"[1. Rcon]\nPort = 2455\nPassword = secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seed(t, db, `UPDATE instances SET state = 'running', container_id = ?, data_dir = ? WHERE id = 'inst-a'`,
		containerID, dataDir)
	seed(t, db, `INSERT INTO instance_mods (
		instance_id, full_name, version, installed_as, side, enabled, file_manifest, installed_at
	) VALUES ('inst-a', ?, '1.6.2', 'explicit', 'server_only', TRUE, '[]', ?)`,
		command.ValheimRCONPackage, store.Now())
}

func TestCapabilitiesAdvertiseInstalledRCON(t *testing.T) {
	rt, db, admin, _ := world(t)
	prepareRCONInstance(t, rt, db)

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet,
		"/api/v1/instances/inst-a/capabilities", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body)
	}
	got := decodeCapabilities(t, rec)
	if got.CommandChannel != "rcon" {
		t.Errorf("command_channel = %q, want rcon", got.CommandChannel)
	}
}

func TestCommandUsesRCONAndWritesAudit(t *testing.T) {
	rt, db, admin, member := world(t)
	prepareRCONInstance(t, rt, db)

	viewerRequest := httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/commands",
		bytes.NewBufferString(`{"command":"save"}`))
	viewerRequest.Header.Set("Content-Type", "application/json")
	if rec := as(rt, member, viewerRequest); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer status = %d (%s), want 403", rec.Code, rec.Body)
	}

	rt.Supervisor().inst.Commands.Dial = func(_ context.Context, _, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		go serveRCONTestConnection(server)
		return client, nil
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/instances/inst-a/commands",
		bytes.NewBufferString(`{"command":"save"}`))
	request.Header.Set("Content-Type", "application/json")
	rec := as(rt, admin, request)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d (%s), want 200", rec.Code, rec.Body)
	}
	var response commandResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Accepted || response.Output != "World saved" {
		t.Errorf("response = %+v", response)
	}
	rows, err := db.ListAuditLog(t.Context(), store.AuditFilter{
		InstanceID: "inst-a", Action: "instances.commands.send",
	}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Detail == nil || !bytes.Contains([]byte(*rows[0].Detail), []byte("save")) {
		t.Errorf("command audit rows = %+v", rows)
	}
}

func serveRCONTestConnection(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var size [4]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return
	}
	login := make([]byte, binary.LittleEndian.Uint32(size[:]))
	if _, err := io.ReadFull(conn, login); err != nil {
		return
	}
	_ = writeTestRCONPacket(conn, 1, "Login success")
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return
	}
	cmd := make([]byte, binary.LittleEndian.Uint32(size[:]))
	if _, err := io.ReadFull(conn, cmd); err != nil {
		return
	}
	_ = writeTestRCONPacket(conn, 2, "World saved")
}

func writeTestRCONPacket(conn net.Conn, id int32, body string) error {
	length := 10 + len(body)
	raw := make([]byte, 4+length)
	binary.LittleEndian.PutUint32(raw[0:4], uint32(length))
	binary.LittleEndian.PutUint32(raw[4:8], uint32(id))
	binary.LittleEndian.PutUint32(raw[8:12], 2)
	copy(raw[12:], body)
	_, err := conn.Write(raw)
	return err
}
