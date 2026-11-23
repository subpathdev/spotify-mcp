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

// CreateInvite returns a new one-time code for p and when it expires.
func (i *Inviter) CreateInvite(ctx context.Context, p Profile, ttl time.Duration) (InviteCode, time.Time, error) {
	if p == nil {
		return "", time.Time{}, errors.New("invite needs a profile")
	}
	if ttl <= 0 {
		return "", time.Time{}, fmt.Errorf("invite TTL must be positive, got %s", ttl)
	}
	code := InviteCode(randomString())
	now := i.now()
	inv := Invite{Hash: hashInvite(code), Profile: p, Expires: now.Add(ttl)}
	if err := i.invites.PruneInvites(ctx, now); err != nil {
		return "", time.Time{}, fmt.Errorf("prune invites: %w", err)
	}
	if err := i.invites.AddInvite(ctx, inv); err != nil {
		return "", time.Time{}, fmt.Errorf("save invite: %w", err)
	}
	return code, inv.Expires, nil
}
