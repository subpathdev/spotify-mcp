package web_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/subpathdev/spotify-mcp/internal/account"
	"github.com/subpathdev/spotify-mcp/internal/filestore"
	"github.com/subpathdev/spotify-mcp/internal/web"
)

// fakeOAuth accepts the code "good" with the verifier of a started login.
type fakeOAuth struct {
	mu        sync.Mutex
	verifiers map[string]string
}

func (f *fakeOAuth) AuthCodeURL(state, verifier string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifiers[state] = verifier
	return "https://accounts.test/authorize?" + url.Values{"state": {state}}.Encode()
}

func (f *fakeOAuth) Exchange(_ context.Context, code, verifier string) (account.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.verifiers {
		if v == verifier && code == "good" {
			return account.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)}, nil
		}
	}
	return account.Token{}, errors.New("bad code")
}

func (f *fakeOAuth) Refresh(context.Context, string) (account.Token, error) {
	return account.Token{}, errors.New("unused")
}

type fakeIdentity struct{ err error }

func (f *fakeIdentity) User(context.Context, string) (account.User, error) {
	return account.User{ID: "jan", DisplayName: "Jan"}, f.err
}

type env struct {
	srv      *httptest.Server
	inviter  *account.Inviter
	store    *filestore.Store
	identity *fakeIdentity
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := filestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := &fakeIdentity{}
	svc := account.NewService(st, st, &fakeOAuth{verifiers: map[string]string{}}, id)
	srv := httptest.NewTLSServer(web.NewHandler(svc, slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	return &env{srv: srv, inviter: account.NewInviter(st), store: st, identity: id}
}

func (e *env) invite(t *testing.T) string {
	t.Helper()
	code, _, err := e.inviter.CreateInvite(context.Background(), "default", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return string(code)
}

// browser is a client with its own cookies that doesn't follow redirects.
func (e *env) browser(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Transport:     e.srv.Client().Transport,
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body(t, resp)
}

func post(t *testing.T, c *http.Client, u string, form url.Values) *http.Response {
	t.Helper()
	resp, err := c.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// connect presses the button and returns the callback URL Spotify would
// redirect to.
func (e *env) connect(t *testing.T, c *http.Client, code string) string {
	t.Helper()
	resp := post(t, c, e.srv.URL+"/connect", url.Values{"invite": {code}, "action": {"connect"}})
	_ = body(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("connect: %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	return e.srv.URL + "/callback?" + url.Values{"state": {loc.Query().Get("state")}, "code": {"good"}}.Encode()
}

func TestConnectFlow(t *testing.T) {
	e := newEnv(t)
	b := e.browser(t)
	code := e.invite(t)

	status, page := get(t, b, e.srv.URL+"/connect?invite="+code)
	if status != http.StatusOK || !strings.Contains(page, "Connect Spotify") {
		t.Fatalf("page: %d %s", status, page)
	}
	callback := e.connect(t, b, code)

	// Another browser with the callback URL can't finish the login, and the
	// attempt uses up the state.
	if status, _ := get(t, e.browser(t), callback); status != http.StatusBadRequest {
		t.Fatalf("foreign browser: %d", status)
	}
	callback = e.connect(t, b, code)
	status, page = get(t, b, callback)
	if status != http.StatusOK || !strings.Contains(page, "connected as Jan") {
		t.Fatalf("callback: %d %s", status, page)
	}
	if status, _ := get(t, b, callback); status != http.StatusBadRequest {
		t.Fatalf("replay: %d", status)
	}
	if status, _ := get(t, b, e.srv.URL+"/connect?invite="+code); status != http.StatusForbidden {
		t.Fatalf("used invite: %d", status)
	}
	if _, err := e.store.Connection(context.Background(), "default"); err != nil {
		t.Fatalf("not stored: %v", err)
	}
}

func TestNotAllowlistedPage(t *testing.T) {
	e := newEnv(t)
	e.identity.err = account.ErrNotAllowlisted
	b := e.browser(t)
	status, page := get(t, b, e.connect(t, b, e.invite(t)))
	if status != http.StatusForbidden || !strings.Contains(page, "User Management") {
		t.Fatalf("%d %s", status, page)
	}
}

func TestDisconnect(t *testing.T) {
	e := newEnv(t)
	b := e.browser(t)
	if status, _ := get(t, b, e.connect(t, b, e.invite(t))); status != http.StatusOK {
		t.Fatal("connect failed")
	}
	code := e.invite(t)
	if _, page := get(t, b, e.srv.URL+"/connect?invite="+code); !strings.Contains(page, "Connected as <strong>Jan</strong>") {
		t.Fatalf("status page: %s", page)
	}
	resp := post(t, b, e.srv.URL+"/connect", url.Values{"invite": {code}, "action": {"disconnect"}})
	if page := body(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(page, "Disconnected") {
		t.Fatalf("disconnect: %d %s", resp.StatusCode, page)
	}
	if _, err := e.store.Connection(context.Background(), "default"); !errors.Is(err, account.ErrNotFound) {
		t.Fatalf("connection kept: %v", err)
	}
}

func TestCrossSitePostRejected(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/connect", strings.NewReader("action=connect&invite="+e.invite(t)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := e.browser(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	resp, err := e.browser(t).Get(e.srv.URL + "/connect?invite=bad")
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, resp)
	if resp.StatusCode != http.StatusForbidden ||
		resp.Header.Get("Referrer-Policy") != "no-referrer" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
}
