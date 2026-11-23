package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
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
type Service struct {
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
func NewService(tokens Tokens, invites Invites, oauth OAuth, identity Identity, opts ...Option) *Service {
	return &Service{
		tokens: tokens, invites: invites, oauth: oauth, identity: identity,
		now:     apply(opts).now,
		pending: map[string]pendingLogin{},
	}
}

// Status is what an invite link's page shows: the invite's profile with its
// connected account, and since when it is connected (zero if it isn't).
type Status struct {
	Profile     Profile
	ConnectedAt time.Time
}

// Login is a started login: send the browser to URL and bind it to the login
// with Nonce (e.g. in a cookie that only the callback receives).
type Login struct {
	URL   string
	Nonce string
}

// Callback is what Spotify sends back, plus the browser's nonce.
type Callback struct {
	State string
	Nonce string
	Code  string
	// Error is Spotify's error parameter, e.g. "access_denied".
	Error string
}

// Status reports the profile an invite belongs to and who is connected,
// without using the invite up.
func (s *Service) Status(ctx context.Context, code InviteCode) (Status, error) {
	inv, err := s.checkInvite(ctx, code)
	if err != nil {
		return Status{}, err
	}
	p, err := loadProfile(ctx, s.tokens, inv.ProfileID)
	if err != nil {
		return Status{}, err
	}
	return Status{Profile: p, ConnectedAt: p.connectedAt}, nil
}

// StartLogin begins the OAuth login (state + PKCE) for an invite. The invite
// is only used up when the login succeeds.
func (s *Service) StartLogin(ctx context.Context, code InviteCode) (Login, error) {
	inv, err := s.checkInvite(ctx, code)
	if err != nil {
		return Login{}, err
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
		return Login{}, ErrTooManyLogins
	}
	s.pending[state] = pendingLogin{
		invite: inv.Hash, profile: inv.ProfileID,
		verifier: verifier, nonce: nonce, expires: now.Add(LoginTTL),
	}
	return Login{URL: s.oauth.AuthCodeURL(state, verifier), Nonce: nonce}, nil
}

// FinishLogin completes a login: it exchanges the code, checks the account
// may use the app, uses up the invite and stores the connection, replacing
// any previous one. Each state can be finished at most once. It returns the
// profile with the newly connected account.
func (s *Service) FinishLogin(ctx context.Context, cb Callback) (Profile, error) {
	s.mu.Lock()
	p, ok := s.pending[cb.State]
	delete(s.pending, cb.State)
	s.mu.Unlock()
	if !ok || s.now().After(p.expires) || subtle.ConstantTimeCompare([]byte(cb.Nonce), []byte(p.nonce)) != 1 {
		return nil, ErrLoginExpired
	}
	if cb.Error == "access_denied" {
		return nil, ErrLoginCancelled
	}
	if cb.Error != "" {
		return nil, fmt.Errorf("spotify refused the login: %s", cb.Error)
	}

	tok, err := s.oauth.Exchange(ctx, cb.Code, p.verifier)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, errors.New("spotify sent no refresh token")
	}
	// Before the invite is used up: an account that isn't allowlisted yet can
	// try again with the same link once the owner added it.
	acc, err := s.identity.Account(ctx, tok.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("look up account: %w", err)
	}
	if _, err := s.takeInvite(ctx, p.invite); err != nil {
		return nil, err
	}
	c := Connection{Token: tok, Account: acc, ConnectedAt: s.now()}
	if err := s.tokens.SaveConnection(ctx, p.profile, c); err != nil {
		return nil, fmt.Errorf("save connection: %w", err)
	}
	return newProfile(p.profile, &c), nil
}

// Disconnect uses up the invite and deletes its profile's connection. It
// returns the profile, now without an account.
func (s *Service) Disconnect(ctx context.Context, code InviteCode) (Profile, error) {
	inv, err := s.takeInvite(ctx, hashInvite(code))
	if err != nil {
		return nil, err
	}
	if err := s.tokens.DeleteConnection(ctx, inv.ProfileID); err != nil {
		return nil, fmt.Errorf("delete connection: %w", err)
	}
	return newProfile(inv.ProfileID, nil), nil
}

func (s *Service) checkInvite(ctx context.Context, code InviteCode) (Invite, error) {
	if code == "" {
		return Invite{}, ErrInvalidInvite
	}
	inv, err := s.invites.Invite(ctx, hashInvite(code))
	return s.validInvite(inv, err)
}

func (s *Service) takeInvite(ctx context.Context, h InviteHash) (Invite, error) {
	inv, err := s.invites.TakeInvite(ctx, h)
	return s.validInvite(inv, err)
}

func (s *Service) validInvite(inv Invite, err error) (Invite, error) {
	switch {
	case errors.Is(err, ErrNotFound):
		return Invite{}, ErrInvalidInvite
	case err != nil:
		return Invite{}, fmt.Errorf("load invite: %w", err)
	case s.now().After(inv.Expires):
		return Invite{}, ErrInvalidInvite
	}
	return inv, nil
}

// hashInvite: only the SHA-256 of a code is stored, so reading the storage
// doesn't give anyone a usable link.
func hashInvite(code InviteCode) InviteHash {
	sum := sha256.Sum256([]byte(code))
	return InviteHash(hex.EncodeToString(sum[:]))
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
