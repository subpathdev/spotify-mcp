// Package web serves the "Connect Spotify" page: a person opens an invite
// link, presses one button, logs in at Spotify, and the server keeps the
// connection. No terminal, no copying tokens around.
//
// The rules (invites, state, PKCE) live in the account package. This package
// handles HTTP: forms, the cookie that binds a login to the browser, security
// headers and the pages.
package web

import (
	"context"
	_ "embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/subpathdev/spotify-mcp/internal/account"
)

// Connector is what the page needs; *account.Service implements it.
type Connector interface {
	Status(ctx context.Context, code account.InviteCode) (account.Status, error)
	StartLogin(ctx context.Context, code account.InviteCode) (account.Login, error)
	FinishLogin(ctx context.Context, cb account.Callback) (account.Profile, error)
	Disconnect(ctx context.Context, code account.InviteCode) (account.Profile, error)
}

const (
	cookieName   = "spotify_mcp_login"
	callbackPath = "/callback"
	maxForm      = 4 << 10
)

type handler struct {
	c   Connector
	log *slog.Logger
}

// NewHandler returns the page routes (/connect, /callback) and /healthz.
//
// Cross-site POSTs are rejected (Sec-Fetch-Site / Origin), so another page
// can't submit the form with a stolen invite in someone else's browser.
func NewHandler(c Connector, log *slog.Logger) http.Handler {
	h := &handler{c: c, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /connect", h.connectPage)
	mux.HandleFunc("POST /connect", h.connectPost)
	mux.HandleFunc("GET "+callbackPath, h.callback)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return securityHeaders(http.NewCrossOriginProtection().Handler(mux))
}

// securityHeaders: no scripts, forms only to this page and Spotify, no
// framing, and no Referer, so the invite in the URL doesn't leak to Spotify.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' https://accounts.spotify.com; frame-ancestors 'none'; base-uri 'none'")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (h *handler) connectPage(w http.ResponseWriter, r *http.Request) {
	code, err := account.ParseInviteCode(r.URL.Query().Get("invite"))
	if err != nil {
		h.fail(w, err)
		return
	}
	st, err := h.c.Status(r.Context(), code)
	if err != nil {
		h.fail(w, err)
		return
	}
	acc, connected := st.Profile.Account()
	d := pageData{Title: "Connect Spotify", Invite: code.Secret(), ShowForm: true, Connected: connected}
	if connected {
		d.Name = acc.Name()
		if !st.ConnectedAt.IsZero() {
			d.Since = st.ConnectedAt.Format(time.DateOnly)
		}
	}
	h.render(w, http.StatusOK, d)
}

func (h *handler) connectPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	code, err := account.ParseInviteCode(r.PostForm.Get("invite"))
	if err != nil {
		h.fail(w, err)
		return
	}
	switch r.PostForm.Get("action") {
	case "connect":
		login, err := h.c.StartLogin(r.Context(), code)
		if err != nil {
			h.fail(w, err)
			return
		}
		http.SetCookie(w, loginCookie(login.Nonce, int(account.LoginTTL/time.Second)))
		http.Redirect(w, r, login.URL, http.StatusSeeOther)
	case "disconnect":
		profile, err := h.c.Disconnect(r.Context(), code)
		if err != nil {
			h.fail(w, err)
			return
		}
		h.log.Info("spotify account disconnected", "profile", profile.ID().String())
		h.render(w, http.StatusOK, pageData{
			Title:   "Disconnected",
			Message: "The Spotify account is disconnected and its token deleted. To also remove the app on Spotify's side, open spotify.com → Account → Manage apps.",
		})
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
	}
}

func (h *handler) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cb := account.Callback{State: q.Get("state"), Code: q.Get("code"), Error: q.Get("error")}
	if c, err := r.Cookie(cookieName); err == nil {
		cb.Nonce = c.Value
	}
	http.SetCookie(w, loginCookie("", -1))
	profile, err := h.c.FinishLogin(r.Context(), cb)
	if err != nil {
		h.fail(w, err)
		return
	}
	acc, _ := profile.Account()
	h.log.Info("spotify account connected", "profile", profile.ID().String(), "spotify_user", acc.ID)
	msg := "Spotify is connected. You can close this page."
	if name := acc.Name(); name != "" {
		msg = "Spotify is connected as " + name + ". You can close this page."
	}
	h.render(w, http.StatusOK, pageData{Title: "Connected", Message: msg})
}

// loginCookie binds a login to the browser. Only the callback receives it.
func loginCookie(nonce string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: cookieName, Value: nonce, Path: callbackPath, MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	}
}

// failure is how an error is shown to a person.
type failure struct {
	status  int
	title   string
	message string
}

var failures = []struct {
	err error
	failure
}{
	{account.ErrInvalidInvite, failure{http.StatusForbidden, "Link not valid",
		"This link is invalid, was already used, or has expired. Ask for a new one."}},
	{account.ErrLoginExpired, failure{http.StatusBadRequest, "Login expired",
		"This login can't be finished (it took too long, or was started in another browser). Open your invite link again and press the button once more."}},
	{account.ErrLoginCancelled, failure{http.StatusOK, "Not connected",
		"You cancelled the login at Spotify. Nothing was connected. Your invite link still works."}},
	{account.ErrNotAllowlisted, failure{http.StatusForbidden, "Account not allowed yet",
		"This Spotify account isn't on the app's user list. The app owner has to add it under User Management in the Spotify developer dashboard (development mode allows up to 5 accounts). Then open the invite link again."}},
	{account.ErrInvalidClient, failure{http.StatusBadGateway, "Not connected",
		"The server's Spotify app settings (client ID or secret) are wrong. Tell whoever runs it."}},
	{account.ErrTooManyLogins, failure{http.StatusTooManyRequests, "Busy",
		"Too many logins are in progress. Try again in a few minutes."}},
}

func (h *handler) fail(w http.ResponseWriter, err error) {
	for _, f := range failures {
		if errors.Is(err, f.err) {
			h.render(w, f.status, pageData{Title: f.title, Message: f.message, Error: true})
			return
		}
	}
	h.log.Error("connect page", "err", err)
	h.render(w, http.StatusBadGateway, pageData{
		Title:   "Not connected",
		Message: "Something went wrong while connecting to Spotify. Try again with your invite link.",
		Error:   true,
	})
}

type pageData struct {
	Title     string
	Message   string
	Error     bool
	ShowForm  bool
	Invite    string
	Connected bool
	Name      string
	Since     string
}

//go:embed page.html
var pageHTML string

var page = template.Must(template.New("page").Parse(pageHTML))

func (h *handler) render(w http.ResponseWriter, status int, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := page.Execute(w, d); err != nil {
		h.log.Error("render page", "err", err)
	}
}
