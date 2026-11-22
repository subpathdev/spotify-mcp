// Package web serves the "Connect Spotify" page: a person opens an invite
// link, presses one button, logs in at Spotify, and the server keeps the
// resulting refresh token. No terminal, no copying tokens around.
//
// Security notes:
//   - Only someone with a valid one-time invite can connect or disconnect an
//     account. Invites are stored hashed and used up when the login finishes.
//   - The OAuth flow uses state (bound to the browser by a cookie) and PKCE,
//     so a stolen callback URL or code can't be replayed.
//   - Pages send no Referer, so the invite in the URL doesn't leak to Spotify.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/subpathdev/spotify-mcp/internal/store"
)

const (
	cookieName  = "spotify_mcp_login"
	loginTTL    = 10 * time.Minute
	maxPending  = 100
	callbackURL = "/callback"
)

// Handler serves /connect and /callback.
type Handler struct {
	OAuth *oauth2.Config
	Store *store.Store
	Log   *slog.Logger
	// MeURL is where the display name of a new login comes from.
	MeURL string
	// HTTPClient is used for the token exchange and /me. May be nil.
	HTTPClient *http.Client
	// OnConnect is called after a profile's token changed (connect or disconnect).
	OnConnect func(profile string)
	// Now is replaceable in tests.
	Now func() time.Time

	mu      sync.Mutex
	pending map[string]pendingLogin
}

type pendingLogin struct {
	profile  string
	invite   string
	verifier string
	nonce    string
	expires  time.Time
}

// Routes returns the HTTP handler with the page routes and /healthz.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /connect", h.connectPage)
	mux.HandleFunc("POST /connect", h.connectPost)
	mux.HandleFunc("GET "+callbackURL, h.callback)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	// Rejects cross-site POSTs (Sec-Fetch-Site / Origin), so another page can't
	// submit the form with a stolen invite in someone else's browser.
	return securityHeaders(http.NewCrossOriginProtection().Handler(mux))
}

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

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) httpCtx(ctx context.Context) context.Context {
	if h.HTTPClient != nil {
		return context.WithValue(ctx, oauth2.HTTPClient, h.HTTPClient)
	}
	return ctx
}

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

type pageData struct {
	Title     string
	Message   string
	Error     bool
	Invite    string
	Connected bool
	Name      string
	Since     string
	ShowForm  bool
}

func (h *Handler) render(w http.ResponseWriter, status int, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := page.Execute(w, d); err != nil {
		h.Log.Error("render page", "err", err)
	}
}

func (h *Handler) invalidInvite(w http.ResponseWriter) {
	h.render(w, http.StatusForbidden, pageData{
		Title:   "Link not valid",
		Message: "This link is invalid, was already used, or has expired. Ask for a new one.",
		Error:   true,
	})
}

func (h *Handler) connectPage(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("invite")
	profile, err := h.Store.CheckInvite(code)
	if err != nil {
		h.invalidInvite(w)
		return
	}
	d := pageData{Title: "Connect Spotify", Invite: code, ShowForm: true}
	if t, err := h.Store.Token(profile); err == nil {
		d.Connected, d.Name = true, t.DisplayName
		if d.Name == "" {
			d.Name = t.SpotifyUserID
		}
		if !t.ConnectedAt.IsZero() {
			d.Since = t.ConnectedAt.Format("2006-01-02")
		}
	}
	h.render(w, http.StatusOK, d)
}

