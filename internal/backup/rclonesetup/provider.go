package rclonesetup

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/oauth2"

	"github.com/valminhq/valmin/internal/backup/remote"
)

// The providers a setup can create a remote for. Each is also the remote's rclone type.
const (
	GoogleDrive = "drive"
	Dropbox     = "dropbox"
	OneDrive    = "onedrive"
	Mega        = "mega"
	S3          = "s3"
)

var oauthFields = []string{"client_id", "client_secret"}

// providerFields lists the Request fields each provider accepts.
var providerFields = map[string][]string{
	GoogleDrive: oauthFields,
	Dropbox:     oauthFields,
	OneDrive:    oauthFields,
	Mega:        {"user", "password"},
	S3:          {"s3_provider", "endpoint", "region", "access_key_id", "secret_access_key", "session_token"},
}

var s3Provider = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

// oauthProvider is how a provider's browser authorization is requested. Scopes and options
// match what rclone config requests, so rclone can refresh the token it is given.
type oauthProvider struct {
	endpoint oauth2.Endpoint
	scopes   []string
	options  []oauth2.AuthCodeOption
}

func oauthProviders() map[string]oauthProvider {
	return map[string]oauthProvider{
		GoogleDrive: {
			endpoint: oauth2.Endpoint{
				AuthURL:   "https://accounts.google.com/o/oauth2/auth",
				TokenURL:  "https://oauth2.googleapis.com/token",
				AuthStyle: oauth2.AuthStyleInParams,
			},
			scopes:  []string{"https://www.googleapis.com/auth/drive.file"},
			options: []oauth2.AuthCodeOption{oauth2.AccessTypeOffline, oauth2.ApprovalForce},
		},
		Dropbox: {
			endpoint: oauth2.Endpoint{
				AuthURL:   "https://www.dropbox.com/oauth2/authorize",
				TokenURL:  "https://api.dropboxapi.com/oauth2/token",
				AuthStyle: oauth2.AuthStyleInParams,
			},
			scopes: []string{
				"files.metadata.write", "files.content.write", "files.content.read",
				"sharing.write", "account_info.read",
			},
			options: []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("token_access_type", "offline")},
		},
		OneDrive: {
			endpoint: oauth2.Endpoint{
				AuthURL:   "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
				TokenURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/token",
				AuthStyle: oauth2.AuthStyleInParams,
			},
			scopes: []string{
				"Files.Read", "Files.ReadWrite", "Files.Read.All", "Files.ReadWrite.All",
				"Sites.Read.All", "offline_access",
			},
		},
	}
}

// Request is what an administrator enters to start a setup. Only the fields of the chosen
// provider may be set.
type Request struct {
	Provider        string `json:"provider"`
	Name            string `json:"name"`
	ClientID        string `json:"client_id"`
	ClientSecret    string `json:"client_secret"`
	User            string `json:"user"`
	Password        string `json:"password"`
	S3Provider      string `json:"s3_provider"`
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
}

type field struct{ name, value string }

func (r *Request) fields() []field {
	return []field{
		{"client_id", r.ClientID},
		{"client_secret", r.ClientSecret},
		{"user", r.User},
		{"password", r.Password},
		{"s3_provider", r.S3Provider},
		{"endpoint", r.Endpoint},
		{"region", r.Region},
		{"access_key_id", r.AccessKeyID},
		{"secret_access_key", r.SecretAccessKey},
		{"session_token", r.SessionToken},
	}
}

// FieldError is one invalid Request field, named by its JSON key.
type FieldError struct {
	Field    string
	Required bool
	Message  string
}

// ValidationError lists every invalid field of a Request.
type ValidationError []FieldError

func (e ValidationError) Error() string {
	return fmt.Sprintf("%d invalid connection setup fields", len(e))
}

func (e *ValidationError) required(name string) {
	*e = append(*e, FieldError{Field: name, Required: true, Message: "Enter a value."})
}

func (e *ValidationError) invalid(name, message string) {
	*e = append(*e, FieldError{Field: name, Message: message})
}

// value checks a field that is written to rclone.conf as entered.
func (e *ValidationError) value(f field, required bool) {
	switch {
	case f.value == "" && required:
		e.required(f.name)
	case f.value != "" && !remote.ValidOptionValue(f.value):
		e.invalid(f.name, "Remove surrounding spaces, line breaks and leading quotes.")
	}
}

