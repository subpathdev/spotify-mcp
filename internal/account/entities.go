package account

import (
	"errors"
	"time"
)

// SpotifyAccount is the Spotify account connected to a profile. Get one from
// NewSpotifyAccount.
type SpotifyAccount interface {
	// ID is Spotify's user ID.
	ID() string
	DisplayName() string
	// Name is what to show a person: the display name, else the ID.
	Name() string
}

type spotifyAccount struct {
	id, displayName string
}

// NewSpotifyAccount builds a SpotifyAccount; the ID is required.
func NewSpotifyAccount(id, displayName string) (SpotifyAccount, error) {
	if id == "" {
		return nil, errors.New("spotify account has no ID")
	}
	return &spotifyAccount{id: id, displayName: displayName}, nil
}

func (a *spotifyAccount) ID() string          { return a.id }
func (a *spotifyAccount) DisplayName() string { return a.displayName }

func (a *spotifyAccount) Name() string {
	if a.displayName != "" {
		return a.displayName
	}
	return a.id
}

// Token is an OAuth token pair as Spotify issues it. Get one from NewToken.
// Spotify's tokens are opaque strings, not JWTs, so there is nothing to parse
// in them.
type Token interface {
	AccessToken() string
	RefreshToken() string
	Expiry() time.Time
	// String is redacted, so a token can't leak into logs by accident.
	String() string
}

type token struct {
	access, refresh string
	expiry          time.Time
}

// NewToken builds a Token; both tokens are required.
func NewToken(accessToken, refreshToken string, expiry time.Time) (Token, error) {
	if accessToken == "" || refreshToken == "" {
		return nil, errors.New("token needs an access and a refresh token")
	}
	return &token{access: accessToken, refresh: refreshToken, expiry: expiry}, nil
}

func (t *token) AccessToken() string  { return t.access }
func (t *token) RefreshToken() string { return t.refresh }
func (t *token) Expiry() time.Time    { return t.expiry }
func (t *token) String() string       { return "Token(redacted)" }

// expiryDelta refreshes a little early, so a token doesn't expire in flight.
const expiryDelta = 30 * time.Second

// Connection is what is stored per profile: the token and the account it
// belongs to. Get one from NewConnection.
type Connection interface {
	Token() Token
	Account() SpotifyAccount
	ConnectedAt() time.Time
}

type connection struct {
	token       Token
	account     SpotifyAccount
	connectedAt time.Time
}

// NewConnection builds a Connection; token and account are required.
func NewConnection(tok Token, acc SpotifyAccount, connectedAt time.Time) (Connection, error) {
	if tok == nil {
		return nil, errors.New("connection needs a token")
	}
	if acc == nil {
		return nil, errors.New("connection needs a Spotify account")
	}
	return &connection{token: tok, account: acc, connectedAt: connectedAt}, nil
}

func (c *connection) Token() Token            { return c.token }
func (c *connection) Account() SpotifyAccount { return c.account }
func (c *connection) ConnectedAt() time.Time  { return c.connectedAt }

// withToken returns a copy of c with another token.
func withToken(c Connection, tok Token) Connection {
	return &connection{token: tok, account: c.Account(), connectedAt: c.ConnectedAt()}
}

func accessValid(c Connection, now time.Time) bool {
	t := c.Token()
	return t != nil && t.AccessToken() != "" && now.Add(expiryDelta).Before(t.Expiry())
}
