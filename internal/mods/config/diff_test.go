package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func str(s string) *string { return &s }

// TestDiffReportsTheSettingsThatDiffer asserts changed, removed and added settings come back
// keyed Section.Key, that secret-looking keys carry no values, and that edits outside a value
// are not changes.
func TestDiffReportsTheSettingsThatDiffer(t *testing.T) {
	tests := []struct {
		name string
		want []Change
	}{
		{"settings", []Change{
			{Key: "General.Enabled", From: str("true"), To: str("false")},
			{Key: "General.Legacy", From: str("old")},
			{Key: "Logging.Console.Enabled", From: str("false"), To: str("true")},
			{Key: "General.Retries", To: str("3")},
		}},
		{"secrets", []Change{
			{Key: "Notifications.ApiKey", Secret: true},
			{Key: "Notifications.AdminPassword", Secret: true},
			{Key: "Notifications.Retries", From: str("3"), To: str("5")},
			{Key: "Notifications.RemovedSecret", Secret: true},
			{Key: "TokenBucket.Rate", From: str("1"), To: str("2")},
			{Key: "Notifications.API_KEY_Backup", Secret: true},
		}},
		{"formatting", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := readDiffFixture(t, tt.name+".before")
			after := readDiffFixture(t, tt.name+".after")
			if got := Diff(before, after); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Diff = %s\nwant   %s", describe(got), describe(tt.want))
			}
		})
	}
}

// TestDiffOfTheCorpusSeesExactlyTheSettingThatWasSet asserts, for every setting in the corpus,
// that an untouched file has no changes and rewriting one value has exactly one.
func TestDiffOfTheCorpusSeesExactlyTheSettingThatWasSet(t *testing.T) {
	for _, path := range corpusFiles(t) {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := Diff(raw, raw); len(got) != 0 {
				t.Fatalf("an unchanged file reports %s", describe(got))
			}
			for _, s := range Parse(raw).Settings() {
				doc := Parse(raw)
				if err := doc.Set(s.Section, s.Key, "SENTINEL"); err != nil {
					t.Fatalf("[%s] %s: %v", s.Section, s.Key, err)
				}
				got := Diff(raw, doc.Bytes())
				if len(got) != 1 {
					t.Fatalf("[%s] %s: %d changes, want 1: %s", s.Section, s.Key, len(got), describe(got))
				}
				if !got[0].Secret && (got[0].To == nil || *got[0].To != "SENTINEL") {
					t.Errorf("[%s] %s: change = %s", s.Section, s.Key, describe(got))
				}
			}
		})
	}
}

// TestSecretKeyIsCaseInsensitiveAndKeyOnly asserts the name test folds case and looks at the
// key alone, so a section called TokenBucket does not hide its settings.
func TestSecretKeyIsCaseInsensitiveAndKeyOnly(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"Password", true},
		{"ADMIN_PASSWD", true},
		{"ClientSecret", true},
		{"AuthToken", true},
		{"DiscordWebhookUrl", true},
		{"ApiKey", true},
		{"my_api_key", true},
		{"Credentials", true},
		{"Enabled", false},
		{"Rate", false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := secretKey(tt.key); got != tt.want {
				t.Errorf("secretKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func readDiffFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "diff", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// describe renders changes with their values dereferenced, so a failure is readable.
func describe(changes []Change) string {
	out := "["
	for i, c := range changes {
		if i > 0 {
			out += ", "
		}
		out += c.Key
		if c.Secret {
			out += "(secret)"
		}
		if c.From != nil {
			out += " from " + *c.From
		}
		if c.To != nil {
			out += " to " + *c.To
		}
	}
	return out + "]"
}
