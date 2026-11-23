// Package config reads the server's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/subpathdev/spotify-mcp/internal/account"
)

// Config holds every setting. Secrets come from a Kubernetes Secret (or a
// local .env) through the environment, never from files in the repo.
type Config struct {
	ClientID     string
	ClientSecret string
	// RedirectURL is the OAuth callback registered in the Spotify app; its
	// path is /callback.
	RedirectURL *url.URL
	// PublicURL is the base of invite links.
	PublicURL *url.URL
	DataDir   string
	ProfileID account.ProfileID
	MCPAddr   string
	WebAddr   string
}

// Load reads the settings through getenv (os.Getenv in production). It
// doesn't require the client secret, which the invite command doesn't need;
// call RequireCredentials before starting the server.
func Load(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	c := Config{
		ClientID:     get("SPOTIFY_CLIENT_ID", ""),
		ClientSecret: get("SPOTIFY_CLIENT_SECRET", ""),
		DataDir:      get("DATA_DIR", "/data"),
		MCPAddr:      get("MCP_ADDR", ":8000"),
		WebAddr:      get("WEB_ADDR", ":8080"),
	}

	raw := get("SPOTIFY_REDIRECT_URI", "")
	if raw == "" {
		return Config{}, errors.New("SPOTIFY_REDIRECT_URI is not set (see .env.example)")
	}
	redirect, err := parseHTTPURL(raw)
	if err != nil {
		return Config{}, fmt.Errorf("SPOTIFY_REDIRECT_URI: %w", err)
	}
	if redirect.Path != "/callback" {
		return Config{}, errors.New("SPOTIFY_REDIRECT_URI must end in /callback, e.g. https://spotify.example.com/callback")
	}
	c.RedirectURL = redirect

	c.PublicURL = &url.URL{Scheme: redirect.Scheme, Host: redirect.Host}
	if raw := get("PUBLIC_URL", ""); raw != "" {
		if c.PublicURL, err = parseHTTPURL(strings.TrimRight(raw, "/")); err != nil {
			return Config{}, fmt.Errorf("PUBLIC_URL: %w", err)
		}
	}

	if c.ProfileID, err = account.ParseProfileID(get("PROFILE", "default")); err != nil {
		return Config{}, fmt.Errorf("PROFILE: %w", err)
	}
	return c, nil
}

// RequireCredentials reports missing Spotify app credentials.
func (c Config) RequireCredentials() error {
	var missing []string
	if c.ClientID == "" {
		missing = append(missing, "SPOTIFY_CLIENT_ID")
	}
	if c.ClientSecret == "" {
		missing = append(missing, "SPOTIFY_CLIENT_SECRET")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing environment variables: %s (see .env.example)", strings.Join(missing, ", "))
	}
	return nil
}

// InviteURL is the link that opens the connect page with code.
func (c Config) InviteURL(code account.InviteCode) string {
	u := c.PublicURL.JoinPath("connect")
	u.RawQuery = url.Values{"invite": {code.Secret()}}.Encode()
	return u.String()
}

func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("%q is not an http(s) URL", raw)
	}
	return u, nil
}
