package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/valminhq/valmin/internal/api/middleware"
	"github.com/valminhq/valmin/internal/authz"
	"github.com/valminhq/valmin/internal/store"
)

// canFunc is the Authorizer as a function, so each test states its own policy in one line
// rather than building a grant table.
type canFunc func(u *store.User, act authz.Action, instanceID string) bool

func (f canFunc) Can(_ context.Context, u *store.User, act authz.Action, instanceID string) bool {
	return f(u, act, instanceID)
}

// fakeRes answers the two existence questions without a database.
type fakeRes struct {
	instances map[string]bool
	// jobs maps a job id to its instance id; the empty string is a global job, and an
	// absent key is a job that does not exist.
	jobs map[string]string
	err  error
}

func (r fakeRes) InstanceExists(_ context.Context, id string) (bool, error) {
	return r.instances[id], r.err
}

func (r fakeRes) JobInstance(_ context.Context, id string) (instanceID string, found bool, err error) {
	instanceID, found = r.jobs[id]
	return instanceID, found, r.err
}

const testOrigin = "https://panel.example"

var admin = &store.User{ID: "u-admin", Username: "admin", Role: store.RoleAdmin}

// env is a hub behind an httptest server, with the session the middleware chain would
// otherwise have put in context.
type env struct {
	hub  *Hub
	srv  *httptest.Server
	user *store.User
	// open is how many connections this test has dialled, so dial can wait for the server
	// to finish registering each one.
	open int
}

func newEnv(t *testing.T, cfg *Config) *env {
	t.Helper()
	if cfg.Origin == "" {
		cfg.Origin = testOrigin
	}
	if cfg.CSRF == nil {
		cfg.CSRF = func(sessionID, token string) bool { return token == "csrf-"+sessionID }
	}
	if cfg.Authz == nil {
		cfg.Authz = canFunc(func(*store.User, authz.Action, string) bool { return true })
	}
	if cfg.Res == nil {
		cfg.Res = fakeRes{}
	}

	e := &env{hub: New(cfg), user: admin}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stands in for the middleware chain, which has already resolved who this is by the
		// time the upgrade handler runs (11 §5.1 row 9).
		ctx := r.Context()
		if session := r.URL.Query().Get("session"); session != "" {
			ctx = middleware.WithUser(ctx, e.user)
			ctx = middleware.WithSessionID(ctx, session)
		}
		e.hub.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// dial opens a connection as session, with the CSRF token in the query parameter and a
// matching Origin — the ordinary browser case.
func (e *env) dial(t *testing.T, session string) *websocket.Conn {
	t.Helper()
	c, err := e.dialRaw(t, "?session="+session+"&csrf=csrf-"+session, testOrigin)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// `↯` Dial returns when the client has the 101, which is before the handler has
	// registered the connection with the hub. Anything a test then addresses to "every
	// connection" — revocation, shutdown — would otherwise race registration and miss one.
	// The same window exists in production and is inherent: a socket that finishes
	// upgrading after a revocation ran is not revoked by it, and is caught instead by the
	// absolute-expiry timer and by every HTTP request it makes with a session that is gone.
	e.open++
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if len(e.hub.snapshot()) >= e.open {
			return c
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the hub did not register the connection")
	return nil
}

func (e *env) dialRaw(t *testing.T, query, origin string) (*websocket.Conn, error) {
	t.Helper()
	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	url := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/v1/ws" + query
	c, resp, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: header,
	})
	if resp != nil && resp.Body != nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	if err != nil {
		if resp != nil {
			return nil, &dialFailure{resp: resp, err: err}
		}
		return nil, err
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c, nil
}

// dialFailure keeps the rejected upgrade's HTTP response, which is the whole point of
// 11 §6.3: a client must be able to read the envelope without parsing a close frame.
type dialFailure struct {
	resp *http.Response
	err  error
}

func (d *dialFailure) Error() string { return d.err.Error() }

func send(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func subscribe(t *testing.T, c *websocket.Conn, topics ...string) {
	t.Helper()
	send(t, c, map[string]any{"type": "subscribe", "topics": topics})
}

// frame is one server→client message, read loosely so a test can assert on the fields it
// cares about without a struct per type.
type frame map[string]any

func read(t *testing.T, c *websocket.Conn) frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	return f
}

// readUntil skips messages until one of the wanted types arrives, so a test asserting on a
// gap is not derailed by the console lines around it.
func readUntil(t *testing.T, c *websocket.Conn, types ...string) frame {
	t.Helper()
	want := map[string]bool{}
	for _, ty := range types {
		want[ty] = true
	}
	for range 4000 {
		f := read(t, c)
		if want[f["type"].(string)] {
			return f
		}
	}
	t.Fatalf("no message of type %v arrived", types)
	return nil
}

// closeCode waits for the connection to close and reports the code.
func closeCode(t *testing.T, c *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}
