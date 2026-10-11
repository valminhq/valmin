package rclonesetup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/valminhq/valmin/internal/backup/remote"
)

const callback = "https://panel.example" + CallbackPath

type fakeRclone struct{ remotes []string }

func (f *fakeRclone) PrepareConfig() error { return nil }

func (f *fakeRclone) Remotes(context.Context) ([]string, error) { return f.remotes, nil }

func (f *fakeRclone) Obscure(_ context.Context, secret string) (string, error) {
	return "obscured-" + strings.ToUpper(secret), nil
}

// provider stands in for an OAuth token endpoint and Microsoft Graph.
type provider struct {
	*httptest.Server
	refreshToken string
	drives       string
	verifiers    []string
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	p := &provider{
		refreshToken: "refresh-1",
		drives:       `{"value":[{"id":"drive-a","name":"OneDrive","driveType":"personal"}]}`,
	}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			p.verifiers = append(p.verifiers, r.PostForm.Get("code_verifier"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access-1", "token_type": "Bearer",
				"refresh_token": p.refreshToken, "expires_in": 3600,
			})
		case "/v1.0/me/drives":
			_, _ = w.Write([]byte(p.drives))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.Close)
	return p
}

func newStore(t *testing.T, p *provider, remotes ...string) *Store {
	t.Helper()
	s := New(&fakeRclone{remotes: remotes}, http.DefaultClient, []netip.Prefix{netip.MustParsePrefix("10.20.0.0/16")})
	for name, op := range s.providers {
		op.endpoint = oauth2.Endpoint{
			AuthURL: "https://auth.example/authorize", TokenURL: p.URL + "/token",
			AuthStyle: oauth2.AuthStyleInParams,
		}
		s.providers[name] = op
	}
	s.graphURL = p.URL
	return s
}

func oauthRequest(provider string) *Request {
	return &Request{Provider: provider, Name: provider + "-backups", ClientID: "client", ClientSecret: "client-secret"}
}

func authorizationState(t *testing.T, authorizationURL string) string {
	t.Helper()
	u, err := url.Parse(authorizationURL)
	if err != nil || authorizationURL == "" {
		t.Fatalf("authorization_url = %q", authorizationURL)
	}
	return u.Query().Get("state")
}

