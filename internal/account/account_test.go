package account_test

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/subpathdev/spotify-mcp/internal/account"
	"github.com/subpathdev/spotify-mcp/internal/filestore"
)

var profile = mustProfile("default")

// mustProfile parses a profile ID that the test knows to be valid.
func mustProfile(id string) account.Profile {
	p, err := account.ParseProfile(id)
	if err != nil {
		panic(err)
	}
	return p
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

func (f fakeIdentity) User(context.Context, string) (account.User, error) {
	return account.User{ID: "jan", DisplayName: "Jan"}, f.err
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
	if err != nil || st.Connected || st.Profile.ID() != profile.ID() {
		t.Fatalf("status before: %+v %v", st, err)
	}
	user, err := e.svc.FinishLogin(ctx, e.start(t, code))
	if err != nil || user.Name() != "Jan" {
		t.Fatalf("finish: %+v %v", user, err)
	}
	c, err := e.store.Connection(ctx, profile)
	if err != nil || c.Token.RefreshToken != "r1" || c.User.ID != "jan" || !c.ConnectedAt.Equal(e.now) {
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
	if st, _ := e.svc.Status(ctx, code); !st.Connected || st.User.Name() != "Jan" {
		t.Fatalf("status: %+v", st)
	}
	if p, err := e.svc.Disconnect(ctx, code); err != nil || p.ID() != profile.ID() {
		t.Fatalf("disconnect: %q %v", p, err)
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
	for _, code := range []account.InviteCode{"", "unknown"} {
		if _, err := e.svc.Status(ctx, code); !errors.Is(err, account.ErrInvalidInvite) {
			t.Fatalf("code %q: %v", code, err)
		}
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

func TestParseProfile(t *testing.T) {
	for _, s := range []string{"", "A", "../x", "a/b", "-a", string(make([]byte, 64))} {
		if _, err := account.ParseProfile(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if p, err := account.ParseProfile("jan-2"); err != nil || p.ID() != "jan-2" {
		t.Fatalf("valid profile: %v %v", p, err)
	}
}
