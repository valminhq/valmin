// Package rclonesetup creates rclone remotes from a guided web setup: the provider's fields,
// browser authorization for OAuth providers, and the remote section written to rclone.conf.
package rclonesetup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/valminhq/valmin/internal/backup/remote"
)

const (
	// TTL is how long a setup stays open after it starts.
	TTL = 15 * time.Minute
	// CallbackPath is the panel page OAuth providers redirect back to.
	CallbackPath = "/admin/rclone-callback"

	exchangeTimeout = 20 * time.Second
)

// The steps a setup moves through. MEGA and S3 start at StepReady.
const (
	StepAuthorize   = "authorize"
	StepChooseDrive = "choose_drive"
	StepReady       = "ready"
)

var (
	// ErrNotFound reports a setup that does not exist, expired, was cancelled, or belongs to
	// another login session.
	ErrNotFound = errors.New("connection setup not found")
	// ErrStaleState reports an authorization response whose state does not match the setup's
	// current authorization request.
	ErrStaleState = errors.New("authorization response does not match the setup")
	// ErrWrongStep reports an action the setup's current step does not allow.
	ErrWrongStep = errors.New("connection setup is not at this step")
)

// AuthorizationError reports a failed browser authorization. The setup stays open with a
// fresh authorization URL, so the administrator can connect again.
type AuthorizationError struct{ Message string }

func (e *AuthorizationError) Error() string { return e.Message }

// RcloneConfig is the rclone configuration a setup checks and obscures passwords with.
type RcloneConfig interface {
	PrepareConfig() error
	Remotes(ctx context.Context) ([]string, error)
	Obscure(ctx context.Context, secret string) (string, error)
}

// Store holds setups in progress. They live in memory only, so a daemon restart drops them.
// Each belongs to the login session that started it, and a session has at most one.
type Store struct {
	rclone    RcloneConfig
	client    *http.Client
	now       func() time.Time
	providers map[string]oauthProvider
	graphURL  string

	mu     sync.Mutex
	setups map[string]*setup
}

// New returns an empty Store. client makes the token exchange and Microsoft Graph requests.
func New(rclone RcloneConfig, client *http.Client) *Store {
	return &Store{
		rclone:    rclone,
		client:    client,
		now:       time.Now,
		providers: oauthProviders(),
		graphURL:  "https://graph.microsoft.com",
		setups:    map[string]*setup{},
	}
}

type setup struct {
	id, owner   string
	req         Request
	callbackURL string
	expires     time.Time
	step        string
	// state and verifier belong to the open authorization request; both are empty while a
	// callback is being exchanged.
	state, verifier string
	token           *oauth2.Token
	drives          []Drive
	drive           Drive
}

// Drive is a OneDrive drive the authorized account can write to.
type Drive struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// Status is a setup as the administrator sees it. It never carries a credential or token.
type Status struct {
	ID               string  `json:"id"`
	Provider         string  `json:"provider"`
	Name             string  `json:"name"`
	Step             string  `json:"step"`
	CallbackURL      string  `json:"callback_url"`
	AuthorizationURL string  `json:"authorization_url,omitempty"`
	Drives           []Drive `json:"drives,omitempty"`
	ExpiresAt        string  `json:"expires_at"`
}

// Callback is the provider's redirect back to the panel, as the callback page received it.
type Callback struct {
	State string
	Code  string
	Error string
}

