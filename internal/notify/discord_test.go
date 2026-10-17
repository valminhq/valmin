package notify

import "testing"

func TestDiscordURLsKeepTheThreadAndAddTheirOwnParts(t *testing.T) {
	tests := []struct {
		name string
		got  func() (string, error)
		want string
	}{
		{
			"post", func() (string, error) { return DiscordPostURL("https://discord.com/api/webhooks/1/tok") },
			"https://discord.com/api/webhooks/1/tok?wait=true",
		},
		{
			"post into a thread",
			func() (string, error) { return DiscordPostURL("https://discord.com/api/webhooks/1/tok?thread_id=9") },
			"https://discord.com/api/webhooks/1/tok?thread_id=9&wait=true",
		},
		{
			"edit in a thread",
			func() (string, error) {
				return DiscordEditURL("https://discord.com/api/webhooks/1/tok/?thread_id=9", "123")
			},
			"https://discord.com/api/webhooks/1/tok/messages/123?thread_id=9",
		},
	}
	for _, tt := range tests {
		got, err := tt.got()
		if err != nil || got != tt.want {
			t.Errorf("%s = %q, %v, want %q", tt.name, got, err, tt.want)
		}
	}
}

func TestDiscordMessageIDTakesOnlyASnowflake(t *testing.T) {
	tests := []struct{ reply, want string }{
		{`{"id":"1291234567890123456","channel_id":"5"}`, "1291234567890123456"},
		{`{"id":"../../x"}`, ""},
		{`{"channel_id":"5"}`, ""},
		{`not json`, ""},
		{``, ""},
	}
	for _, tt := range tests {
		if got := DiscordMessageID([]byte(tt.reply)); got != tt.want {
			t.Errorf("DiscordMessageID(%s) = %q, want %q", tt.reply, got, tt.want)
		}
	}
}
