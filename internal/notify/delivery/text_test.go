package delivery

import (
	"strings"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

// TestStoredValuesAreFormattedForAReader asserts the raw condition values a notification turns
// into words, and that a value it cannot read passes through rather than vanishing.
func TestStoredValuesAreFormattedForAReader(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"job kind", jobLabel("mod_install"), "Mod install"},
		{"empty job kind", jobLabel(""), "A job"},
		{"bytes", formatBytes("4831838208"), "4.5 GB"},
		{"large bytes", formatBytes("10737418240"), "10 GB"},
		{"not bytes", formatBytes("lots"), "lots"},
		{"hours", formatDuration("6h0m0s"), "6 hours"},
		{"hours and minutes", formatDuration("2h3m0s"), "2 hours 3 minutes"},
		{"one day", formatDuration("27h0m0s"), "1 day 3 hours"},
		{"seconds", formatDuration("40s"), "under a minute"},
		{"not a duration", formatDuration("soon"), "soon"},
		{"time", formatTime("2026-10-07T14:03:00Z", time.UTC), "7 Oct 2026, 14:03 UTC"},
		{"time in a zone", formatTime("2026-10-07T14:03:00Z", time.FixedZone("CEST", 2*3600)), "7 Oct 2026, 16:03 CEST"},
		{"sentence", asSentence("container exited"), "Container exited."},
		{"already a sentence", asSentence("Disk full."), "Disk full."},
		{"no reason", failureReason(""), "Valmin did not record a reason."},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestAnAlertEdgeReadsAsASentenceWithNextSteps asserts each opening names what happened in
// words and ends with what to do, and that a resolution drops the advice.
func TestAnAlertEdgeReadsAsASentenceWithNextSteps(t *testing.T) {
	instanceID := "01920000-0000-7000-8000-000000000002"
	for _, tc := range []struct {
		name      string
		condition store.AlertCondition
		edge      string
		headline  string
		fields    map[string]string
		advice    bool
	}{
		{
			name: "failed job carries its reason",
			condition: store.AlertCondition{Kind: "job_failed", InstanceID: &instanceID, Detail: map[string]string{
				"Job": "backup", "Error": "internal", "Reason": "the archive could not be written",
			}},
			edge: store.EdgeOpened, headline: "Backup failed",
			fields: map[string]string{"Reason": "The archive could not be written."},
			advice: true,
		},
		{
			name:      "low disk in readable sizes",
			condition: store.AlertCondition{Kind: "low_disk", Detail: map[string]string{"Free": "4831838208", "Alarm": "10737418240"}},
			edge:      store.EdgeOpened, headline: "The host is running low on disk space",
			fields: map[string]string{"Free space": "4.5 GB", "Alert below": "10 GB"},
			advice: true,
		},
		{
			name:      "crash loop in words",
			condition: store.AlertCondition{Kind: "crash_loop", InstanceID: &instanceID, Detail: map[string]string{"Stops": "3", "Window": "1h0m0s"}},
			edge:      store.EdgeOpened, headline: "The server keeps crashing",
			fields: map[string]string{"Unexpected stops": "3 in the last 1 hour"},
			advice: true,
		},
		{
			name:      "stale backup with no archive",
			condition: store.AlertCondition{Kind: "stale_backup", InstanceID: &instanceID, Detail: map[string]string{"Every": "6h0m0s"}},
			edge:      store.EdgeOpened, headline: "Scheduled backups have stopped",
			fields: map[string]string{"Schedule": "Every 6 hours", "Last backup": "None on record"},
			advice: true,
		},
		{
			name:      "stuck job",
			condition: store.AlertCondition{Kind: "job_stuck", InstanceID: &instanceID, Detail: map[string]string{"Job": "game_update", "Running": "2h3m0s"}},
			edge:      store.EdgeOpened, headline: "Game update is taking unusually long",
			fields: map[string]string{"Running for": "2 hours 3 minutes"},
			advice: true,
		},
		{
			name: "server error names an out-of-memory kill",
			condition: store.AlertCondition{Kind: "instance_error", InstanceID: &instanceID, Detail: map[string]string{
				"Reason": instance.ReasonOOMKilled, "MemLimitMB": "4096",
			}},
			edge: store.EdgeOpened, headline: "The server needs a check after a failure",
			fields: map[string]string{
				"Cause":      "The server used all 4096 MB its memory limit allows, so the kernel stopped it. The world may be damaged.",
				"What to do": "Raise the memory limit in the server's settings, then open the server in Valmin and choose Check this server.",
			},
			advice: true,
		},
		{
			name: "mods that failed to load",
			condition: store.AlertCondition{Kind: "mods_failed", InstanceID: &instanceID, Detail: map[string]string{
				"Mods": "Jotunn, PlanBuild (skipped)", "Reason": "Error loading [Jotunn 2.30.2]",
			}},
			edge: store.EdgeOpened, headline: "Mods failed to load",
			fields: map[string]string{"Mods": "Jotunn, PlanBuild (skipped)", "Reason": "Error loading [Jotunn 2.30.2]"},
			advice: true,
		},
		{
			name: "pending mod updates",
			condition: store.AlertCondition{Kind: "mod_updates", InstanceID: &instanceID, Detail: map[string]string{
				"Count": "1", "Mods": "Jotunn 2.30.2 → 2.30.3",
			}},
			edge: store.EdgeOpened, headline: "Mod updates are available",
			fields: map[string]string{"Updates": "1", "Mods": "Jotunn 2.30.2 → 2.30.3"},
			advice: true,
		},
		{
			name:      "resolution drops the advice",
			condition: store.AlertCondition{Kind: "crash_loop", InstanceID: &instanceID, Detail: map[string]string{"Stops": "3", "Window": "1h0m0s"}},
			edge:      store.EdgeResolved, headline: "Resolved: The server keeps crashing",
			fields: map[string]string{"Unexpected stops": "3 in the last 1 hour"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := alertEvent(
				&tc.condition,
				notify.KindAlertOpened,
				tc.edge,
				map[string]string{instanceID: "Midgard"},
				time.UTC,
			)
			if e.Headline() != tc.headline {
				t.Errorf("headline = %q, want %q", e.Headline(), tc.headline)
			}
			got := map[string]string{}
			for _, f := range e.Detail {
				got[f.Name] = f.Value
			}
			for name, want := range tc.fields {
				if got[name] != want {
					t.Errorf("%s = %q, want %q (detail %v)", name, got[name], want, e.Detail)
				}
			}
			if _, has := got["What to do"]; has != tc.advice {
				t.Errorf("advice present = %v, want %v", has, tc.advice)
			}
			if tc.condition.InstanceID != nil && e.InstanceName != "Midgard" {
				t.Errorf("instance name = %q, want the server's", e.InstanceName)
			}
		})
	}
}

// TestAnUnexpectedStopSaysWhereTheServerIsAndWhatToDo asserts the states a server can be left in
// after going down on its own are explained rather than named.
func TestAnUnexpectedStopSaysWhereTheServerIsAndWhatToDo(t *testing.T) {
	for _, tc := range []struct{ to, status string }{
		{"stopped", "Stopped"},
		{"error", "Held in the error state; its controls are locked until someone checks it"},
		{"running", "Running again"},
	} {
		fields := downDetail(tc.to, "container exited", 0)
		if fields[0].Value != tc.status || fields[1].Value != "Container exited." || fields[2].Value == "" {
			t.Errorf("downDetail(%s) = %+v", tc.to, fields)
		}
	}
}

// TestAnOutOfMemoryStopAdvisesOnMemory asserts a kernel kill is named in words, with advice
// that depends on whether the server has a memory limit.
func TestAnOutOfMemoryStopAdvisesOnMemory(t *testing.T) {
	for _, tc := range []struct {
		limitMB      int
		cause, setup string
	}{
		{4096, "The server used all 4096 MB its memory limit allows", "Raise the memory limit"},
		{0, "The host ran out of memory", "Free memory on the host"},
	} {
		fields := downDetail("error", instance.ReasonOOMKilled, tc.limitMB)
		if !strings.HasPrefix(fields[1].Value, tc.cause) || !strings.HasPrefix(fields[2].Value, tc.setup) {
			t.Errorf("downDetail with a %d MB limit = %+v", tc.limitMB, fields)
		}
	}
}

// TestALinkPointsAtTheServerOrThePanel asserts where a notification links: the server's page,
// the front page for a host-wide event, and nowhere without a configured address.
func TestALinkPointsAtTheServerOrThePanel(t *testing.T) {
	n := &Notifier{ExternalURL: "https://panel.example/"}
	server := &notify.Event{InstanceID: "abc"}
	n.link(server)
	host := &notify.Event{}
	n.link(host)
	if server.URL != "https://panel.example/instances/abc" || host.URL != "https://panel.example/" {
		t.Errorf("links = %q, %q", server.URL, host.URL)
	}
	none := &notify.Event{InstanceID: "abc"}
	(&Notifier{}).link(none)
	if none.URL != "" {
		t.Errorf("link without an address = %q, want none", none.URL)
	}
}
