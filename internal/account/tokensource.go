package account

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TokenSource hands out a valid access token for one profile, refreshing it
// when needed.
//
// The refresh runs inside Tokens.UpdateConnection, so it reads, refreshes and
// writes the connection as one step. A login that lands meanwhile waits and
// then replaces the result, instead of being overwritten by the old account's
// refreshed token. It also means two callers never refresh the same token.
type TokenSource struct {
	tokens  Tokens
	oauth   OAuth
	profile ProfileID
	now     func() time.Time
}

// NewTokenSource returns a TokenSource for p.
func NewTokenSource(tokens Tokens, oauth OAuth, id ProfileID, opts ...Option) *TokenSource {
	return &TokenSource{tokens: tokens, oauth: oauth, profile: id, now: apply(opts).now}
}

// AccessToken returns a valid access token, or ErrNotConnected.
func (ts *TokenSource) AccessToken(ctx context.Context) (string, error) {
	c, err := ts.tokens.Connection(ctx, ts.profile)
	if errors.Is(err, ErrNotFound) {
		return "", ErrNotConnected
	}
	if err != nil {
		return "", fmt.Errorf("load connection: %w", err)
	}
	if c.accessValid(ts.now()) {
		return c.Token.AccessToken, nil
	}

	var access string
	err = ts.tokens.UpdateConnection(ctx, ts.profile, func(c *Connection) error {
		if c.accessValid(ts.now()) { // refreshed by someone else meanwhile
			access = c.Token.AccessToken
			return nil
		}
		tok, err := ts.oauth.Refresh(ctx, c.Token.RefreshToken)
		if err != nil {
			return err
		}
		if tok.RefreshToken == "" { // Spotify kept the old one
			tok.RefreshToken = c.Token.RefreshToken
		}
		c.Token, access = tok, tok.AccessToken
		return nil
	})
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrRevoked):
		return "", ErrNotConnected
	case err != nil:
		return "", fmt.Errorf("refresh Spotify token: %w", err)
	}
	return access, nil
}

// Invalidate marks accessToken as expired after Spotify rejected it, so the
// next call refreshes. It does nothing if the stored token has changed since.
func (ts *TokenSource) Invalidate(ctx context.Context, accessToken string) error {
	err := ts.tokens.UpdateConnection(ctx, ts.profile, func(c *Connection) error {
		if c.Token.AccessToken == accessToken {
			c.Token.Expiry = time.Time{}
		}
		return nil
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
