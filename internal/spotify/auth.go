package spotify

import (
	"context"
	"errors"
	"net/http"

	"golang.org/x/oauth2"

	"github.com/subpathdev/spotify-mcp/internal/account"
)

// Scopes the server asks for at login: read and control playback, nothing
// else. No library, playlist or e-mail access.
var Scopes = []string{
	"user-read-playback-state",
	"user-modify-playback-state",
	"user-read-currently-playing",
}

// DefaultEndpoint is Spotify's OAuth endpoint. The client secret goes in the
// Authorization header, as Spotify documents.
var DefaultEndpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.spotify.com/authorize",
	TokenURL:  "https://accounts.spotify.com/api/token",
	AuthStyle: oauth2.AuthStyleInHeader,
}

// OAuthConfig configures OAuth.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Endpoint     oauth2.Endpoint // DefaultEndpoint if empty
	HTTPClient   *http.Client    // may be nil
}

// OAuth implements account.OAuth.
type OAuth struct {
	cfg  oauth2.Config
	http *http.Client
}

var _ account.OAuth = (*OAuth)(nil)

// NewOAuth returns an OAuth for cfg.
func NewOAuth(cfg OAuthConfig) *OAuth {
	ep := cfg.Endpoint
	if ep.TokenURL == "" {
		ep = DefaultEndpoint
	}
	return &OAuth{
		cfg: oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
			Endpoint: ep, Scopes: Scopes,
		},
		http: cfg.HTTPClient,
	}
}

func (o *OAuth) ctx(ctx context.Context) context.Context {
	if o.http == nil {
		return ctx
	}
	return context.WithValue(ctx, oauth2.HTTPClient, o.http)
}

// AuthCodeURL implements account.OAuth. show_dialog makes Spotify always show
// its consent page, so someone on a shared browser sees which account they
// are about to connect.
func (o *OAuth) AuthCodeURL(state, verifier string) string {
	return o.cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("show_dialog", "true"))
}

// Exchange implements account.OAuth.
func (o *OAuth) Exchange(ctx context.Context, code, verifier string) (account.Token, error) {
	t, err := o.cfg.Exchange(o.ctx(ctx), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return account.Token{}, mapOAuthError(err)
	}
	return account.Token{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, Expiry: t.Expiry}, nil
}

// Refresh implements account.OAuth.
func (o *OAuth) Refresh(ctx context.Context, refreshToken string) (account.Token, error) {
	t, err := o.cfg.TokenSource(o.ctx(ctx), &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		return account.Token{}, mapOAuthError(err)
	}
	// oauth2 copies the old refresh token when Spotify sends none; report the
	// new one only if it changed.
	rt := t.RefreshToken
	if rt == refreshToken {
		rt = ""
	}
	return account.Token{AccessToken: t.AccessToken, RefreshToken: rt, Expiry: t.Expiry}, nil
}

func mapOAuthError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		switch re.ErrorCode {
		case "invalid_grant":
			return account.ErrRevoked
		case "invalid_client":
			return account.ErrInvalidClient
		}
	}
	return err
}

// User implements account.Identity. Spotify answers 403 for accounts that
// aren't on a development-mode app's user list.
func (a *API) User(ctx context.Context, accessToken string) (account.User, error) {
	var out struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	status, err := a.do(ctx, accessToken, request{method: http.MethodGet, path: "/me", out: &out})
	if status == http.StatusForbidden {
		return account.User{}, account.ErrNotAllowlisted
	}
	if err != nil {
		return account.User{}, err
	}
	return account.User{ID: out.ID, DisplayName: out.DisplayName}, nil
}

var _ account.Identity = (*API)(nil)
