package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFile creates a file under t.TempDir and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// env builds a getenv func over a map.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// minimalYAML satisfies the two required keys so tests can focus on one thing at a time.
const minimalYAML = `
server:
  external_url: https://valmin.example
data:
  host_root: /srv/valmin
`

// listenYAML is minimalYAML with server.listen set, written out in full rather than
// concatenated so the key lands under server rather than data.
const listenYAML = `
server:
  external_url: https://valmin.example
  listen: ":9000"
data:
  host_root: /srv/valmin
`

const relativeHostRootYAML = `
server:
  external_url: https://valmin.example
data:
  host_root: srv/valmin
`

func loadWith(t *testing.T, yaml string, vars map[string]string, args ...string) (*Config, error) {
	t.Helper()
	path := writeFile(t, "config.yaml", yaml)
	if vars == nil {
		vars = map[string]string{}
	}
	vars["VALMIN_CONFIG"] = path
	return Load(args, env(vars))
}

// TestPrecedence covers 10 §1: flag beats env, env beats file, file beats default.
func TestPrecedence(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		vars map[string]string
		args []string
		want string
	}{
		{
			name: "built-in default",
			yaml: minimalYAML,
			want: ":8080",
		},
		{
			name: "file beats default",
			yaml: listenYAML,
			want: ":9000",
		},
		{
			name: "env beats file",
			yaml: listenYAML,
			vars: map[string]string{"VALMIN_SERVER_LISTEN": ":9100"},
			want: ":9100",
		},
		{
			name: "flag beats env",
			yaml: listenYAML,
			vars: map[string]string{"VALMIN_SERVER_LISTEN": ":9100"},
			args: []string{"--server.listen", ":9200"},
			want: ":9200",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadWith(t, tt.yaml, tt.vars, tt.args...)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Server.Listen != tt.want {
				t.Errorf("server.listen = %q, want %q", cfg.Server.Listen, tt.want)
			}
		})
	}
}

// TestRequiredKeys covers the two keys 10 §1.1 marks required.
func TestRequiredKeys(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "missing host_root",
			yaml:    "server:\n  external_url: https://valmin.example\n",
			wantErr: "data.host_root is required",
		},
		{
			name:    "missing external_url",
			yaml:    "data:\n  host_root: /srv/valmin\n",
			wantErr: "server.external_url is required",
		},
		{
			name:    "relative host_root",
			yaml:    relativeHostRootYAML,
			wantErr: "must be an absolute path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadWith(t, tt.yaml, nil)
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestSecretsInConfigFileAreRefused covers 10 §1.1: refused, not warned about.
func TestSecretsInConfigFileAreRefused(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "master key env var in the file",
			yaml:    minimalYAML + "\nVALMIN_MASTER_KEY: aGVsbG8gd29ybGQ=\n",
			wantErr: "VALMIN_MASTER_KEY",
		},
		{
			name: "dsn with a keyword password",
			yaml: minimalYAML + "\ndb:\n  driver: postgres\n" +
				"  dsn: host=db user=valmin password=hunter2 dbname=valmin\n",
			wantErr: "db.dsn carries a password",
		},
		{
			name: "dsn with a url password",
			yaml: minimalYAML + "\ndb:\n  driver: postgres\n" +
				"  dsn: postgres://valmin:hunter2@db:5432/valmin\n",
			wantErr: "db.dsn carries a password",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadWith(t, tt.yaml, nil)
			if err == nil {
				t.Fatal("Load succeeded, want a refusal")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tt.wantErr)
			}
		})
	}
}

// TestFileSuffix covers the _FILE convention that makes Docker and Podman secrets work
// without an entrypoint shim (10 §1.1).
func TestFileSuffix(t *testing.T) {
	secret := writeFile(t, "dsn", "postgres://valmin@db:5432/valmin\n")

	t.Run("resolves to the file contents", func(t *testing.T) {
		cfg, err := loadWith(t, minimalYAML, map[string]string{"VALMIN_DB_DSN_FILE": secret})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := "postgres://valmin@db:5432/valmin"; cfg.DB.DSN != want {
			t.Errorf("db.dsn = %q, want %q (trailing newline trimmed)", cfg.DB.DSN, want)
		}
	})

	t.Run("both forms set is an error", func(t *testing.T) {
		_, err := loadWith(t, minimalYAML, map[string]string{
			"VALMIN_DB_DSN":      "file:/tmp/x.db",
			"VALMIN_DB_DSN_FILE": secret,
		})
		if err == nil || !strings.Contains(err.Error(), "both set") {
			t.Errorf("error = %v, want a both-set refusal", err)
		}
	})
}

