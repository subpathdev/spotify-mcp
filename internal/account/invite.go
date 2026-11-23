package account

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Inviter creates one-time invite links. It needs no Spotify access, so the
// invite command can run it on its own.
type Inviter struct {
	invites Invites
	now     func() time.Time
}

// NewInviter returns an Inviter storing into invites.
func NewInviter(invites Invites, opts ...Option) *Inviter {
	return &Inviter{invites: invites, now: apply(opts).now}
}

// CreateInvite returns a new one-time code for the profile and when it expires.
func (i *Inviter) CreateInvite(ctx context.Context, id ProfileID, ttl time.Duration) (InviteCode, time.Time, error) {
	if id == nil {
		return nil, time.Time{}, errors.New("invite needs a profile")
	}
	if ttl <= 0 {
		return nil, time.Time{}, fmt.Errorf("invite TTL must be positive, got %s", ttl)
	}
	code := newInviteCode()
	now := i.now()
	inv := &invite{hash: hashInvite(code), profileID: id, expires: now.Add(ttl)}
	if err := i.invites.PruneInvites(ctx, now); err != nil {
		return nil, time.Time{}, fmt.Errorf("prune invites: %w", err)
	}
	if err := i.invites.AddInvite(ctx, inv); err != nil {
		return nil, time.Time{}, fmt.Errorf("save invite: %w", err)
	}
	return code, inv.expires, nil
}
