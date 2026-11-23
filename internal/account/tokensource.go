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
type TokenSource interface {
	// AccessToken returns a valid access token, or ErrNotConnected.
	AccessToken(ctx context.Context) (string, error)
	// Invalidate marks accessToken as expired after Spotify rejected it, so
	// the next call refreshes. It does nothing if the stored token has
	// changed since.
	Invalidate(ctx context.Context, accessToken string) error
}

type tokenSource struct {
	tokens  Tokens
	oauth   OAuth
	profile ProfileID
	clock   Clock
}

// NewTokenSource returns a TokenSource for the profile.
func NewTokenSource(tokens Tokens, oauth OAuth, id ProfileID, clock Clock) TokenSource {
	return &tokenSource{tokens: tokens, oauth: oauth, profile: id, clock: clock}
}

func (ts *tokenSource) AccessToken(ctx context.Context) (string, error) {
	if ts.profile.IsZero() {
		return "", ErrNotConnected
	}
	c, err := ts.tokens.Connection(ctx, ts.profile)
	if errors.Is(err, ErrNotFound) {
		return "", ErrNotConnected
	}
	if err != nil {
		return "", fmt.Errorf("load connection: %w", err)
	}
	if accessValid(c, ts.clock.Now()) {
		return c.Token().AccessToken(), nil
	}

	var access string
	err = ts.tokens.UpdateConnection(ctx, ts.profile, func(c Connection) (Connection, error) {
		if accessValid(c, ts.clock.Now()) { // refreshed by someone else meanwhile
			access = c.Token().AccessToken()
			return c, nil
		}
		tok, err := ts.oauth.Refresh(ctx, c.Token().RefreshToken())
		if err != nil {
			return nil, err
		}
		access = tok.AccessToken()
		return withToken(c, tok), nil
	})
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrRevoked):
		return "", ErrNotConnected
	case err != nil:
		return "", fmt.Errorf("refresh Spotify token: %w", err)
	}
	return access, nil
}

func (ts *tokenSource) Invalidate(ctx context.Context, accessToken string) error {
	err := ts.tokens.UpdateConnection(ctx, ts.profile, func(c Connection) (Connection, error) {
		t := c.Token()
		if t.AccessToken() != accessToken {
			return c, nil
		}
		expired, err := NewToken(t.AccessToken(), t.RefreshToken(), time.Time{})
		if err != nil {
			return nil, err
		}
		return withToken(c, expired), nil
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
