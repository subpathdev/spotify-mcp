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
// local .env) through the environment, never from files in the repo. Get one
// from Load.
type Config interface {
	ClientID() string
	// ClientSecret is the Spotify app's secret. Keep it out of logs.
	ClientSecret() string
	// RedirectURL is the OAuth callback registered in the Spotify app; its
	// path is /callback.
	RedirectURL() string
	// ConnectURL is the connect page, the base of invite links.
	ConnectURL() string
	DataDir() string
	ProfileID() account.ProfileID
	MCPAddr() string
	WebAddr() string
	// RequireCredentials reports missing Spotify app credentials.
	RequireCredentials() error
	// InviteURL is the link that opens the connect page with code.
	InviteURL(code account.InviteCode) string
}

type config struct {
	clientID     string
	clientSecret string
	redirectURL  *url.URL
	publicURL    *url.URL
	dataDir      string
	profileID    account.ProfileID
	mcpAddr      string
	webAddr      string
}

func (c *config) ClientID() string             { return c.clientID }
func (c *config) ClientSecret() string         { return c.clientSecret }
func (c *config) RedirectURL() string          { return c.redirectURL.String() }
func (c *config) ConnectURL() string           { return c.publicURL.JoinPath("connect").String() }
func (c *config) DataDir() string              { return c.dataDir }
func (c *config) ProfileID() account.ProfileID { return c.profileID }
func (c *config) MCPAddr() string              { return c.mcpAddr }
func (c *config) WebAddr() string              { return c.webAddr }

// String keeps the client secret out of logs and error messages.
func (c *config) String() string {
	return fmt.Sprintf("config{redirect %s, data %s, profile %s, mcp %s, web %s}",
		c.redirectURL, c.dataDir, c.profileID, c.mcpAddr, c.webAddr)
}

// Load reads the settings through getenv (os.Getenv in production). It
// doesn't require the client secret, which the invite command doesn't need;
// call RequireCredentials before starting the server.
func Load(getenv func(string) string) (Config, error) {
	c, err := load(getenv)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func load(getenv func(string) string) (*config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	c := &config{
		clientID:     get("SPOTIFY_CLIENT_ID", ""),
		clientSecret: get("SPOTIFY_CLIENT_SECRET", ""),
		dataDir:      get("DATA_DIR", "/data"),
		mcpAddr:      get("MCP_ADDR", ":8000"),
		webAddr:      get("WEB_ADDR", ":8080"),
	}

	raw := get("SPOTIFY_REDIRECT_URI", "")
	if raw == "" {
		return nil, errors.New("SPOTIFY_REDIRECT_URI is not set (see .env.example)")
	}
	redirect, err := parseHTTPURL(raw)
	if err != nil {
		return nil, fmt.Errorf("SPOTIFY_REDIRECT_URI: %w", err)
	}
	if redirect.Path != "/callback" {
		return nil, errors.New("SPOTIFY_REDIRECT_URI must end in /callback, e.g. https://spotify.example.com/callback")
	}
	c.redirectURL = redirect

	c.publicURL = &url.URL{Scheme: redirect.Scheme, Host: redirect.Host}
	if raw := get("PUBLIC_URL", ""); raw != "" {
		if c.publicURL, err = parseHTTPURL(strings.TrimRight(raw, "/")); err != nil {
			return nil, fmt.Errorf("PUBLIC_URL: %w", err)
		}
	}

	if c.profileID, err = account.ParseProfileID(get("PROFILE", "default")); err != nil {
		return nil, fmt.Errorf("PROFILE: %w", err)
	}
	return c, nil
}

func (c *config) RequireCredentials() error {
	var missing []string
	if c.clientID == "" {
		missing = append(missing, "SPOTIFY_CLIENT_ID")
	}
	if c.clientSecret == "" {
		missing = append(missing, "SPOTIFY_CLIENT_SECRET")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing environment variables: %s (see .env.example)", strings.Join(missing, ", "))
	}
	return nil
}

func (c *config) InviteURL(code account.InviteCode) string {
	u := c.publicURL.JoinPath("connect")
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
