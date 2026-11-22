package web

import (
	"crypto/sha256"
	"encoding/base64"
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

	"golang.org/x/oauth2"

	"github.com/subpathdev/spotify-mcp/internal/store"
)

// fakeAccounts plays accounts.spotify.com and api.spotify.com/v1/me.
type fakeAccounts struct {
	*httptest.Server
	mu        sync.Mutex
	challenge string
	meStatus  int
}

func newFakeAccounts(t *testing.T) *fakeAccounts {
	f := &fakeAccounts{meStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		f.mu.Lock()
		ok := r.PostForm.Get("code") == "good-code" && base64.RawURLEncoding.EncodeToString(sum[:]) == f.challenge
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"a1","token_type":"Bearer","expires_in":3600,"refresh_token":"r1"}`)
	})
	mux.HandleFunc("GET /v1/me", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		st := f.meStatus
		f.mu.Unlock()
		w.WriteHeader(st)
		_, _ = io.WriteString(w, `{"id":"jan","display_name":"Jan"}`)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

type env struct {
	st      *store.Store
	acc     *fakeAccounts
	web     *httptest.Server
	browser *http.Client
	changed []string
}

func setup(t *testing.T) *env {
	e := &env{acc: newFakeAccounts(t)}
	var err error
	if e.st, err = store.Open(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		OAuth: &oauth2.Config{
			ClientID: "id", ClientSecret: "secret", Scopes: []string{"user-modify-playback-state"},
			Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/authorize", TokenURL: e.acc.URL + "/api/token", AuthStyle: oauth2.AuthStyleInHeader},
		},
		Store:      e.st,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MeURL:      e.acc.URL + "/v1/me",
		HTTPClient: e.acc.Client(),
		OnConnect:  func(p string) { e.changed = append(e.changed, p) },
	}
	e.web = httptest.NewTLSServer(h.Routes())
	t.Cleanup(e.web.Close)
	h.OAuth.RedirectURL = e.web.URL + "/callback"
	jar, _ := cookiejar.New(nil)
	e.browser = e.web.Client()
	e.browser.Jar = jar
	// Stop at the redirect to Spotify instead of following it.
	e.browser.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return e
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// startLogin opens the invite page, presses Connect and returns the state
// from the redirect to Spotify.
func (e *env) startLogin(t *testing.T, code string) string {
	t.Helper()
	resp, err := e.browser.Get(e.web.URL + "/connect?invite=" + url.QueryEscape(code))
	if err != nil {
		t.Fatal(err)
	}
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "Connect Spotify") {
		t.Fatalf("invite page: %d %s", resp.StatusCode, b)
	}
	if rp := resp.Header.Get("Referrer-Policy"); rp != "no-referrer" {
		t.Fatalf("Referrer-Policy %q", rp)
	}
	resp, err = e.browser.PostForm(e.web.URL+"/connect", url.Values{"invite": {code}, "action": {"connect"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("connect: %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("show_dialog") != "true" || q.Get("state") == "" {
		t.Fatalf("authorize URL %s", loc)
	}
	e.acc.mu.Lock()
	e.acc.challenge = q.Get("code_challenge")
	e.acc.mu.Unlock()
	return q.Get("state")
}

func TestConnectFlow(t *testing.T) {
	e := setup(t)
	code, _ := e.st.CreateInvite("default", time.Hour)
	state := e.startLogin(t, code)

	resp, err := e.browser.Get(e.web.URL + "/callback?code=good-code&state=" + state)
	if err != nil {
		t.Fatal(err)
	}
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "connected as Jan") {
		t.Fatalf("callback: %d %s", resp.StatusCode, b)
	}
	tok, err := e.st.Token("default")
	if err != nil || tok.RefreshToken != "r1" || tok.DisplayName != "Jan" {
		t.Fatalf("token %+v %v", tok, err)
	}
	if len(e.changed) != 1 {
		t.Fatalf("OnConnect calls %v", e.changed)
	}
	// The invite is used up, and the state can't be replayed.
	if _, err := e.st.CheckInvite(code); err == nil {
		t.Fatal("invite still valid after login")
	}
	resp, _ = e.browser.Get(e.web.URL + "/callback?code=good-code&state=" + state)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed callback: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestCallbackNeedsSameBrowser(t *testing.T) {
	e := setup(t)
	code, _ := e.st.CreateInvite("default", time.Hour)
	state := e.startLogin(t, code)
	// An attacker's browser has no login cookie.
	other := &http.Client{Transport: e.web.Client().Transport}
	resp, err := other.Get(e.web.URL + "/callback?code=good-code&state=" + state)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback from other browser: %d", resp.StatusCode)
	}
	if _, err := e.st.Token("default"); err == nil {
		t.Fatal("token saved from foreign browser")
	}
}

func TestNotOnAllowlist(t *testing.T) {
	e := setup(t)
	e.acc.meStatus = http.StatusForbidden
	code, _ := e.st.CreateInvite("default", time.Hour)
	state := e.startLogin(t, code)
	resp, _ := e.browser.Get(e.web.URL + "/callback?code=good-code&state=" + state)
	if b := body(t, resp); resp.StatusCode != http.StatusForbidden || !strings.Contains(b, "User Management") {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	if _, err := e.st.CheckInvite(code); err != nil {
		t.Fatal("invite used up although the login failed")
	}
}

func TestCancelAtSpotify(t *testing.T) {
	e := setup(t)
	code, _ := e.st.CreateInvite("default", time.Hour)
	state := e.startLogin(t, code)
	resp, _ := e.browser.Get(e.web.URL + "/callback?error=access_denied&state=" + state)
	if b := body(t, resp); !strings.Contains(b, "cancelled") {
		t.Fatalf("cancel page: %s", b)
	}
	if _, err := e.st.CheckInvite(code); err != nil {
		t.Fatal("invite used up by a cancelled login")
	}
}

func TestBadInviteAndCrossSite(t *testing.T) {
	e := setup(t)
	resp, _ := e.browser.Get(e.web.URL + "/connect?invite=nope")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad invite: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	code, _ := e.st.CreateInvite("default", time.Hour)
	req, _ := http.NewRequest(http.MethodPost, e.web.URL+"/connect", strings.NewReader(url.Values{"invite": {code}, "action": {"disconnect"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := e.browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", resp.StatusCode)
	}
}

func TestDisconnect(t *testing.T) {
	e := setup(t)
	_ = e.st.SaveToken("default", store.Token{RefreshToken: "r", DisplayName: "Jan"})
	code, _ := e.st.CreateInvite("default", time.Hour)
	resp, _ := e.browser.Get(e.web.URL + "/connect?invite=" + code)
	if b := body(t, resp); !strings.Contains(b, "Connected as <strong>Jan</strong>") {
		t.Fatalf("status page: %s", b)
	}
	resp, _ = e.browser.PostForm(e.web.URL+"/connect", url.Values{"invite": {code}, "action": {"disconnect"}})
	if b := body(t, resp); resp.StatusCode != 200 || !strings.Contains(b, "Disconnected") {
		t.Fatalf("disconnect: %d %s", resp.StatusCode, b)
	}
	if _, err := e.st.Token("default"); err == nil {
		t.Fatal("token still there")
	}
}
