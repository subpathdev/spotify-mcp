// Package spotify is a small client for the parts of the Spotify Web API this
// server needs: playback control, devices and search. It keeps the OAuth token
// of one profile in the store and writes it back whenever Spotify refreshes it.
package spotify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/subpathdev/spotify-mcp/internal/store"
)

// Scopes the server asks for at login: read and control playback, nothing else.
// No library, playlist or e-mail access.
var Scopes = []string{
	"user-read-playback-state",
	"user-modify-playback-state",
	"user-read-currently-playing",
}

// Endpoint is Spotify's OAuth endpoint. The client secret goes in the
// Authorization header, as Spotify documents.
var Endpoint = oauth2.Endpoint{
	AuthURL:   "https://accounts.spotify.com/authorize",
	TokenURL:  "https://accounts.spotify.com/api/token",
	AuthStyle: oauth2.AuthStyleInHeader,
}

// MaxSearchLimit is the most results Spotify returns per search for apps in
// development mode (February 2026 rules).
const MaxSearchLimit = 10

var (
	// ErrNotConnected means nobody has connected a Spotify account to the profile
	// yet, or Spotify revoked the access.
	ErrNotConnected = errors.New("no Spotify account is connected yet: create an invite link (spotify-mcp invite) and open it to connect one")
	// ErrNoActiveDevice is Spotify's NO_ACTIVE_DEVICE.
	ErrNoActiveDevice = errors.New("no active Spotify device")
	// ErrPremiumRequired is Spotify's PREMIUM_REQUIRED.
	ErrPremiumRequired = errors.New("playback control needs Spotify Premium (both for the app owner and the connected account)")
)

// APIError is any other error answer from the Web API.
type APIError struct {
	Status  int
	Message string
	Reason  string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("Spotify API error %d", e.Status)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Reason != "" {
		msg += " (" + e.Reason + ")"
	}
	return msg
}

// Client talks to the Web API for one profile.
type Client struct {
	oauth   *oauth2.Config
	store   *store.Store
	profile string
	http    *http.Client // used for API and token calls
	apiBase string

	mu  sync.Mutex
	tok *oauth2.Token
}

// NewClient returns a client for profile. httpClient may be nil.
func NewClient(cfg *oauth2.Config, st *store.Store, profile string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{oauth: cfg, store: st, profile: profile, http: httpClient, apiBase: "https://api.spotify.com/v1"}
}

// SetAPIBase points the client at another server (tests).
func (c *Client) SetAPIBase(u string) { c.apiBase = strings.TrimRight(u, "/") }

// Forget drops the cached token, e.g. after a new login or a disconnect.
func (c *Client) Forget() {
	c.mu.Lock()
	c.tok = nil
	c.mu.Unlock()
}

// token returns a valid access token, refreshing and persisting it if needed.
func (c *Client) token(ctx context.Context) (*oauth2.Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok == nil {
		st, err := c.store.Token(c.profile)
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotConnected
		}
		if err != nil {
			return nil, fmt.Errorf("load token: %w", err)
		}
		c.tok = &oauth2.Token{RefreshToken: st.RefreshToken, AccessToken: st.AccessToken, Expiry: st.Expiry, TokenType: "Bearer"}
	}
	if c.tok.Valid() {
		return c.tok, nil
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, c.http)
	nt, err := c.oauth.TokenSource(ctx, c.tok).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
			// Revoked at Spotify or the app's secret changed: a new login is needed.
			c.tok = nil
			return nil, ErrNotConnected
		}
		return nil, fmt.Errorf("refresh Spotify token: %w", err)
	}
	// Spotify may or may not send a new refresh token; either way the current
	// one goes to disk before it's used, so a pod restart keeps working.
	st, err := c.store.Token(c.profile)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	st.RefreshToken = nt.RefreshToken
	st.AccessToken = nt.AccessToken
	st.Expiry = nt.Expiry
	if err := c.store.SaveToken(c.profile, st); err != nil {
		return nil, fmt.Errorf("save refreshed token: %w", err)
	}
	c.tok = nt
	return nt, nil
}

