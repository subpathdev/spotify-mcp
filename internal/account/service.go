package account

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// LoginTTL is how long a started login waits for Spotify's callback.
	LoginTTL   = 10 * time.Minute
	maxPending = 100
)

// Service runs logins and disconnects for holders of an invite.
type Service interface {
	// Status reports the profile an invite belongs to and who is connected,
	// without using the invite up.
	Status(ctx context.Context, code InviteCode) (Status, error)
	// StartLogin begins the OAuth login (state + PKCE) for an invite. The
	// invite is only used up when the login succeeds.
	StartLogin(ctx context.Context, code InviteCode) (Login, error)
	// FinishLogin completes a login: it exchanges the code, checks the
	// account may use the app, uses up the invite and stores the connection,
	// replacing any previous one. Each state can be finished at most once.
	// It returns the profile with the newly connected account.
	FinishLogin(ctx context.Context, cb Callback) (Profile, error)
	// Disconnect uses up the invite and deletes its profile's connection. It
	// returns the profile, now without an account.
	Disconnect(ctx context.Context, code InviteCode) (Profile, error)
}

type service struct {
	tokens   Tokens
	invites  Invites
	oauth    OAuth
	identity Identity
	now      func() time.Time

	mu      sync.Mutex
	pending map[string]pendingLogin // by OAuth state
}

type pendingLogin struct {
	invite   InviteHash
	profile  ProfileID
	verifier string
	nonce    string
	expires  time.Time
}

// NewService returns a Service using the given ports.
func NewService(tokens Tokens, invites Invites, oauth OAuth, identity Identity, opts ...Option) Service {
	return &service{
		tokens: tokens, invites: invites, oauth: oauth, identity: identity,
		now:     apply(opts).now,
		pending: map[string]pendingLogin{},
	}
}

// Status is what an invite link's page shows.
type Status interface {
	// Profile is the invite's profile with its connected account.
	Profile() Profile
	// ConnectedAt is when the account was connected; zero if none is.
	ConnectedAt() time.Time
}

type status struct{ p *profile }

func (s status) Profile() Profile       { return s.p }
func (s status) ConnectedAt() time.Time { return s.p.connectedAt }

// Login is a started login: send the browser to URL and bind it to the login
// with Nonce (e.g. in a cookie that only the callback receives).
type Login interface {
	URL() string
	Nonce() string
}

type login struct{ url, nonce string }

func (l login) URL() string   { return l.url }
func (l login) Nonce() string { return l.nonce }

// Callback is what Spotify sends back, plus the browser's nonce. Get one
// from NewCallback.
type Callback interface {
	State() string
	Nonce() string
	Code() string
	// SpotifyError is Spotify's error parameter, e.g. "access_denied".
	SpotifyError() string
}

type callback struct{ state, nonce, code, spotifyError string }

// NewCallback builds a Callback from the callback request. A state or nonce
// that doesn't belong to a started login makes FinishLogin fail with
// ErrLoginExpired.
func NewCallback(state, nonce, code, spotifyError string) Callback {
	return callback{state: state, nonce: nonce, code: code, spotifyError: spotifyError}
}

func (c callback) State() string        { return c.state }
func (c callback) Nonce() string        { return c.nonce }
func (c callback) Code() string         { return c.code }
func (c callback) SpotifyError() string { return c.spotifyError }

func (s *service) Status(ctx context.Context, code InviteCode) (Status, error) {
	inv, err := s.checkInvite(ctx, code)
	if err != nil {
		return nil, err
	}
	p, err := loadProfile(ctx, s.tokens, inv.ProfileID())
	if err != nil {
		return nil, err
	}
	return status{p: p}, nil
}

func (s *service) StartLogin(ctx context.Context, code InviteCode) (Login, error) {
	inv, err := s.checkInvite(ctx, code)
	if err != nil {
		return nil, err
	}
	state, nonce, verifier := randomString(), randomString(), randomString()
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.pending {
		if now.After(p.expires) {
			delete(s.pending, k)
		}
	}
	if len(s.pending) >= maxPending {
		return nil, ErrTooManyLogins
	}
	s.pending[state] = pendingLogin{
		invite: inv.Hash(), profile: inv.ProfileID(),
		verifier: verifier, nonce: nonce, expires: now.Add(LoginTTL),
	}
	return login{url: s.oauth.AuthCodeURL(state, verifier), nonce: nonce}, nil
}

func (s *service) FinishLogin(ctx context.Context, cb Callback) (Profile, error) {
	if cb == nil {
		return nil, ErrLoginExpired
	}
	s.mu.Lock()
	p, ok := s.pending[cb.State()]
	delete(s.pending, cb.State())
	s.mu.Unlock()
	if !ok || s.now().After(p.expires) || subtle.ConstantTimeCompare([]byte(cb.Nonce()), []byte(p.nonce)) != 1 {
		return nil, ErrLoginExpired
	}
	if cb.SpotifyError() == "access_denied" {
		return nil, ErrLoginCancelled
	}
	if e := cb.SpotifyError(); e != "" {
		return nil, fmt.Errorf("spotify refused the login: %s", e)
	}

	tok, err := s.oauth.Exchange(ctx, cb.Code(), p.verifier)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}
	if tok == nil || tok.RefreshToken() == "" {
		return nil, errors.New("spotify sent no refresh token")
	}
	// Before the invite is used up: an account that isn't allowlisted yet can
	// try again with the same link once the owner added it.
	acc, err := s.identity.Account(ctx, tok.AccessToken())
	if err != nil {
		return nil, fmt.Errorf("look up account: %w", err)
	}
	c, err := NewConnection(tok, acc, s.now())
	if err != nil {
		return nil, err
	}
	if _, err := s.takeInvite(ctx, p.invite); err != nil {
		return nil, err
	}
	if err := s.tokens.SaveConnection(ctx, p.profile, c); err != nil {
		return nil, fmt.Errorf("save connection: %w", err)
	}
	return newProfile(p.profile, c), nil
}

func (s *service) Disconnect(ctx context.Context, code InviteCode) (Profile, error) {
	if code == nil {
		return nil, ErrInvalidInvite
	}
	inv, err := s.takeInvite(ctx, hashInvite(code))
	if err != nil {
		return nil, err
	}
	if err := s.tokens.DeleteConnection(ctx, inv.ProfileID()); err != nil {
		return nil, fmt.Errorf("delete connection: %w", err)
	}
	return newProfile(inv.ProfileID(), nil), nil
}

func (s *service) checkInvite(ctx context.Context, code InviteCode) (Invite, error) {
	if code == nil {
		return nil, ErrInvalidInvite
	}
	inv, err := s.invites.Invite(ctx, hashInvite(code))
	return s.validInvite(inv, err)
}

func (s *service) takeInvite(ctx context.Context, h InviteHash) (Invite, error) {
	inv, err := s.invites.TakeInvite(ctx, h)
	return s.validInvite(inv, err)
}

func (s *service) validInvite(inv Invite, err error) (Invite, error) {
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, ErrInvalidInvite
	case err != nil:
		return nil, fmt.Errorf("load invite: %w", err)
	case inv == nil || inv.ProfileID() == nil:
		return nil, errors.New("load invite: repository returned an incomplete invite")
	case s.now().After(inv.Expires()):
		return nil, ErrInvalidInvite
	}
	return inv, nil
}

// randomString returns 256 random bits, base64url-encoded (43 characters,
// which is also a valid PKCE verifier).
func randomString() string {
	return base64.RawURLEncoding.EncodeToString(randomBytes(32))
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails, see crypto/rand.Read
	return b
}