func (h *Handler) connectPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	code := r.PostForm.Get("invite")
	switch r.PostForm.Get("action") {
	case "connect":
		profile, err := h.Store.CheckInvite(code)
		if err != nil {
			h.invalidInvite(w)
			return
		}
		state, nonce, verifier := randomString(), randomString(), oauth2.GenerateVerifier()
		h.mu.Lock()
		if h.pending == nil {
			h.pending = map[string]pendingLogin{}
		}
		for k, p := range h.pending {
			if h.now().After(p.expires) {
				delete(h.pending, k)
			}
		}
		if len(h.pending) >= maxPending {
			h.mu.Unlock()
			http.Error(w, "too many logins in progress, try again in a few minutes", http.StatusTooManyRequests)
			return
		}
		h.pending[state] = pendingLogin{profile: profile, invite: code, verifier: verifier, nonce: nonce, expires: h.now().Add(loginTTL)}
		h.mu.Unlock()
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: nonce, Path: callbackURL,
			MaxAge: int(loginTTL.Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		})
		// show_dialog: always show Spotify's consent page, so a family member on
		// a shared browser sees which account they're about to connect.
		u := h.OAuth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("show_dialog", "true"))
		http.Redirect(w, r, u, http.StatusSeeOther)
	case "disconnect":
		profile, err := h.Store.UseInvite(code)
		if err != nil {
			h.invalidInvite(w)
			return
		}
		if err := h.Store.DeleteToken(profile); err != nil {
			h.Log.Error("delete token", "profile", profile, "err", err)
			http.Error(w, "could not disconnect", http.StatusInternalServerError)
			return
		}
		h.Log.Info("spotify account disconnected", "profile", profile)
		if h.OnConnect != nil {
			h.OnConnect(profile)
		}
		h.render(w, http.StatusOK, pageData{
			Title:   "Disconnected",
			Message: "The Spotify account is disconnected and its token deleted. To also remove the app on Spotify's side, open spotify.com → Account → Manage apps.",
		})
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
	}
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	h.mu.Lock()
	p, ok := h.pending[state]
	delete(h.pending, state) // one attempt per state
	h.mu.Unlock()
	c, cerr := r.Cookie(cookieName)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: callbackURL, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	if !ok || h.now().After(p.expires) || cerr != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(p.nonce)) != 1 {
		h.render(w, http.StatusBadRequest, pageData{
			Title:   "Login expired",
			Message: "This login can't be finished (it took too long, or was started in another browser). Open your invite link again and press the button once more.",
			Error:   true,
		})
		return
	}
	if e := q.Get("error"); e != "" {
		msg := "Spotify didn't allow the connection (" + e + ")."
		if e == "access_denied" {
			msg = "You cancelled the login at Spotify. Nothing was connected. Your invite link still works."
		}
		h.render(w, http.StatusOK, pageData{Title: "Not connected", Message: msg, Error: true})
		return
	}
	ctx := h.httpCtx(r.Context())
	tok, err := h.OAuth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(p.verifier))
	if err != nil {
		h.Log.Error("token exchange", "err", err)
		msg := "Spotify didn't accept the login. Try again with your invite link."
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_client" {
			msg = "The server's Spotify app settings (client ID or secret) are wrong. Tell whoever runs it."
		}
		h.render(w, http.StatusBadGateway, pageData{Title: "Not connected", Message: msg, Error: true})
		return
	}
	if tok.RefreshToken == "" {
		h.render(w, http.StatusBadGateway, pageData{Title: "Not connected", Message: "Spotify sent no refresh token. Try again.", Error: true})
		return
	}
	me, err := h.fetchMe(ctx, tok)
	if err != nil {
		h.Log.Warn("fetch profile after login", "err", err)
		if errors.Is(err, errNotAllowlisted) {
			h.render(w, http.StatusForbidden, pageData{
				Title:   "Account not allowed yet",
				Message: "This Spotify account isn't on the app's user list. The app owner has to add it under User Management in the Spotify developer dashboard (development mode allows up to 5 accounts). Then open the invite link again.",
				Error:   true,
			})
			return
		}
	}
	if _, err := h.Store.UseInvite(p.invite); err != nil {
		h.invalidInvite(w)
		return
	}
	st := store.Token{
		RefreshToken: tok.RefreshToken, AccessToken: tok.AccessToken, Expiry: tok.Expiry,
		SpotifyUserID: me.ID, DisplayName: me.DisplayName, ConnectedAt: h.now(),
	}
	if err := h.Store.SaveToken(p.profile, st); err != nil {
		h.Log.Error("save token", "profile", p.profile, "err", err)
		http.Error(w, "could not save the login", http.StatusInternalServerError)
		return
	}
	h.Log.Info("spotify account connected", "profile", p.profile, "spotify_user", me.ID)
	if h.OnConnect != nil {
		h.OnConnect(p.profile)
	}
	name := me.DisplayName
	if name == "" {
		name = me.ID
	}
	msg := "Spotify is connected. You can close this page."
	if name != "" {
		msg = "Spotify is connected as " + name + ". You can close this page."
	}
	h.render(w, http.StatusOK, pageData{Title: "Connected", Message: msg})
}

var errNotAllowlisted = errors.New("account not on the development-mode user list")

type me struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

func (h *Handler) fetchMe(ctx context.Context, tok *oauth2.Token) (me, error) {
	var m me
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.MeURL, nil)
	if err != nil {
		return m, err
	}
	resp, err := h.OAuth.Client(ctx, tok).Do(req)
	if err != nil {
		return m, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return m, errNotAllowlisted
	}
	if resp.StatusCode != http.StatusOK {
		return m, fmt.Errorf("GET /me: %s", resp.Status)
	}
	return m, json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&m)
}

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>{{.Title}}</title>
<style>
:root { color-scheme: light dark; --bg:#f6f7f6; --card:#fff; --fg:#1b1f1c; --muted:#5c665f; --accent:#1db954; --accent-fg:#06200f; --err:#b3261e; --line:#d9ded9; }
@media (prefers-color-scheme: dark) { :root { --bg:#111412; --card:#1b1f1c; --fg:#e8ece9; --muted:#a3ada6; --line:#2d332f; --err:#f2b8b5; } }
* { box-sizing: border-box; }
body { margin:0; min-height:100vh; display:grid; place-items:center; background:var(--bg); color:var(--fg); font:16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; padding:16px; }
main { width:100%; max-width:420px; background:var(--card); border:1px solid var(--line); border-radius:16px; padding:28px 24px; }
h1 { font-size:1.4rem; margin:0 0 12px; }
p { margin:0 0 16px; color:var(--muted); }
p.err { color:var(--err); }
.status { padding:12px 14px; border-radius:10px; border:1px solid var(--line); margin-bottom:20px; color:var(--fg); }
button { width:100%; font:inherit; font-weight:600; border:0; border-radius:999px; padding:14px; cursor:pointer; }
.primary { background:var(--accent); color:var(--accent-fg); }
.secondary { background:transparent; color:var(--muted); border:1px solid var(--line); margin-top:12px; }
form { margin:0; }
</style>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
{{if .Message}}<p{{if .Error}} class="err"{{end}}>{{.Message}}</p>{{end}}
{{if .ShowForm}}
  {{if .Connected}}
  <div class="status">Connected as <strong>{{.Name}}</strong>{{if .Since}} since {{.Since}}{{end}}.</div>
  {{else}}
  <p>Press the button and log in with your Spotify account. Afterwards Claude can play, pause and skip music for you. It can't see your playlists or e-mail address.</p>
  {{end}}
  <form method="post" action="/connect">
    <input type="hidden" name="invite" value="{{.Invite}}">
    <button class="primary" name="action" value="connect">{{if .Connected}}Connect a different account{{else}}Connect Spotify{{end}}</button>
    {{if .Connected}}<button class="secondary" name="action" value="disconnect">Disconnect</button>{{end}}
  </form>
{{end}}
</main>
</body>
</html>
`))