// Start validates req and opens a setup for owner, the login session ID, replacing any setup
// that session already had. It checks rclone and its configuration first, so a missing
// executable, an unwritable configuration or a taken name is reported before authorization.
func (s *Store) Start(ctx context.Context, owner, callbackURL string, req *Request) (Status, error) {
	if owner == "" {
		return Status{}, errors.New("start connection setup: no login session")
	}
	if errs := req.validate(); len(errs) > 0 {
		return Status{}, errs
	}
	if err := s.rclone.PrepareConfig(); err != nil {
		return Status{}, fmt.Errorf("check rclone configuration: %w", err)
	}
	names, err := s.rclone.Remotes(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("list rclone remotes: %w", err)
	}
	if slices.Contains(names, req.Name) {
		return Status{}, remote.ErrRemoteExists
	}

	st := &setup{
		id: rand.Text(), owner: owner, req: *req, callbackURL: callbackURL,
		expires: s.now().Add(TTL), step: StepReady,
	}
	if st.req.Provider == Mega {
		if st.req.Password, err = s.rclone.Obscure(ctx, req.Password); err != nil {
			return Status{}, fmt.Errorf("obscure MEGA password: %w", err)
		}
	}
	if _, ok := s.providers[req.Provider]; ok {
		st.step = StepAuthorize
		st.arm()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	for id, other := range s.setups {
		if other.owner == owner {
			delete(s.setups, id)
		}
	}
	s.setups[st.id] = st
	return s.status(st), nil
}

// Status returns owner's setup id.
func (s *Store) Status(owner, id string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.lookup(owner, id)
	if err != nil {
		return Status{}, err
	}
	return s.status(st), nil
}

// Authorize completes the browser authorization with the provider's callback. The state is
// single-use: a matching callback consumes it whether or not the exchange succeeds, and a
// failure re-arms the setup with a new authorization request.
func (s *Store) Authorize(ctx context.Context, owner, id string, cb Callback) (Status, error) {
	s.mu.Lock()
	st, err := s.lookup(owner, id)
	if err != nil {
		s.mu.Unlock()
		return Status{}, err
	}
	if st.step != StepAuthorize || st.state == "" ||
		subtle.ConstantTimeCompare([]byte(cb.State), []byte(st.state)) != 1 {
		s.mu.Unlock()
		return Status{}, ErrStaleState
	}
	verifier, conf := st.verifier, s.oauthConfig(st)
	st.state, st.verifier = "", ""
	s.mu.Unlock()

	token, drives, authErr := s.exchange(ctx, conf, st.req.Provider, verifier, cb)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setups[id] != st {
		return Status{}, ErrNotFound
	}
	if authErr != nil {
		st.arm()
		return Status{}, authErr
	}
	st.token, st.drives, st.step = token, drives, StepReady
	if st.req.Provider == OneDrive {
		st.step = StepChooseDrive
	}
	return s.status(st), nil
}

// ChooseDrive selects the OneDrive drive the remote writes to. Only a drive listed for the
// authorized account is accepted.
func (s *Store) ChooseDrive(owner, id, driveID string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.lookup(owner, id)
	if err != nil {
		return Status{}, err
	}
	if st.step != StepChooseDrive {
		return Status{}, ErrWrongStep
	}
	i := slices.IndexFunc(st.drives, func(d Drive) bool { return d.ID == driveID })
	if i < 0 {
		return Status{}, ValidationError{{Field: "drive_id", Message: "Choose one of the listed drives."}}
	}
	st.drive, st.step = st.drives[i], StepReady
	return s.status(st), nil
}

// Section returns the remote a ready setup writes to rclone.conf.
func (s *Store) Section(owner, id string) (remote.RcloneSection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.lookup(owner, id)
	if err != nil {
		return remote.RcloneSection{}, err
	}
	if st.step != StepReady {
		return remote.RcloneSection{}, ErrWrongStep
	}
	return st.section()
}

// Cancel discards owner's setup id.
func (s *Store) Cancel(owner, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.lookup(owner, id); err != nil {
		return err
	}
	delete(s.setups, id)
	return nil
}

// lookup returns owner's setup id after discarding expired setups. The caller holds s.mu.
func (s *Store) lookup(owner, id string) (*setup, error) {
	s.sweep()
	st := s.setups[id]
	if st == nil || owner == "" || st.owner != owner {
		return nil, ErrNotFound
	}
	return st, nil
}

func (s *Store) sweep() {
	now := s.now()
	for id, st := range s.setups {
		if !now.Before(st.expires) {
			delete(s.setups, id)
		}
	}
}

func (s *Store) status(st *setup) Status {
	status := Status{
		ID: st.id, Provider: st.req.Provider, Name: st.req.Name, Step: st.step,
		CallbackURL: st.callbackURL, ExpiresAt: st.expires.UTC().Format(time.RFC3339),
	}
	if st.step == StepAuthorize && st.state != "" {
		p := s.providers[st.req.Provider]
		options := append([]oauth2.AuthCodeOption{oauth2.S256ChallengeOption(st.verifier)}, p.options...)
		status.AuthorizationURL = s.oauthConfig(st).AuthCodeURL(st.state, options...)
	}
	if st.step == StepChooseDrive {
		status.Drives = st.drives
	}
	return status
}

// arm opens a new authorization request. The setup ID prefixes the state so the callback
// page knows which setup the provider's redirect belongs to.
func (st *setup) arm() {
	st.state = st.id + "." + rand.Text()
	st.verifier = oauth2.GenerateVerifier()
}

func (s *Store) oauthConfig(st *setup) *oauth2.Config {
	p := s.providers[st.req.Provider]
	return &oauth2.Config{
		ClientID: st.req.ClientID, ClientSecret: st.req.ClientSecret,
		Endpoint: p.endpoint, RedirectURL: st.callbackURL, Scopes: p.scopes,
	}
}

var providerErrorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// exchange trades the callback's code for a token that can be refreshed unattended, and for
// OneDrive lists the drives that token can reach.
func (s *Store) exchange(
	ctx context.Context,
	conf *oauth2.Config,
	provider, verifier string,
	cb Callback,
) (*oauth2.Token, []Drive, error) {
	switch {
	case cb.Error == "access_denied":
		return nil, nil, &AuthorizationError{"Authorization was denied at the provider. Connect again to retry."}
	case providerErrorCode.MatchString(cb.Error):
		return nil, nil, &AuthorizationError{
			fmt.Sprintf(
				"The provider refused the authorization request (%s). Check the app's callback URL and permissions, then connect again.",
				cb.Error,
			),
		}
	case cb.Error != "", cb.Code == "":
		return nil, nil, &AuthorizationError{"The provider did not return an authorization code. Connect again."}
	}
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, s.client)
	token, err := conf.Exchange(ctx, cb.Code, oauth2.VerifierOption(verifier))
	if _, rejected := errors.AsType[*oauth2.RetrieveError](err); rejected {
		return nil, nil, &AuthorizationError{
			"The provider rejected the authorization. Check the client ID, client secret and callback URL, then connect again.",
		}
	}
	if err != nil {
		return nil, nil, &AuthorizationError{
			"Valmin could not reach the provider to finish authorization. Connect again.",
		}
	}
	if token.RefreshToken == "" {
		return nil, nil, &AuthorizationError{
			"The provider did not grant offline access, which unattended backups need. Remove the app's access from your account, then connect again.",
		}
	}
	if provider != OneDrive {
		return token, nil, nil
	}
	drives, err := s.drives(ctx, token.AccessToken)
	if err != nil {
		return nil, nil, err
	}
	return token, drives, nil
}