// TestStopTimeoutFloor covers INVARIANTS B1 / ADR-008. The measured 3-5 s stop is
// shutdown overhead, not flush time, and does not license lowering the floor.
func TestStopTimeoutFloor(t *testing.T) {
	t.Run("below the floor is refused", func(t *testing.T) {
		_, err := loadWith(t, minimalYAML, map[string]string{"VALMIN_GAME_STOP_TIMEOUT": "30s"})
		if err == nil || !strings.Contains(err.Error(), "floor is 2m0s") {
			t.Errorf("error = %v, want a stop-timeout floor refusal", err)
		}
	})

	t.Run("at the floor is accepted", func(t *testing.T) {
		cfg, err := loadWith(t, minimalYAML, map[string]string{"VALMIN_GAME_STOP_TIMEOUT": "120s"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Game.StopTimeout.Std() != MinStopTimeout {
			t.Errorf("game.stop_timeout = %s, want %s", cfg.Game.StopTimeout, MinStopTimeout)
		}
	})
}

// TestEveryKeyHasAnEnvName is the structural half of 10 §1's "every key has an env
// equivalent". Deriving the name rather than tabulating it is what keeps this true.
func TestEveryKeyHasAnEnvName(t *testing.T) {
	cfg := Defaults()
	seen := map[string]string{}

	got := fields(&cfg)
	if len(got) < 25 {
		t.Errorf("walked %d leaf keys, expected the whole 10 §1.1 table", len(got))
	}
	for _, f := range got {
		if !strings.HasPrefix(f.env, envPrefix) {
			t.Errorf("%s: env name %q lacks the %s prefix", f.path, f.env, envPrefix)
		}
		if prev, dup := seen[f.env]; dup {
			t.Errorf("%s and %s both map to %s", prev, f.path, f.env)
		}
		seen[f.env] = f.path
	}

	// The mapping rule of 10 §1: server.listen -> VALMIN_SERVER_LISTEN.
	if seen["VALMIN_SERVER_LISTEN"] != "server.listen" {
		t.Errorf("VALMIN_SERVER_LISTEN maps to %q, want server.listen", seen["VALMIN_SERVER_LISTEN"])
	}
	if seen["VALMIN_DATA_HOST_ROOT"] != "data.host_root" {
		t.Errorf("VALMIN_DATA_HOST_ROOT maps to %q, want data.host_root", seen["VALMIN_DATA_HOST_ROOT"])
	}
}

// TestParseDuration covers the day suffix, which time.ParseDuration has no unit for but
// 10 §1.1 uses for auth.session_absolute_ttl.
func TestParseDuration(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "30s", want: 30 * time.Second},
		{in: "168h", want: 168 * time.Hour},
		{in: "30d", want: 30 * 24 * time.Hour},
		{in: "1d", want: 24 * time.Hour},
		{in: "1h30m", want: 90 * time.Minute},
		{in: "banana", wantErr: true},
		{in: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseDuration(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseDuration(%q) = %s, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDuration(%q): %v", tt.in, err)
			}
			if got.Std() != tt.want {
				t.Errorf("ParseDuration(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

// TestDerivedDefaults covers the two defaults that reference data.root and therefore
// cannot be resolved until the whole precedence chain has run (10 §1.1).
func TestDerivedDefaults(t *testing.T) {
	cfg, err := loadWith(t, minimalYAML, map[string]string{"VALMIN_DATA_ROOT": "/data/valmin"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := "file:/data/valmin/panel.db"; cfg.DB.DSN != want {
		t.Errorf("db.dsn = %q, want %q", cfg.DB.DSN, want)
	}
	if want := "/data/valmin/secret.key"; cfg.Secrets.MasterKeyFile != want {
		t.Errorf("secrets.master_key_file = %q, want %q", cfg.Secrets.MasterKeyFile, want)
	}
}

// TestMissingConfigFile distinguishes an absent default path, which is fine because the
// shipped deployment speaks env, from an explicitly named file that is not there.
func TestMissingConfigFile(t *testing.T) {
	t.Run("absent default path is not an error", func(t *testing.T) {
		_, err := Load(nil, env(map[string]string{
			"VALMIN_SERVER_EXTERNAL_URL": "https://valmin.example",
			"VALMIN_DATA_HOST_ROOT":      "/srv/valmin",
		}))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
	})

	t.Run("explicit missing file is an error", func(t *testing.T) {
		_, err := Load([]string{"--config", "/nonexistent/valmin.yaml"}, env(nil))
		if err == nil || !strings.Contains(err.Error(), "/nonexistent/valmin.yaml") {
			t.Errorf("error = %v, want it to name the missing file", err)
		}
	})
}

// TestTrustedProxiesValidation covers 10 §5: the list is CIDR prefixes and empty by
// default, so a malformed entry must not silently disable the check.
func TestTrustedProxiesValidation(t *testing.T) {
	t.Run("empty by default", func(t *testing.T) {
		cfg, err := loadWith(t, minimalYAML, nil)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Server.TrustedProxies) != 0 {
			t.Errorf("server.trusted_proxies = %v, want empty (ADR-025)", cfg.Server.TrustedProxies)
		}
	})

	t.Run("malformed CIDR is refused", func(t *testing.T) {
		_, err := loadWith(t, minimalYAML, map[string]string{
			"VALMIN_SERVER_TRUSTED_PROXIES": "10.0.0.0/8,not-a-cidr",
		})
		if err == nil || !strings.Contains(err.Error(), "not a CIDR prefix") {
			t.Errorf("error = %v, want a CIDR refusal", err)
		}
	})

	t.Run("comma-separated list parses", func(t *testing.T) {
		cfg, err := loadWith(t, minimalYAML, map[string]string{
			"VALMIN_SERVER_TRUSTED_PROXIES": "10.0.0.0/8, 172.16.0.0/12",
		})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Server.TrustedProxies) != 2 {
			t.Errorf("server.trusted_proxies = %v, want two entries", cfg.Server.TrustedProxies)
		}
	})
}
