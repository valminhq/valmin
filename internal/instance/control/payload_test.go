package control

import (
	"encoding/json"
	"testing"

	"github.com/valminhq/valmin/internal/mods/manager"
)

func TestPersistedJobPayloadJSON(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"provision", ProvisionPayload{StartAfterProvision: true}, `{"start_after_provision":true}`},
		{
			"clone",
			ClonePayload{SourceID: "src", ArchiveID: "arc", ArchivePath: "/tmp/arc"},
			`{"source_instance_id":"src","archive_id":"arc","archive_path":"/tmp/arc"}`,
		},
		{"backup", BackupPayload{Mode: "quiesced", Dest: "/tmp/arc"}, `{"mode":"quiesced","dest":"/tmp/arc"}`},
		{"restore", RestorePayload{BackupID: "arc"}, `{"backup_id":"arc"}`},
		{"game update", GameUpdatePayload{ConfirmModded: true}, `{"confirm_modded":true}`},
		{"delete", DeletePayload{KeepWorlds: true}, `{"keep_worlds":true}`},
		{
			"world import",
			WorldImportPayload{StagingDir: "/tmp/world", AllowBackupVariant: true},
			`{"staging_dir":"/tmp/world","allow_backup_variant":true}`,
		},
		{"world delete", WorldDeletePayload{World: "Main"}, `{"world":"Main"}`},
		{
			"setup",
			SetupJobPayload{SetupID: "s", StagingDir: "/tmp/setup", ETag: "tag", Name: "saved", BackupID: "arc"},
			`{"setup_id":"s","staging_dir":"/tmp/setup","etag":"tag","name":"saved","world_backup_id":"arc"}`,
		},
		{"config apply", ConfigApplyPayload{Files: 2}, `{"files":2}`},
		{"adoption", AdoptionPayload{ContainerID: "ctr"}, `{"container_id":"ctr"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("payload = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestPersistedOperationJSON(t *testing.T) {
	steps, err := json.Marshal([]OperationStep{{Kind: "provision"}, {Kind: "mod_install", Ref: "A-B", JobID: "job"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"kind":"provision"},{"kind":"mod_install","ref":"A-B","job_id":"job"}]`; string(steps) != want {
		t.Fatalf("steps = %s, want %s", steps, want)
	}
	plan, err := json.Marshal(
		OperationPlan{
			Mods:  []manager.PackageRequest{{FullName: "A-B", Version: "1", Source: "thunderstore"}},
			Start: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"mods":[{"full_name":"A-B","version":"1","source":"thunderstore"}],"start_after_provision":true}`; string(
		plan,
	) != want {
		t.Fatalf("plan = %s, want %s", plan, want)
	}
}
