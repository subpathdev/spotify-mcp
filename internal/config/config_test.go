package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/subpathdev/spotify-mcp/internal/account"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(envOf(map[string]string{"SPOTIFY_REDIRECT_URI": "https://spotify.example.com/callback"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ConnectURL() != "https://spotify.example.com/connect" || c.ProfileID().String() != "default" || c.DataDir() != "/data" || c.MCPAddr() != ":8000" {
		t.Fatalf("%v", c)
	}
	if err := c.RequireCredentials(); err == nil || !strings.Contains(err.Error(), "SPOTIFY_CLIENT_SECRET") {
		t.Fatalf("credentials: %v", err)
	}
	if got := c.InviteURL(mustCode(t, "a-b_c"+strings.Repeat("x", 38))); got != "https://spotify.example.com/connect?invite=a-b_c"+strings.Repeat("x", 38) {
		t.Fatalf("invite URL %q", got)
	}
}

func TestSecretStaysOutOfPrint(t *testing.T) {
	c, err := Load(envOf(map[string]string{
		"SPOTIFY_REDIRECT_URI": "https://spotify.example.com/callback", "SPOTIFY_CLIENT_SECRET": "s3cret-value",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ClientSecret() != "s3cret-value" {
		t.Fatal("secret not loaded")
	}
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c)} {
		if strings.Contains(s, "s3cret") {
			t.Fatalf("secret printed: %s", s)
		}
	}
}

func TestLoadRejects(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"no redirect":       {},
		"redirect path":     {"SPOTIFY_REDIRECT_URI": "https://x.example/login"},
		"redirect scheme":   {"SPOTIFY_REDIRECT_URI": "ftp://x.example/callback"},
		"bad profile":       {"SPOTIFY_REDIRECT_URI": "https://x.example/callback", "PROFILE": "Jan"},
		"bad public URL":    {"SPOTIFY_REDIRECT_URI": "https://x.example/callback", "PUBLIC_URL": "x.example"},
		"relative redirect": {"SPOTIFY_REDIRECT_URI": "/callback"},
	} {
		if _, err := Load(envOf(env)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func mustCode(t *testing.T, s string) account.InviteCode {
	t.Helper()
	code, err := account.ParseInviteCode(s)
	if err != nil {
		t.Fatal(err)
	}
	return code
}
