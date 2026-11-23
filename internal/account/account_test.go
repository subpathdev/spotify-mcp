package account_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/subpathdev/spotify-mcp/internal/account"
	"github.com/subpathdev/spotify-mcp/internal/filestore"
)

var profile = mustProfileID("default")

// mustProfileID parses a profile ID that the test knows to be valid.
func mustProfileID(s string) account.ProfileID {
	id, err := account.ParseProfileID(s)
	if err != nil {
		panic(err)
	}
	return id
}

// fakeOAuth issues tokens for the code "good" and the verifier of the last
// started login.
type fakeOAuth struct {
	mu        sync.Mutex
	verifiers map[string]string // by state
	refresh   func(ctx context.Context, rt string) (account.Token, error)
}

func (f *fakeOAuth) AuthCodeURL(state, verifier string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.verifiers == nil {
		f.verifiers = map[string]string{}
	}
	f.verifiers[state] = verifier
	return "https://auth.test/authorize?" + url.Values{"state": {state}}.Encode()
}

func (f *fakeOAuth) Exchange(_ context.Context, code, verifier string) (account.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.verifiers {
		if v == verifier && code == "good" {
			return account.Token{AccessToken: "a1", RefreshToken: "r1", Expiry: time.Now().Add(time.Hour)}, nil
		}
	}
	return account.Token{}, errors.New("bad code or verifier")
}

func (f *fakeOAuth) Refresh(ctx context.Context, rt string) (account.Token, error) {
	return f.refresh(ctx, rt)
}

type fakeIdentity struct{ err error }

func (f fakeIdentity) Account(context.Context, string) (account.SpotifyAccount, error) {
	return account.SpotifyAccount{ID: "jan", DisplayName: "Jan"}, f.err
}

type env struct {
	store   *filestore.Store
	oauth   *fakeOAuth
	inviter *account.Inviter
	svc     *account.Service
	now     time.Time
}

func newEnv(t *testing.T, identityErr error) *env {
	t.Helper()
	st, err := filestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{store: st, oauth: &fakeOAuth{}, now: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
	clock := account.WithClock(func() time.Time { return e.now })
	e.inviter = account.NewInviter(st, clock)
	e.svc = account.NewService(st, st, e.oauth, fakeIdentity{err: identityErr}, clock)
	return e
}

func (e *env) invite(t *testing.T) account.InviteCode {
	t.Helper()
	code, _, err := e.inviter.CreateInvite(context.Background(), profile, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// start begins a login and returns the callback a browser would deliver.
func (e *env) start(t *testing.T, code account.InviteCode) account.Callback {
	t.Helper()
	login, err := e.svc.StartLogin(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(login.URL)
	if err != nil {
		t.Fatal(err)
	}
	return account.Callback{State: u.Query().Get("state"), Nonce: login.Nonce, Code: "good"}
}

func TestLoginConnectsAndUsesUpInvite(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	code := e.invite(t)

	st, err := e.svc.Status(ctx, code)
	if err != nil || st.Profile.ID() != profile || !st.ConnectedAt.IsZero() {
		t.Fatalf("status before: %+v %v", st, err)
	}
	if _, connected := st.Profile.Account(); connected {
		t.Fatal("connected before login")
	}
	p, err := e.svc.FinishLogin(ctx, e.start(t, code))
	if err != nil || p.ID() != profile {
		t.Fatalf("finish: %v %v", p, err)
	}
	if acc, ok := p.Account(); !ok || acc.Name() != "Jan" {
		t.Fatalf("finish account: %+v %v", acc, ok)
	}
	c, err := e.store.Connection(ctx, profile)
	if err != nil || c.Token.RefreshToken != "r1" || c.Account.ID != "jan" || !c.ConnectedAt.Equal(e.now) {
		t.Fatalf("stored: %+v %v", c, err)
	}
	if _, err := e.svc.Status(ctx, code); !errors.Is(err, account.ErrInvalidInvite) {
		t.Fatalf("invite still valid after login: %v", err)
	}
}

func TestFinishLoginNeedsSameBrowserOnce(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	code := e.invite(t)

	cb := e.start(t, code)
	foreign := cb
	foreign.Nonce = "someone-else"
	if _, err := e.svc.FinishLogin(ctx, foreign); !errors.Is(err, account.ErrLoginExpired) {
		t.Fatalf("foreign browser: %v", err)
	}
	// The failed attempt used up the state, so the real browser must start again.
	if _, err := e.svc.FinishLogin(ctx, cb); !errors.Is(err, account.ErrLoginExpired) {
		t.Fatalf("replayed state: %v", err)
	}

	cb = e.start(t, code)
	e.now = e.now.Add(account.LoginTTL + time.Second)
	if _, err := e.svc.FinishLogin(ctx, cb); !errors.Is(err, account.ErrLoginExpired) {
		t.Fatalf("late callback: %v", err)
	}
}

func TestFailedLoginsKeepInvite(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		identityErr error
		spotifyErr  string
		want        error
	}{
		"cancelled":       {spotifyErr: "access_denied", want: account.ErrLoginCancelled},
		"not allowlisted": {identityErr: account.ErrNotAllowlisted, want: account.ErrNotAllowlisted},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, tc.identityErr)
			code := e.invite(t)
			cb := e.start(t, code)
			cb.Error = tc.spotifyErr
			if _, err := e.svc.FinishLogin(ctx, cb); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if _, err := e.svc.Status(ctx, code); err != nil {
				t.Fatalf("invite used up: %v", err)
			}
		})
	}
}