func (r *Request) validate() ValidationError {
	var errs ValidationError
	switch {
	case r.Name == "":
		errs.required("name")
	case !remote.ValidRemoteName(r.Name) || r.Name == "DEFAULT":
		errs.invalid("name", "Use up to 64 letters, digits, hyphens or underscores, starting with a letter or digit.")
	}
	allowed, ok := providerFields[r.Provider]
	if !ok {
		errs.invalid("provider", "Choose Google Drive, Dropbox, OneDrive, MEGA or S3-compatible storage.")
		return errs
	}
	for _, f := range r.fields() {
		if f.value != "" && !slices.Contains(allowed, f.name) {
			errs.invalid(f.name, "This field does not apply to the chosen provider.")
		}
	}
	switch r.Provider {
	case GoogleDrive, Dropbox, OneDrive:
		errs.value(field{"client_id", r.ClientID}, true)
		errs.value(field{"client_secret", r.ClientSecret}, true)
	case Mega:
		errs.mega(r)
	case S3:
		errs.s3(r)
	}
	return errs
}

func (e *ValidationError) mega(r *Request) {
	switch address, err := mail.ParseAddress(r.User); {
	case r.User == "":
		e.required("user")
	case err != nil || address.Address != r.User:
		e.invalid("user", "Enter the email address of the MEGA account.")
	}
	switch {
	case r.Password == "":
		e.required("password")
	case strings.ContainsFunc(r.Password, unicode.IsControl):
		e.invalid("password", "The password cannot contain line breaks or control characters.")
	}
}

func (e *ValidationError) s3(r *Request) {
	switch {
	case r.S3Provider == "":
		e.required("s3_provider")
	case !s3Provider.MatchString(r.S3Provider):
		e.invalid("s3_provider", "Use the provider name rclone expects, such as AWS, Cloudflare or Other.")
	}
	e.value(field{"access_key_id", r.AccessKeyID}, true)
	e.value(field{"secret_access_key", r.SecretAccessKey}, true)
	e.value(field{"session_token", r.SessionToken}, false)
	e.value(field{"region", r.Region}, false)
	switch {
	case r.Endpoint == "" && r.S3Provider != "AWS":
		e.required("endpoint")
	case r.Endpoint != "" && !validEndpoint(r.Endpoint):
		e.invalid("endpoint", "Enter an HTTPS address without credentials, query or fragment.")
	}
}

func validEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && remote.ValidOptionValue(raw) && u.Scheme == "https" && u.Host != "" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
}

// storedToken is the token shape rclone keeps in a remote's token option.
type storedToken struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

// section is the remote written to rclone.conf. The fixed values match deploy/rclone's templates.
func (st *setup) section() (remote.RcloneSection, error) {
	r := st.req
	options := []remote.RcloneOption{{Key: "type", Value: r.Provider}}
	add := func(key, value string) {
		if value != "" {
			options = append(options, remote.RcloneOption{Key: key, Value: value})
		}
	}
	if slices.Equal(providerFields[r.Provider], oauthFields) {
		add("client_id", r.ClientID)
		add("client_secret", r.ClientSecret)
	}
	switch r.Provider {
	case GoogleDrive:
		add("scope", "drive.file")
		add("use_trash", "true")
	case Dropbox:
		add("batch_mode", "sync")
	case OneDrive:
		add("drive_id", st.drive.ID)
		add("drive_type", st.drive.Type)
	case Mega:
		add("user", r.User)
		add("pass", r.Password)
		add("use_https", "true")
		add("hard_delete", "false")
	case S3:
		add("provider", r.S3Provider)
		add("access_key_id", r.AccessKeyID)
		add("secret_access_key", r.SecretAccessKey)
		add("session_token", r.SessionToken)
		add("endpoint", r.Endpoint)
		add("region", r.Region)
	}
	if st.token != nil {
		//nolint:gosec // Writing the token into the private rclone configuration is the purpose.
		token, err := json.Marshal(storedToken{
			AccessToken: st.token.AccessToken, TokenType: st.token.TokenType,
			RefreshToken: st.token.RefreshToken, Expiry: st.token.Expiry,
		})
		if err != nil {
			return remote.RcloneSection{}, fmt.Errorf("encode token: %w", err)
		}
		add("token", string(token))
	}
	return remote.RcloneSection{Name: r.Name, Options: options}, nil
}
