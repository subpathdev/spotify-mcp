package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/subpathdev/spotify-mcp/internal/store"
)

type fakeSpotify struct {
	*httptest.Server
	refreshes atomic.Int32
	lastBody  atomic.Value
	lastPath  atomic.Value
}

func newFake(t *testing.T) *fakeSpotify {
	f := &fakeSpotify{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "r1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Refresh token revoked"}`)
			return
		}
		f.refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// Spotify rotates the refresh token here.
		_, _ = io.WriteString(w, `{"access_token":"a2","token_type":"Bearer","expires_in":3600,"refresh_token":"r2"}`)
	})
	api := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer a2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.lastBody.Store(string(b))
		f.lastPath.Store(r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery)
		switch r.URL.Path {
		case "/v1/me/player/devices":
			_, _ = io.WriteString(w, `{"devices":[{"id":"d1","name":"Pixel","type":"Smartphone","is_active":false}]}`)
		case "/v1/me/player/pause":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"status":404,"message":"Player command failed: No active device found","reason":"NO_ACTIVE_DEVICE"}}`)
		case "/v1/me/player":
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "/v1/search":
			if r.URL.Query().Get("limit") != "10" {
				t.Errorf("search limit %q, want clamped to 10", r.URL.Query().Get("limit"))
			}
			_, _ = io.WriteString(w, `{"tracks":{"items":[{"type":"track","name":"Song","uri":"spotify:track:1","artists":[{"name":"A"},{"name":"B"}],"album":{"name":"Al"}},null]}}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
	mux.HandleFunc("/v1/", api)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func setup(t *testing.T) (*Client, *store.Store, *fakeSpotify) {
	f := newFake(t)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// An expired access token, so the first call has to refresh.
	if err := st.SaveToken("default", store.Token{RefreshToken: "r1", AccessToken: "old", Expiry: time.Now().Add(-time.Hour), DisplayName: "Jan"}); err != nil {
		t.Fatal(err)
	}
	cfg := &oauth2.Config{ClientID: "id", ClientSecret: "secret", Endpoint: oauth2.Endpoint{TokenURL: f.URL + "/api/token", AuthStyle: oauth2.AuthStyleInHeader}}
	c := NewClient(cfg, st, "default", f.Client())
	c.SetAPIBase(f.URL + "/v1")
	return c, st, f
}

func TestRefreshIsPersisted(t *testing.T) {
	c, st, f := setup(t)
	ctx := context.Background()
	devs, err := c.Devices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].Name != "Pixel" {
		t.Fatalf("devices: %+v", devs)
	}
	got, _ := st.Token("default")
	if got.RefreshToken != "r2" || got.AccessToken != "a2" || got.DisplayName != "Jan" {
		t.Fatalf("rotated token not saved (or profile info lost): %+v", got)
	}
	// Cached: no second refresh.
	if _, err := c.Devices(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.refreshes.Load(); n != 1 {
		t.Fatalf("refreshes = %d, want 1", n)
	}
}

func TestRevokedTokenMeansNotConnected(t *testing.T) {
	c, st, _ := setup(t)
	_ = st.SaveToken("default", store.Token{RefreshToken: "revoked"})
	c.Forget()
	if _, err := c.Devices(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("got %v, want ErrNotConnected", err)
	}
}

func TestNoToken(t *testing.T) {
	c, st, _ := setup(t)
	_ = st.DeleteToken("default")
	c.Forget()
	if err := c.Pause(context.Background(), ""); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("got %v", err)
	}
}

func TestNoActiveDevice(t *testing.T) {
	c, _, _ := setup(t)
	if err := c.Pause(context.Background(), ""); !errors.Is(err, ErrNoActiveDevice) {
		t.Fatalf("got %v, want ErrNoActiveDevice", err)
	}
}

func TestPlayBodies(t *testing.T) {
	c, _, f := setup(t)
	ctx := context.Background()
	cases := []struct{ uri, want string }{
		{"spotify:track:1", `{"uris":["spotify:track:1"]}`},
		{"spotify:album:2", `{"context_uri":"spotify:album:2"}`},
		{"", ``},
	}
	for _, tc := range cases {
		if err := c.Play(ctx, "d1", tc.uri); err != nil {
			t.Fatal(err)
		}
		if got := f.lastBody.Load().(string); got != tc.want {
			t.Errorf("play %q body %s, want %s", tc.uri, got, tc.want)
		}
		if got := f.lastPath.Load().(string); got != "PUT /v1/me/player/play?device_id=d1" {
			t.Errorf("path %s", got)
		}
	}
	if err := c.Play(ctx, "", "https://open.spotify.com/track/1"); err == nil {
		t.Error("URL accepted as URI")
	}
}

func TestTransferAndSearch(t *testing.T) {
	c, _, f := setup(t)
	ctx := context.Background()
	if err := c.Transfer(ctx, "d1", true); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(f.lastBody.Load().(string)), &body)
	if body["play"] != true || body["device_ids"].([]any)[0] != "d1" {
		t.Fatalf("transfer body %v", body)
	}
	items, err := c.Search(ctx, "song", "track", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Artists != "A, B" || items[0].Album != "Al" {
		t.Fatalf("items %+v", items)
	}
	if _, err := c.Search(ctx, "x", "podcast", 1); err == nil || !strings.Contains(err.Error(), "unknown search type") {
		t.Fatalf("bad type: %v", err)
	}
	pb, err := c.NowPlaying(ctx)
	if err != nil || pb != nil {
		t.Fatalf("now playing on 204: %+v %v", pb, err)
	}
}
