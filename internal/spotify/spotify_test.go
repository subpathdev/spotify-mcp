package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"

	"github.com/subpathdev/spotify-mcp/internal/account"
	"github.com/subpathdev/spotify-mcp/internal/playback"
)

type fakeTokens struct {
	mu          sync.Mutex
	tok         string
	invalidated []string
}

func (f *fakeTokens) AccessToken(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tok, nil
}

func (f *fakeTokens) Invalidate(_ context.Context, tok string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated = append(f.invalidated, tok)
	f.tok = "fresh"
	return nil
}

type call struct{ method, path, query, body, auth string }

// fakeAPI records calls and answers with the handler for "METHOD /path".
type fakeAPI struct {
	mu     sync.Mutex
	calls  []call
	routes map[string]http.HandlerFunc
}

func newPlayer(t *testing.T, routes map[string]http.HandlerFunc) (*Player, *fakeAPI, *fakeTokens) {
	t.Helper()
	f := &fakeAPI{routes: routes}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, call{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Get("Authorization")})
		f.mu.Unlock()
		if h, ok := f.routes[r.Method+" "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	tokens := &fakeTokens{tok: "t1"}
	return NewPlayer(NewAPI(srv.URL, srv.Client()), tokens), f, tokens
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeAPI) last() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func spotifyError(status int, reason string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"status": status, "message": "nope", "reason": reason}})
	}
}

func mustURI(t *testing.T, s string) playback.URI {
	t.Helper()
	u, err := playback.ParseURI(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPlayBodies(t *testing.T) {
	ctx := context.Background()
	p, api, _ := newPlayer(t, nil)
	for uri, want := range map[string]string{
		"spotify:track:1":    `{"uris":["spotify:track:1"]}`,
		"spotify:episode:2":  `{"uris":["spotify:episode:2"]}`,
		"spotify:album:3":    `{"context_uri":"spotify:album:3"}`,
		"spotify:playlist:4": `{"context_uri":"spotify:playlist:4"}`,
	} {
		if err := p.Play(ctx, "dev1", mustURI(t, uri)); err != nil {
			t.Fatal(err)
		}
		c := api.last()
		if c.method != http.MethodPut || c.path != "/me/player/play" || c.query != "device_id=dev1" || c.body != want || c.auth != "Bearer t1" {
			t.Errorf("%s: %+v", uri, c)
		}
	}
	if err := p.Resume(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if c := api.last(); c.body != "" || c.query != "" {
		t.Errorf("resume: %+v", c)
	}
	if err := p.Transfer(ctx, "ph1", true); err != nil {
		t.Fatal(err)
	}
	if c := api.last(); c.path != "/me/player" || c.body != `{"device_ids":["ph1"],"play":true}` {
		t.Errorf("transfer: %+v", c)
	}
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	p, _, _ := newPlayer(t, map[string]http.HandlerFunc{
		"PUT /me/player/pause": spotifyError(404, "NO_ACTIVE_DEVICE"),
		"POST /me/player/next": spotifyError(403, "PREMIUM_REQUIRED"),
		"POST /me/player/previous": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		},
	})
	if err := p.Pause(ctx, ""); !errors.Is(err, playback.ErrNoActiveDevice) {
		t.Errorf("pause: %v", err)
	}
	if err := p.Next(ctx, ""); !errors.Is(err, playback.ErrPremiumRequired) {
		t.Errorf("next: %v", err)
	}
	var apiErr *Error
	if err := p.Previous(ctx, ""); !errors.As(err, &apiErr) || apiErr.Status != 429 || !strings.Contains(err.Error(), "7 s") {
		t.Errorf("previous: %v", err)
	}
}

func TestUnauthorizedRetriesOnceWithFreshToken(t *testing.T) {
	ctx := context.Background()
	p, api, tokens := newPlayer(t, map[string]http.HandlerFunc{
		"PUT /me/player/pause": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer fresh" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		},
	})
	if err := p.Pause(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if len(tokens.invalidated) != 1 || tokens.invalidated[0] != "t1" || api.count() != 2 {
		t.Fatalf("invalidated %v after %d calls", tokens.invalidated, api.count())
	}
}

func TestSearchMapsItems(t *testing.T) {
	p, api, _ := newPlayer(t, map[string]http.HandlerFunc{
		"GET /search": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"tracks":{"items":[null,
				{"type":"track","name":"Song","uri":"spotify:track:1","duration_ms":61000,
				 "artists":[{"name":"A"},{"name":"B"}],"album":{"name":"LP"}},
				{"type":"track","name":"Local","uri":"spotify:local:x:y:z"}]}}`)
		},
	})
	items, err := p.Search(context.Background(), "song", playback.KindTrack, 50)
	if err != nil {
		t.Fatal(err)
	}
	if q := api.last().query; !strings.Contains(q, "limit=10") || !strings.Contains(q, "type=track") {
		t.Errorf("query %q", q)
	}
	if len(items) != 1 {
		t.Fatalf("items %+v", items)
	}
	it := items[0]
	if it.Name != "Song" || it.URI.String() != "spotify:track:1" || strings.Join(it.Artists, ",") != "A,B" || it.Album != "LP" || it.Duration.Seconds() != 61 {
		t.Fatalf("item %+v", it)
	}
}

func TestStateNothingPlaying(t *testing.T) {
	p, _, _ := newPlayer(t, nil) // every call answers 204
	st, err := p.State(context.Background())
	if err != nil || st != nil {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestAccountNotAllowlisted(t *testing.T) {
	srv := httptest.NewServer(spotifyError(http.StatusForbidden, ""))
	defer srv.Close()
	if _, err := NewAPI(srv.URL, srv.Client()).Account(context.Background(), "t"); !errors.Is(err, account.ErrNotAllowlisted) {
		t.Fatalf("got %v", err)
	}
}

func TestOAuthErrors(t *testing.T) {
	ctx := context.Background()
	var answer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if answer != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"`+answer+`"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"a2","token_type":"Bearer","expires_in":3600,"refresh_token":"r1"}`)
	}))
	defer srv.Close()
	o := NewOAuth(OAuthConfig{
		ClientID: "id", ClientSecret: "secret", HTTPClient: srv.Client(),
		Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInHeader},
	})

	tok, err := o.Refresh(ctx, "r1")
	if err != nil || tok.AccessToken != "a2" || tok.RefreshToken != "" {
		t.Fatalf("unchanged refresh token must be reported as empty: %+v %v", tok, err)
	}
	answer = "invalid_grant"
	if _, err := o.Refresh(ctx, "r1"); !errors.Is(err, account.ErrRevoked) {
		t.Fatalf("invalid_grant: %v", err)
	}
	answer = "invalid_client"
	if _, err := o.Exchange(ctx, "code", "verifier"); !errors.Is(err, account.ErrInvalidClient) {
		t.Fatalf("invalid_client: %v", err)
	}
}
