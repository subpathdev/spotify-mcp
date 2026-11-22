// Package spotify adapts the Spotify Web API and Spotify's OAuth server to the
// ports of the account and playback packages. Spotify's JSON formats stay in
// this package.
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
	"time"

	"github.com/subpathdev/spotify-mcp/internal/playback"
)

// DefaultAPIURL is the Web API's base URL.
const DefaultAPIURL = "https://api.spotify.com/v1"

const maxBody = 4 << 20

// API sends requests to the Web API with a given access token.
type API struct {
	base string
	http *http.Client
}

// NewAPI returns an API for baseURL (DefaultAPIURL in production). httpClient
// may be nil.
func NewAPI(baseURL string, httpClient *http.Client) *API {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &API{base: strings.TrimRight(baseURL, "/"), http: httpClient}
}

// Error is an error answer from the Web API that has no domain meaning.
type Error struct {
	Status  int
	Message string
	Reason  string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("spotify API error %d", e.Status)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.Reason != "" {
		msg += " (" + e.Reason + ")"
	}
	return msg
}

// errUnauthorized marks a 401, which the Player answers by invalidating the
// access token.
var errUnauthorized = errors.New("spotify rejected the access token")

// request describes one Web API call.
type request struct {
	method string
	path   string
	query  url.Values
	body   any // JSON-encoded if not nil
	out    any // JSON-decoded from the answer if not nil
}

// do sends req and returns the HTTP status. A 204 leaves req.out untouched.
func (a *API) do(ctx context.Context, token string, req request) (int, error) {
	u := a.base + req.path
	if len(req.query) > 0 {
		u += "?" + req.query.Encode()
	}
	var body io.Reader = http.NoBody
	if req.body != nil {
		b, err := json.Marshal(req.body)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	hr, err := http.NewRequestWithContext(ctx, req.method, u, body)
	if err != nil {
		return 0, err
	}
	hr.Header.Set("Authorization", "Bearer "+token)
	if req.body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(hr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, apiError(resp, data)
	}
	if req.out != nil && resp.StatusCode != http.StatusNoContent && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, req.out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode Spotify answer: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func apiError(resp *http.Response, data []byte) error {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Reason  string `json:"reason"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &e) // not every error answer is JSON
	switch {
	case e.Error.Reason == "NO_ACTIVE_DEVICE":
		return playback.ErrNoActiveDevice
	case e.Error.Reason == "PREMIUM_REQUIRED":
		return playback.ErrPremiumRequired
	case resp.StatusCode == http.StatusUnauthorized:
		return errUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests:
		msg := "rate limited by Spotify, try again shortly"
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			msg = fmt.Sprintf("rate limited by Spotify, try again in %d s", s)
		}
		return &Error{Status: resp.StatusCode, Message: msg}
	}
	msg := e.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(data))
		if len(msg) > 200 {
			msg = msg[:200]
		}
	}
	return &Error{Status: resp.StatusCode, Message: msg, Reason: e.Error.Reason}
}
