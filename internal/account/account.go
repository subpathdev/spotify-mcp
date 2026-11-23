// Package account is the domain of connecting Spotify accounts: profiles,
// one-time invite links, the OAuth login and the stored connection.
//
// It knows nothing about HTTP, files or Spotify's wire format. Those live in
// adapters that implement the ports declared here (Tokens, Invites, OAuth,
// Identity), so the rules below can be tested on their own.
package account

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ProfileID identifies a profile: 1-63 lower-case letters, digits and
// dashes. Adapters use it in file and key names. Get one from ParseProfileID.
type ProfileID string

var profileRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ParseProfileID checks a profile ID.
func ParseProfileID(s string) (ProfileID, error) {
	if !profileRe.MatchString(s) {
		return "", fmt.Errorf("invalid profile %q: use 1-63 lower-case letters, digits and dashes", s)
	}
	return ProfileID(s), nil
}

// Profile is one slot a Spotify account can be connected to, as it was when
// it was loaded (see Profiles). Phase 1 uses a single profile; more about a
// profile (a display name, the person a sign-in system maps to it) is added
// here later.
type Profile interface {
	ID() ProfileID
	// Account is the connected Spotify account; false if none is connected.
	Account() (SpotifyAccount, bool)
}

// profile is the Profile this package builds.
type profile struct {
	id          ProfileID
	account     SpotifyAccount
	connected   bool
	connectedAt time.Time // for Status; not part of Profile
}

// newProfile builds a profile from its connection; c is nil if it has none.
func newProfile(id ProfileID, c *Connection) *profile {
	p := &profile{id: id}
	if c != nil {
		p.account, p.connected, p.connectedAt = c.Account, true, c.ConnectedAt
	}
	return p
}

func (p *profile) ID() ProfileID                   { return p.id }
func (p *profile) Account() (SpotifyAccount, bool) { return p.account, p.connected }

// String makes a profile print as its ID in logs and messages.
func (p *profile) String() string { return string(p.id) }

// SpotifyAccount is the Spotify account connected to a profile.
type SpotifyAccount struct {
	ID          string
	DisplayName string
}

// Name is what to show a person: the display name, else the account ID.
func (a SpotifyAccount) Name() string {
	if a.DisplayName != "" {
		return a.DisplayName
	}
	return a.ID
}

// Token is an OAuth token pair as Spotify issues it.
type Token struct {
	AccessToken  string
	RefreshToken string // empty when Spotify keeps the old one
	Expiry       time.Time
}

// expiryDelta refreshes a little early, so a token doesn't expire in flight.
const expiryDelta = 30 * time.Second

// Connection is what is stored per profile.
type Connection struct {
	Token       Token
	Account     SpotifyAccount
	ConnectedAt time.Time
}

func (c Connection) accessValid(now time.Time) bool {
	return c.Token.AccessToken != "" && now.Add(expiryDelta).Before(c.Token.Expiry)
}

// InviteCode is the secret part of an invite link. It is only ever shown
// once; repositories see its hash.
type InviteCode string

// InviteHash identifies an invite in a repository.
type InviteHash string

// Invite lets whoever holds its code connect or disconnect one profile.
type Invite struct {
	Hash      InviteHash
	ProfileID ProfileID
	Expires   time.Time
}

// Errors returned by this package and by the ports' implementations.
var (
	// ErrNotFound is returned by repositories for a missing entry.
	ErrNotFound = errors.New("not found")
	// ErrInvalidInvite covers unknown, used and expired invites alike, so a
	// caller can't tell which one it was.
	ErrInvalidInvite = errors.New("invite link is invalid or has expired")
	// ErrNotConnected means no account is connected to the profile, or
	// Spotify revoked the access.
	ErrNotConnected = errors.New("no Spotify account is connected yet: create an invite link (spotify-mcp invite) and open it to connect one")
	// ErrLoginExpired means a callback doesn't belong to a login started in
	// the same browser within the last few minutes.
	ErrLoginExpired = errors.New("login expired or was started in another browser")
	// ErrLoginCancelled means the person declined at Spotify.
	ErrLoginCancelled = errors.New("login cancelled at Spotify")
	// ErrTooManyLogins caps the logins waiting for a callback.
	ErrTooManyLogins = errors.New("too many logins in progress")

	// ErrRevoked is returned by OAuth when the refresh token is no longer valid.
	ErrRevoked = errors.New("access was revoked at Spotify")
	// ErrInvalidClient is returned by OAuth when the client ID or secret is wrong.
	ErrInvalidClient = errors.New("client ID or secret rejected by Spotify")
	// ErrNotAllowlisted is returned by Identity when the account isn't on the
	// development-mode user list of the Spotify app.
	ErrNotAllowlisted = errors.New("account is not on the Spotify app's user list")
)

// Tokens stores one Connection per profile.
type Tokens interface {
	// Connection returns ErrNotFound if the profile has none.
	Connection(ctx context.Context, id ProfileID) (Connection, error)
	// SaveConnection replaces the profile's connection.
	SaveConnection(ctx context.Context, id ProfileID, c Connection) error
	// UpdateConnection reads, changes and writes the connection as one step:
	// no other write to the profile may happen in between. It returns
	// ErrNotFound if the profile has none, and writes nothing if fn fails.
	UpdateConnection(ctx context.Context, id ProfileID, fn func(*Connection) error) error
	// DeleteConnection forgets the profile's connection; a missing one is fine.
	DeleteConnection(ctx context.Context, id ProfileID) error
}

// Invites stores pending invites.
type Invites interface {
	AddInvite(ctx context.Context, inv Invite) error
	// Invite returns ErrNotFound for an unknown hash.
	Invite(ctx context.Context, h InviteHash) (Invite, error)
	// TakeInvite returns and removes an invite as one step, so it can be
	// taken only once. It returns ErrNotFound for an unknown hash.
	TakeInvite(ctx context.Context, h InviteHash) (Invite, error)
	// PruneInvites removes invites that expired before t.
	PruneInvites(ctx context.Context, t time.Time) error
}

// OAuth is Spotify's authorization server.
type OAuth interface {
	// AuthCodeURL is where the browser logs in. verifier is the PKCE verifier;
	// the URL carries its S256 challenge.
	AuthCodeURL(state, verifier string) string
	// Exchange trades a callback code for a token.
	Exchange(ctx context.Context, code, verifier string) (Token, error)
	// Refresh returns a new access token, or ErrRevoked.
	Refresh(ctx context.Context, refreshToken string) (Token, error)
}

// Identity tells which Spotify account an access token belongs to.
type Identity interface {
	// Account returns ErrNotAllowlisted for accounts the app may not serve.
	Account(ctx context.Context, accessToken string) (SpotifyAccount, error)
}
