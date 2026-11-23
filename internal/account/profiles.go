package account

import (
	"context"
	"errors"
	"fmt"
)

// Profiles loads profiles with what is known about them right now.
type Profiles interface {
	// Profile returns the profile with its connected Spotify account, if any.
	// A profile without a connection is not an error.
	Profile(ctx context.Context, id ProfileID) (Profile, error)
}

// NewProfiles returns Profiles that reads connections from tokens.
func NewProfiles(tokens Tokens) Profiles {
	return profiles{tokens: tokens}
}

type profiles struct {
	tokens Tokens
}

func (p profiles) Profile(ctx context.Context, id ProfileID) (Profile, error) {
	return loadProfile(ctx, p.tokens, id)
}

func loadProfile(ctx context.Context, tokens Tokens, id ProfileID) (*profile, error) {
	if id == nil {
		return nil, errors.New("no profile given")
	}
	c, err := tokens.Connection(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return newProfile(id, nil), nil
	case err != nil:
		return nil, fmt.Errorf("load connection: %w", err)
	}
	return newProfile(id, c), nil
}