type graphDrive struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	DriveType string `json:"driveType"`
}

// drives lists the account's drives the way rclone config does: /me/drives, plus /me/drive
// when the list leaves it out.
func (s *Store) drives(ctx context.Context, accessToken string) ([]Drive, error) {
	var list struct {
		Value []graphDrive `json:"value"`
	}
	if err := s.graph(ctx, accessToken, "/v1.0/me/drives", &list); err != nil {
		return nil, &AuthorizationError{"Valmin could not list the account's OneDrive drives. Connect again."}
	}
	var own graphDrive
	// An account without a provisioned personal drive answers /me/drive with an error; the
	// list above is still usable.
	if err := s.graph(ctx, accessToken, "/v1.0/me/drive", &own); err == nil {
		list.Value = append(list.Value, own)
	}
	var drives []Drive
	for _, d := range list.Value {
		usable := remote.ValidOptionValue(d.ID) &&
			slices.Contains([]string{"personal", "business", "documentLibrary"}, d.DriveType)
		if !usable || slices.ContainsFunc(drives, func(x Drive) bool { return x.ID == d.ID }) {
			continue
		}
		name := strings.TrimSpace(d.Name)
		if name == "" {
			name = d.DriveType
		}
		drives = append(drives, Drive{ID: d.ID, Name: name, Type: d.DriveType})
	}
	if len(drives) == 0 {
		return nil, &AuthorizationError{"This account has no OneDrive drive Valmin can use."}
	}
	return drives, nil
}

func (s *Store) graph(ctx context.Context, accessToken, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.graphURL+path, http.NoBody)
	if err != nil {
		return fmt.Errorf("build graph request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("graph request %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("graph request %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("decode graph response %s: %w", path, err)
	}
	return nil
}
