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

// Profile is one connected account. Phase 1 uses a single profile; it is an
// interface so more about a profile (a display name, the person a sign-in
// system maps to it) can be added later without changing the code that only
// passes profiles around.
//
// Profiles are not comparable with ==; compare their IDs.
type Profile interface {
	// ID is the validated identifier: 1-63 lower-case letters, digits and
	// dashes. Adapters use it in file and key names.
	ID() string
}

// profile is the Profile built by ParseProfile.
type profile struct {
	id string
}

func (p *profile) ID() string { return p.id }

// String makes a profile print as its ID in logs and messages.
func (p *profile) String() string { return p.id }

var profileRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ParseProfile checks a profile ID and returns its Profile. Adapters may use
// the ID in file or key names, so it is restricted to lower-case letters,
// digits and dashes.
func ParseProfile(id string) (Profile, error) {
	if !profileRe.MatchString(id) {
		return nil, fmt.Errorf("invalid profile %q: use 1-63 lower-case letters, digits and dashes", id)
	}
	return &profile{id: id}, nil
}

// User is the Spotify account behind a connection.
type User struct {
	ID          string
	DisplayName string
}

// Name is what to show a person: the display name, else the account ID.
func (u User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.ID
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
	User        User
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
	Hash    InviteHash
	Profile Profile
	Expires time.Time
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
	Connection(ctx context.Context, p Profile) (Connection, error)
	// SaveConnection replaces the profile's connection.
	SaveConnection(ctx context.Context, p Profile, c Connection) error
	// UpdateConnection reads, changes and writes the connection as one step:
	// no other write to the profile may happen in between. It returns
	// ErrNotFound if the profile has none, and writes nothing if fn fails.
	UpdateConnection(ctx context.Context, p Profile, fn func(*Connection) error) error
	// DeleteConnection forgets the profile's connection; a missing one is fine.
	DeleteConnection(ctx context.Context, p Profile) error
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

// Identity tells who an access token belongs to.
type Identity interface {
	// User returns ErrNotAllowlisted for accounts the app may not serve.
	User(ctx context.Context, accessToken string) (User, error)
}