// do sends one API request. body is JSON-encoded if not nil; out is decoded
// from a JSON answer if not nil. A 204 leaves out untouched.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) (int, error) {
	tok, err := c.token(ctx)
	if err != nil {
		return 0, err
	}
	u := c.apiBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	} else if method == http.MethodPut || method == http.MethodPost {
		req.ContentLength = 0
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, c.apiError(resp, data)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode Spotify answer: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func (c *Client) apiError(resp *http.Response, data []byte) error {
	var e struct {
		Error struct {
			Status  int    `json:"status"`
			Message string `json:"message"`
			Reason  string `json:"reason"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &e)
	switch {
	case e.Error.Reason == "NO_ACTIVE_DEVICE":
		return ErrNoActiveDevice
	case e.Error.Reason == "PREMIUM_REQUIRED":
		return ErrPremiumRequired
	case resp.StatusCode == http.StatusUnauthorized:
		// The access token was rejected although it looked valid: drop it so the
		// next call refreshes.
		c.mu.Lock()
		if c.tok != nil {
			c.tok.Expiry = time.Unix(1, 0)
		}
		c.mu.Unlock()
	case resp.StatusCode == http.StatusTooManyRequests:
		wait := resp.Header.Get("Retry-After")
		if s, err := strconv.Atoi(wait); err == nil {
			return &APIError{Status: 429, Message: fmt.Sprintf("rate limited by Spotify, try again in %d s", s)}
		}
		return &APIError{Status: 429, Message: "rate limited by Spotify, try again shortly"}
	}
	msg := e.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(data))
		if len(msg) > 200 {
			msg = msg[:200]
		}
	}
	return &APIError{Status: resp.StatusCode, Message: msg, Reason: e.Error.Reason}
}

// User is the connected account.
type User struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// Me returns the connected account.
func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	_, err := c.do(ctx, http.MethodGet, "/me", nil, nil, &u)
	return u, err
}

// Device is a Spotify Connect device.
type Device struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	IsActive      bool   `json:"is_active"`
	IsRestricted  bool   `json:"is_restricted"`
	VolumePercent *int   `json:"volume_percent,omitempty"`
}

// Devices lists the account's devices that Spotify currently sees.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var out struct {
		Devices []Device `json:"devices"`
	}
	_, err := c.do(ctx, http.MethodGet, "/me/player/devices", nil, nil, &out)
	return out.Devices, err
}

// Item is one search result or the playing item.
type Item struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	URI     string `json:"uri"`
	Artists string `json:"artists,omitempty"`
	Album   string `json:"album,omitempty"`
	Owner   string `json:"owner,omitempty"`
}

type apiArtist struct {
	Name string `json:"name"`
}

type apiItem struct {
	Type    string      `json:"type"`
	Name    string      `json:"name"`
	URI     string      `json:"uri"`
	Artists []apiArtist `json:"artists"`
	Album   *struct {
		Name string `json:"name"`
	} `json:"album"`
	Owner *struct {
		DisplayName string `json:"display_name"`
	} `json:"owner"`
	Show *struct {
		Name string `json:"name"`
	} `json:"show"`
	DurationMS int `json:"duration_ms"`
}

func (a apiItem) item() Item {
	it := Item{Type: a.Type, Name: a.Name, URI: a.URI}
	names := make([]string, 0, len(a.Artists))
	for _, ar := range a.Artists {
		names = append(names, ar.Name)
	}
	it.Artists = strings.Join(names, ", ")
	if a.Album != nil {
		it.Album = a.Album.Name
	}
	if a.Owner != nil {
		it.Owner = a.Owner.DisplayName
	}
	if a.Show != nil {
		it.Album = a.Show.Name
	}
	return it
}

// SearchTypes are the types Search accepts.
var SearchTypes = []string{"track", "album", "artist", "playlist", "show", "episode"}

// Search returns up to limit results (1-10) of one type.
func (c *Client) Search(ctx context.Context, query, typ string, limit int) ([]Item, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("search query is empty")
	}
	if typ == "" {
		typ = "track"
	}
	valid := false
	for _, t := range SearchTypes {
		if t == typ {
			valid = true
		}
	}
	if !valid {
		return nil, fmt.Errorf("unknown search type %q (use one of %s)", typ, strings.Join(SearchTypes, ", "))
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}
	q := url.Values{"q": {query}, "type": {typ}, "limit": {strconv.Itoa(limit)}}
	var out map[string]struct {
		Items []*apiItem `json:"items"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/search", q, nil, &out); err != nil {
		return nil, err
	}
	var items []Item
	for _, it := range out[typ+"s"].Items {
		if it == nil { // Spotify sometimes returns null entries for playlists
			continue
		}
		items = append(items, it.item())
	}
	return items, nil
}

func deviceQuery(deviceID string) url.Values {
	if deviceID == "" {
		return nil
	}
	return url.Values{"device_id": {deviceID}}
}

// Play starts playing uri on deviceID (empty: the active device). A track or
// episode URI plays on its own; an album, artist, playlist or show URI plays
// as a context. An empty uri resumes.
func (c *Client) Play(ctx context.Context, deviceID, uri string) error {
	var body any
	if uri != "" {
		kind, err := URIType(uri)
		if err != nil {
			return err
		}
		switch kind {
		case "track", "episode":
			body = map[string]any{"uris": []string{uri}}
		default:
			body = map[string]any{"context_uri": uri}
		}
	}
	_, err := c.do(ctx, http.MethodPut, "/me/player/play", deviceQuery(deviceID), body, nil)
	return err
}

// Pause pauses playback.
func (c *Client) Pause(ctx context.Context, deviceID string) error {
	_, err := c.do(ctx, http.MethodPut, "/me/player/pause", deviceQuery(deviceID), nil, nil)
	return err
}

// Next skips to the next item.
func (c *Client) Next(ctx context.Context, deviceID string) error {
	_, err := c.do(ctx, http.MethodPost, "/me/player/next", deviceQuery(deviceID), nil, nil)
	return err
}

// Previous goes back to the previous item.
func (c *Client) Previous(ctx context.Context, deviceID string) error {
	_, err := c.do(ctx, http.MethodPost, "/me/player/previous", deviceQuery(deviceID), nil, nil)
	return err
}

// Transfer moves playback to deviceID; play starts it there even if it was paused.
func (c *Client) Transfer(ctx context.Context, deviceID string, play bool) error {
	body := map[string]any{"device_ids": []string{deviceID}, "play": play}
	_, err := c.do(ctx, http.MethodPut, "/me/player", nil, body, nil)
	return err
}

// Playback is the current playback state.
type Playback struct {
	IsPlaying bool    `json:"is_playing"`
	Device    *Device `json:"device,omitempty"`
	Item      *Item   `json:"item,omitempty"`
	// ProgressMS and DurationMS of the item.
	ProgressMS int `json:"progress_ms"`
	DurationMS int `json:"duration_ms"`
}

// NowPlaying returns the playback state, or nil when nothing is playing anywhere.
func (c *Client) NowPlaying(ctx context.Context) (*Playback, error) {
	var out struct {
		IsPlaying  bool     `json:"is_playing"`
		ProgressMS int      `json:"progress_ms"`
		Device     *Device  `json:"device"`
		Item       *apiItem `json:"item"`
	}
	status, err := c.do(ctx, http.MethodGet, "/me/player", url.Values{"additional_types": {"track,episode"}}, nil, &out)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	p := &Playback{IsPlaying: out.IsPlaying, Device: out.Device, ProgressMS: out.ProgressMS}
	if out.Item != nil {
		it := out.Item.item()
		p.Item = &it
		p.DurationMS = out.Item.DurationMS
	}
	return p, nil
}

// URIType checks a spotify:<type>:<id> URI (or an open.spotify.com link,
// which it doesn't convert) and returns its type.
func URIType(uri string) (string, error) {
	parts := strings.Split(uri, ":")
	if len(parts) != 3 || parts[0] != "spotify" || parts[2] == "" {
		return "", fmt.Errorf("%q is not a Spotify URI like spotify:track:<id>", uri)
	}
	switch parts[1] {
	case "track", "episode", "album", "artist", "playlist", "show":
		return parts[1], nil
	}
	return "", fmt.Errorf("unsupported Spotify URI type %q", parts[1])
}
