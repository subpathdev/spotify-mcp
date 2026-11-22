package filestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/subpathdev/spotify-mcp/internal/account"
)

const profile = account.Profile("default")

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConnectionRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := s.Connection(ctx, profile); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("empty store: %v", err)
	}
	want := account.Connection{
		Token:       account.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Date(2026, 11, 22, 13, 0, 0, 0, time.UTC)},
		User:        account.User{ID: "jan", DisplayName: "Jan"},
		ConnectedAt: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC),
	}
	if err := s.SaveConnection(ctx, profile, want); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "tokens", "default.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	// A second store on the same dir, as after a pod restart, sees it.
	got, err := open(t, dir).Connection(ctx, profile)
	if err != nil || got != want {
		t.Fatalf("got %+v %v, want %+v", got, err, want)
	}
	for range 2 {
		if err := s.DeleteConnection(ctx, profile); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpdateConnection(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	noop := func(*account.Connection) error { return nil }
	if err := s.UpdateConnection(ctx, profile, noop); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	_ = s.SaveConnection(ctx, profile, account.Connection{Token: account.Token{RefreshToken: "r1"}})
	boom := errors.New("boom")
	err := s.UpdateConnection(ctx, profile, func(c *account.Connection) error {
		c.Token.RefreshToken = "lost"
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("fn error: %v", err)
	}
	if c, _ := s.Connection(ctx, profile); c.Token.RefreshToken != "r1" {
		t.Fatalf("failed update was written: %+v", c)
	}
}

func TestRejectsBadProfile(t *testing.T) {
	s := open(t, t.TempDir())
	if err := s.SaveConnection(context.Background(), "../x", account.Connection{}); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestInviteTakenOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	inv := account.Invite{Hash: "h1", Profile: profile, Expires: time.Now().Add(time.Hour)}
	if err := s.AddInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Invite(ctx, "h1"); err != nil || got.Profile != profile {
		t.Fatalf("invite: %+v %v", got, err)
	}

	// Half go through a second Store, like the invite command's process.
	other := open(t, dir)
	var wg sync.WaitGroup
	var mu sync.Mutex
	taken := 0
	for i := range 20 {
		st := s
		if i%2 == 1 {
			st = other
		}
		wg.Go(func() {
			if _, err := st.TakeInvite(ctx, "h1"); err == nil {
				mu.Lock()
				taken++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if taken != 1 {
		t.Fatalf("taken %d times, want once", taken)
	}
}

func TestConcurrentInvitesFromTwoStores(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a, b := open(t, dir), open(t, dir)
	var wg sync.WaitGroup
	for i := range 20 {
		st := a
		if i%2 == 1 {
			st = b
		}
		wg.Go(func() {
			h := account.InviteHash(strings.Repeat("x", i+1))
			if err := st.AddInvite(ctx, account.Invite{Hash: h, Profile: profile, Expires: time.Now().Add(time.Hour)}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for i := range 20 {
		if _, err := a.Invite(ctx, account.InviteHash(strings.Repeat("x", i+1))); err != nil {
			t.Errorf("invite %d lost: %v", i, err)
		}
	}
}

func TestPruneInvites(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	now := time.Now()
	_ = s.AddInvite(ctx, account.Invite{Hash: "old", Profile: profile, Expires: now.Add(-time.Minute)})
	_ = s.AddInvite(ctx, account.Invite{Hash: "new", Profile: profile, Expires: now.Add(time.Minute)})
	if err := s.PruneInvites(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Invite(ctx, "old"); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("expired invite kept: %v", err)
	}
	if _, err := s.Invite(ctx, "new"); err != nil {
		t.Fatalf("valid invite pruned: %v", err)
	}
}