func TestDisconnect(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	if _, err := e.svc.FinishLogin(ctx, e.start(t, e.invite(t))); err != nil {
		t.Fatal(err)
	}
	code := e.invite(t)
	st, err := e.svc.Status(ctx, code)
	if err != nil || !st.ConnectedAt.Equal(e.now) {
		t.Fatalf("status: %+v %v", st, err)
	}
	if acc, ok := st.Profile.Account(); !ok || acc.Name() != "Jan" {
		t.Fatalf("status account: %+v %v", acc, ok)
	}
	p, err := e.svc.Disconnect(ctx, code)
	if err != nil || p.ID() != profile {
		t.Fatalf("disconnect: %v %v", p, err)
	}
	if _, ok := p.Account(); ok {
		t.Fatal("account kept after disconnect")
	}
	if _, err := e.store.Connection(ctx, profile); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("connection kept: %v", err)
	}
	if _, err := e.svc.Disconnect(ctx, code); !errors.Is(err, account.ErrInvalidInvite) {
		t.Fatalf("second disconnect: %v", err)
	}
}

func TestInvites(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	if _, err := e.svc.Status(ctx, nil); !errors.Is(err, account.ErrInvalidInvite) {
		t.Fatalf("nil code: %v", err)
	}
	unknown, err := account.ParseInviteCode(strings.Repeat("a", 43))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Status(ctx, unknown); !errors.Is(err, account.ErrInvalidInvite) {
		t.Fatalf("unknown code: %v", err)
	}
	code := e.invite(t)
	e.now = e.now.Add(2 * time.Hour)
	if _, err := e.svc.StartLogin(ctx, code); !errors.Is(err, account.ErrInvalidInvite) {
		t.Fatalf("expired invite accepted: %v", err)
	}
	if _, _, err := e.inviter.CreateInvite(ctx, nil, time.Hour); err == nil {
		t.Fatal("nil profile accepted")
	}
	if _, _, err := e.inviter.CreateInvite(ctx, profile, 0); err == nil {
		t.Fatal("zero TTL accepted")
	}
}

func TestParseProfileID(t *testing.T) {
	for _, s := range []string{"", "A", "../x", "a/b", "-a", string(make([]byte, 64))} {
		if _, err := account.ParseProfileID(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	a, err := account.ParseProfileID("jan-2")
	if err != nil || a.String() != "jan-2" {
		t.Fatalf("valid profile: %v %v", a, err)
	}
	if b := mustProfileID("jan-2"); a != b {
		t.Fatal("equal IDs compare unequal")
	}
	if a == profile {
		t.Fatal("different IDs compare equal")
	}
}

func TestParseInviteCode(t *testing.T) {
	for _, s := range []string{"", "unknown", strings.Repeat("a", 42), strings.Repeat("a", 44), strings.Repeat("a", 42) + "="} {
		if _, err := account.ParseInviteCode(s); !errors.Is(err, account.ErrInvalidInvite) {
			t.Errorf("%q: %v", s, err)
		}
	}
	code, err := account.ParseInviteCode(strings.Repeat("a", 42) + "_")
	if err != nil {
		t.Fatal(err)
	}
	if s := fmt.Sprint(code); strings.Contains(s, "aaaa") {
		t.Fatalf("code printed: %s", s)
	}
}

func TestRestoreInvite(t *testing.T) {
	hash, exp := strings.Repeat("0", 64), time.Now()
	for name, tc := range map[string]struct {
		hash, profile string
		exp           time.Time
	}{
		"short hash":  {"abc", "default", exp},
		"upper hash":  {strings.Repeat("A", 64), "default", exp},
		"bad profile": {hash, "../x", exp},
		"no expiry":   {hash, "default", time.Time{}},
	} {
		if _, err := account.RestoreInvite(tc.hash, tc.profile, tc.exp); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	inv, err := account.RestoreInvite(hash, "default", exp)
	if err != nil || inv.Hash().String() != hash || inv.ProfileID() != profile || !inv.Expires().Equal(exp) {
		t.Fatalf("restore: %v %v", inv, err)
	}
}

func TestProfiles(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	profiles := account.NewProfiles(e.store)
	p, err := profiles.Profile(ctx, profile)
	if err != nil || p.ID() != profile {
		t.Fatalf("profile: %v %v", p, err)
	}
	if _, ok := p.Account(); ok {
		t.Fatal("account without a connection")
	}
	if _, err := e.svc.FinishLogin(ctx, e.start(t, e.invite(t))); err != nil {
		t.Fatal(err)
	}
	// A loaded profile is a snapshot: p stays as it was, a new load sees the login.
	if _, ok := p.Account(); ok {
		t.Fatal("snapshot changed")
	}
	p, err = profiles.Profile(ctx, profile)
	if acc, ok := p.Account(); err != nil || !ok || acc.ID != "jan" {
		t.Fatalf("after login: %+v %v %v", acc, ok, err)
	}
}
