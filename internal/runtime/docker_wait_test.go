package runtime

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/docker/docker/client"
)

// TestWaitSurvivesABrokenWait serves a daemon whose first wait response is cut off after its
// headers, as a proxy at its idle timeout leaves it, and checks the exit code still arrives. A
// daemon that answers every wait with an error ends the wait instead.
func TestWaitSurvivesABrokenWait(t *testing.T) {
	tests := []struct {
		name    string
		running bool
		// refuse answers every wait with an error.
		refuse   bool
		wantCode int
		wantErr  bool
		// wantWaits is how many waits the daemon answers.
		wantWaits int32
	}{
		{name: "still running waits again", running: true, wantCode: 3, wantWaits: 2},
		{name: "exited takes the inspected code", running: false, wantCode: 7, wantWaits: 1},
		{name: "refused waits give up", running: true, refuse: true, wantErr: true, wantWaits: maxQuickBreaks},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var waits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/containers/c1/wait"):
					n := waits.Add(1)
					w.Header().Set("Content-Type", "application/json")
					if tt.refuse {
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = fmt.Fprint(w, `{"message":"refused"}`)
						return
					}
					w.WriteHeader(http.StatusOK)
					if n == 1 {
						return
					}
					_, _ = fmt.Fprint(w, `{"StatusCode":3}`)
				case strings.HasSuffix(r.URL.Path, "/containers/c1/json"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"Id":"c1","State":{"Running":%t,"ExitCode":7}}`, tt.running)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			cli, err := client.NewClientWithOpts(
				client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.45"))
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			d := &Docker{cli: cli}

			code, err := d.Wait(t.Context(), "c1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("Wait error = %v, want error %t", err, tt.wantErr)
			}
			if code != tt.wantCode {
				t.Errorf("Wait code = %d, want %d", code, tt.wantCode)
			}
			if got := waits.Load(); got != tt.wantWaits {
				t.Errorf("waits = %d, want %d", got, tt.wantWaits)
			}
		})
	}
}
