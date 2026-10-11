package manager

import (
	"encoding/json"
	"testing"
)

func TestPersistedModJobPayloadJSON(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{
			"install",
			InstallPayload{StagingDir: "/tmp/mod", FullName: "A-B", Version: "1", Source: "thunderstore"},
			`{"staging_dir":"/tmp/mod","full_name":"A-B","version":"1","source":"thunderstore"}`,
		},
		{
			"update",
			InstallPayload{
				Updates: []UpdateTarget{{FullName: "A-B", Source: "thunderstore", FromVersion: "1", Version: "2"}},
				Backup:  true,
			},
			`{"staging_dir":"","full_name":"","version":"","source":"","updates":[{"full_name":"A-B","source":"thunderstore","from_version":"1","version":"2"}],"backup":true}`,
		},
		{
			"uninstall",
			UninstallPayload{StagingDir: "/tmp/mod", FullNames: []string{"A-B"}},
			`{"staging_dir":"/tmp/mod","full_names":["A-B"]}`,
		},
		{"toggle", TogglePayload{FullName: "A-B", Enable: true}, `{"full_name":"A-B","enable":true}`},
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