func TestStartValidatesFieldsForTheChosenProvider(t *testing.T) {
	for _, tt := range []struct {
		name string
		req  Request
		want []string
	}{
		{"unknown provider", Request{Provider: "ftp", Name: "x"}, []string{"provider"}},
		{"bad name", Request{Provider: S3, Name: "DEFAULT", S3Provider: "AWS", AccessKeyID: "a", SecretAccessKey: "b"}, []string{"name"}},
		{"oauth without credentials", Request{Provider: GoogleDrive, Name: "d"}, []string{"client_id", "client_secret"}},
		{"field of another provider", Request{Provider: Dropbox, Name: "d", ClientID: "c", ClientSecret: "s", User: "u"}, []string{"user"}},
		{"mega needs an email", Request{Provider: Mega, Name: "m", User: "not-an-email", Password: "p"}, []string{"user"}},
		{"mega password line break", Request{Provider: Mega, Name: "m", User: "a@b.example", Password: "a\nb"}, []string{"password"}},
		{"s3 endpoint needed beyond AWS", Request{Provider: S3, Name: "s", S3Provider: "Other", AccessKeyID: "a", SecretAccessKey: "b"}, []string{"endpoint"}},
		{"s3 endpoint must be https", Request{Provider: S3, Name: "s", S3Provider: "Other", AccessKeyID: "a", SecretAccessKey: "b", Endpoint: "http://minio.local"}, []string{"endpoint"}},
		{"s3 secret with padding", Request{Provider: S3, Name: "s", S3Provider: "AWS", AccessKeyID: "a", SecretAccessKey: " b"}, []string{"secret_access_key"}},
		{"s3 on AWS", Request{Provider: S3, Name: "s", S3Provider: "AWS", AccessKeyID: "a", SecretAccessKey: "b", Region: "eu-west-1"}, nil},
		{"s3 on a public address", Request{Provider: S3, Name: "s", S3Provider: "Minio", AccessKeyID: "a", SecretAccessKey: "b", Endpoint: "https://1.1.1.1:9000"}, nil},
		{"s3 on an allowed private network", Request{Provider: S3, Name: "s", S3Provider: "Minio", AccessKeyID: "a", SecretAccessKey: "b", Endpoint: "https://10.20.0.5:9000"}, nil},
		{"s3 on another private network", Request{Provider: S3, Name: "s", S3Provider: "Minio", AccessKeyID: "a", SecretAccessKey: "b", Endpoint: "https://192.168.1.5:9000"}, []string{"endpoint"}},
		{"s3 on loopback", Request{Provider: S3, Name: "s", S3Provider: "Minio", AccessKeyID: "a", SecretAccessKey: "b", Endpoint: "https://127.0.0.1:9000"}, []string{"endpoint"}},
		{"s3 on the metadata address", Request{Provider: S3, Name: "s", S3Provider: "Other", AccessKeyID: "a", SecretAccessKey: "b", Endpoint: "https://169.254.169.254"}, []string{"endpoint"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t, newProvider(t))
			_, err := s.Start(t.Context(), "session", callback, &tt.req)
			var got []string
			if fields, ok := errors.AsType[ValidationError](err); ok {
				for _, f := range fields {
					got = append(got, f.Field)
				}
			} else if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("Start() invalid fields = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStartRefusesAnExistingRemoteName(t *testing.T) {
	s := newStore(t, newProvider(t), "drive-backups")
	if _, err := s.Start(
		t.Context(),
		"session",
		callback,
		oauthRequest(GoogleDrive),
	); !errors.Is(
		err,
		remote.ErrRemoteExists,
	) {
		t.Fatalf("Start() error = %v, want ErrRemoteExists", err)
	}
}

func TestAuthorizationUsesPKCEAndASingleUseState(t *testing.T) {
	p := newProvider(t)
	s := newStore(t, p)
	status, err := s.Start(t.Context(), "session", callback, oauthRequest(GoogleDrive))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(status.AuthorizationURL)
	q := u.Query()
	for key, want := range map[string]string{
		"redirect_uri": callback, "access_type": "offline", "prompt": "consent",
		"code_challenge_method": "S256", "scope": "https://www.googleapis.com/auth/drive.file",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("authorization %s = %q, want %q", key, got, want)
		}
	}
	state := q.Get("state")
	if !strings.HasPrefix(state, status.ID+".") {
		t.Fatalf("state %q does not name setup %q", state, status.ID)
	}
	if _, err := s.Authorize(
		t.Context(),
		"session",
		status.ID,
		Callback{State: status.ID + ".forged", Code: "c"},
	); !errors.Is(
		err,
		ErrStaleState,
	) {
		t.Fatalf("Authorize(forged state) error = %v, want ErrStaleState", err)
	}

	status, err = s.Authorize(t.Context(), "session", status.ID, Callback{State: state, Code: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if status.Step != StepReady || status.AuthorizationURL != "" {
		t.Fatalf("status after authorization = %+v", status)
	}
	if len(p.verifiers) != 1 || oauth2.S256ChallengeFromVerifier(p.verifiers[0]) != q.Get("code_challenge") {
		t.Fatalf("token request verifiers = %q; want one matching the challenge", p.verifiers)
	}
	if _, err := s.Authorize(
		t.Context(),
		"session",
		status.ID,
		Callback{State: state, Code: "c"},
	); !errors.Is(
		err,
		ErrStaleState,
	) {
		t.Fatalf("Authorize(reused state) error = %v, want ErrStaleState", err)
	}
	section, err := s.Section("session", status.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token := option(section, "token"); !strings.Contains(token, `"refresh_token":"refresh-1"`) {
		t.Fatalf("token option = %q", token)
	}
}

func TestFailedAuthorizationOpensANewRequest(t *testing.T) {
	for _, tt := range []struct {
		name         string
		callback     Callback
		refreshToken string
	}{
		{"denied", Callback{Error: "access_denied"}, "refresh-1"},
		{"no refresh token", Callback{Code: "c"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := newProvider(t)
			p.refreshToken = tt.refreshToken
			s := newStore(t, p)
			status, err := s.Start(t.Context(), "session", callback, oauthRequest(Dropbox))
			if err != nil {
				t.Fatal(err)
			}
			first := authorizationState(t, status.AuthorizationURL)
			tt.callback.State = first
			if _, err := s.Authorize(t.Context(), "session", status.ID, tt.callback); err == nil {
				t.Fatal("Authorize() succeeded")
			} else if _, ok := errors.AsType[*AuthorizationError](err); !ok {
				t.Fatalf("Authorize() error = %v, want AuthorizationError", err)
			}
			status, err = s.Status("session", status.ID)
			if err != nil {
				t.Fatal(err)
			}
			if status.Step != StepAuthorize || authorizationState(t, status.AuthorizationURL) == first {
				t.Fatalf("status after failure = %+v; want a new authorization request", status)
			}
		})
	}
}

func TestOneDriveAcceptsOnlyAListedDrive(t *testing.T) {
	s := newStore(t, newProvider(t))
	status, err := s.Start(t.Context(), "session", callback, oauthRequest(OneDrive))
	if err != nil {
		t.Fatal(err)
	}
	status, err = s.Authorize(
		t.Context(),
		"session",
		status.ID,
		Callback{State: authorizationState(t, status.AuthorizationURL), Code: "c"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if status.Step != StepChooseDrive || len(status.Drives) != 1 {
		t.Fatalf("status = %+v; want one drive to choose", status)
	}
	if _, err := s.Section("session", status.ID); !errors.Is(err, ErrWrongStep) {
		t.Fatalf("Section() before a drive is chosen error = %v, want ErrWrongStep", err)
	}
	if _, err := s.ChooseDrive("session", status.ID, "someone-elses-drive"); err == nil {
		t.Fatal("ChooseDrive(unlisted drive) succeeded")
	}
	if _, err := s.ChooseDrive("session", status.ID, "drive-a"); err != nil {
		t.Fatal(err)
	}
	section, err := s.Section("session", status.ID)
	if err != nil {
		t.Fatal(err)
	}
	if option(section, "drive_id") != "drive-a" || option(section, "drive_type") != "personal" {
		t.Fatalf("section = %+v", section)
	}
}

func TestSetupBelongsToItsSessionAndExpires(t *testing.T) {
	s := newStore(t, newProvider(t))
	now := time.Now()
	s.now = func() time.Time { return now }
	first, err := s.Start(t.Context(), "session", callback, oauthRequest(GoogleDrive))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Status("other-session", first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Status(other session) error = %v, want ErrNotFound", err)
	}
	second, err := s.Start(t.Context(), "session", callback, oauthRequest(Dropbox))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Status("session", first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Status(replaced setup) error = %v, want ErrNotFound", err)
	}
	now = now.Add(TTL)
	if _, err := s.Status("session", second.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Status(expired setup) error = %v, want ErrNotFound", err)
	}
}

func TestStatusAndSectionHandleSecrets(t *testing.T) {
	s := newStore(t, newProvider(t))
	mega := &Request{Provider: Mega, Name: "mega-backups", User: "owner@example.com", Password: "plain-password"}
	status, err := s.Start(t.Context(), "session", callback, mega)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "plain-password") || strings.Contains(string(encoded), "OBSCURED") {
		t.Fatalf("status exposes the password: %s", encoded)
	}
	section, err := s.Section("session", status.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := option(section, "pass"); got != "obscured-PLAIN-PASSWORD" {
		t.Fatalf("pass option = %q, want the obscured password", got)
	}

	drive, err := s.Start(t.Context(), "session", callback, oauthRequest(GoogleDrive))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(drive)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "client-secret") {
		t.Fatalf("status exposes the client secret: %s", encoded)
	}
}

func TestSectionsKeepTheTemplateDefaults(t *testing.T) {
	requests := map[string]*Request{
		GoogleDrive: oauthRequest(GoogleDrive),
		Dropbox:     oauthRequest(Dropbox),
		OneDrive:    oauthRequest(OneDrive),
		Mega:        {Provider: Mega, Name: "mega-backups", User: "owner@example.com", Password: "p"},
		S3: {
			Provider: S3, Name: "s3-backups", S3Provider: "Other", Endpoint: "https://s3.example",
			AccessKeyID: "a", SecretAccessKey: "b",
		},
	}
	for provider, req := range requests {
		t.Run(provider, func(t *testing.T) {
			st := &setup{req: *req, token: &oauth2.Token{AccessToken: "a", RefreshToken: "r"}}
			section, err := st.section()
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range templateDefaults(t, provider) {
				if got := option(section, key); got != want {
					t.Errorf("%s = %q, want the template's %q", key, got, want)
				}
			}
		})
	}
}

// templateDefaults reads the fixed options of deploy/rclone's example for provider, leaving
// out the placeholders an administrator replaces.
func templateDefaults(t *testing.T, provider string) map[string]string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "deploy", "rclone", provider+".conf.example"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	defaults := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), " = ")
		if ok && !strings.HasPrefix(key, "#") && !strings.Contains(value, "YOUR_") {
			defaults[key] = value
		}
	}
	if len(defaults) == 0 {
		t.Fatalf("template for %s has no fixed options", provider)
	}
	return defaults
}

func option(section remote.RcloneSection, key string) string {
	for _, o := range section.Options {
		if o.Key == key {
			return o.Value
		}
	}
	return ""
}
