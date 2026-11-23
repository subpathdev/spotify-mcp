package account_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/subpathdev/spotify-mcp/internal/account"
)

func (e *env) connect(t *testing.T, c account.Connection) {
	t.Helper()
	if err := e.store.SaveConnection(context.Background(), profile, c); err != nil {
		t.Fatal(err)
	}
}

func (e *env) tokens() account.TokenSource {
	return account.NewTokenSource(e.store, e.oauth, profile, clockFunc(func() time.Time { return e.now }))
}

func expired(rt string) account.Connection {
	return connection(token("old", rt, time.Time{}), spotifyAccount("jan", "Jan"), time.Time{})
}

func TestAccessTokenRefreshesAndPersists(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct{ sent, stored string }{
		"rotated": {sent: "r2", stored: "r2"},
		"kept":    {sent: "r1", stored: "r1"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.connect(t, expired("r1"))
			calls := 0
			e.oauth.refresh = func(_ context.Context, rt string) (account.Token, error) {
				calls++
				if rt != "r1" {
					t.Errorf("refreshed with %q", rt)
				}
				return token("new", tc.sent, e.now.Add(time.Hour)), nil
			}
			ts := e.tokens()
			for range 2 {
				if tok, err := ts.AccessToken(ctx); err != nil || tok != "new" {
					t.Fatalf("token %q %v", tok, err)
				}
			}
			if calls != 1 {
				t.Fatalf("refreshed %d times, want once", calls)
			}
			c, _ := e.store.Connection(ctx, profile)
			if c.Token().RefreshToken() != tc.stored || c.Account().DisplayName() != "Jan" {
				t.Fatalf("stored %+v", c)
			}
		})
	}
}

func TestAccessTokenNotConnected(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	if _, err := e.tokens().AccessToken(ctx); !errors.Is(err, account.ErrNotConnected) {
		t.Fatalf("no connection: %v", err)
	}
	e.connect(t, expired("r1"))
	e.oauth.refresh = func(context.Context, string) (account.Token, error) { return nil, account.ErrRevoked }
	if _, err := e.tokens().AccessToken(ctx); !errors.Is(err, account.ErrNotConnected) {
		t.Fatalf("revoked: %v", err)
	}
}

func TestInvalidateForcesRefresh(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.connect(t, connection(token("old", "r1", e.now.Add(time.Hour)), spotifyAccount("jan", "Jan"), time.Time{}))
	e.oauth.refresh = func(context.Context, string) (account.Token, error) {
		return token("new", "r1", e.now.Add(time.Hour)), nil
	}
	ts := e.tokens()
	if err := ts.Invalidate(ctx, "not-the-stored-one"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := ts.AccessToken(ctx); tok != "old" {
		t.Fatalf("invalidating another token changed the stored one: %q", tok)
	}
	if err := ts.Invalidate(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := ts.AccessToken(ctx); tok != "new" {
		t.Fatalf("after invalidate: %q", tok)
	}
}

// A login that lands while a refresh is running must not be overwritten by the
// old account's refreshed token.
func TestLoginDuringRefreshWins(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	e.connect(t, expired("old-account"))

	refreshing, release := make(chan struct{}), make(chan struct{})
	e.oauth.refresh = func(context.Context, string) (account.Token, error) {
		close(refreshing)
		<-release
		return token("old-account-access", "old-account", e.now.Add(time.Hour)), nil
	}
	refreshed := make(chan error, 1)
	go func() {
		_, err := e.tokens().AccessToken(ctx)
		refreshed <- err
	}()
	<-refreshing

	login := connection(token("new-access", "new-account", e.now.Add(time.Hour)), spotifyAccount("someone-else", ""), time.Time{})
	saved := make(chan error, 1)
	go func() { saved <- e.store.SaveConnection(ctx, profile, login) }()
	select {
	case err := <-saved:
		t.Fatalf("login saved during the refresh (err %v); it must wait", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	c, err := e.store.Connection(ctx, profile)
	if err != nil || c.Token().RefreshToken() != "new-account" || c.Account().ID() != "someone-else" {
		t.Fatalf("stored %+v %v, want the new login", c, err)
	}
}
