package spotify

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/subpathdev/spotify-mcp/internal/playback"
)

// TokenSource provides the access token of the connected account.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
	// Invalidate is called after Spotify rejected accessToken.
	Invalidate(ctx context.Context, accessToken string) error
}

// Player implements playback.Spotify for one connected account.
type Player struct {
	api    *API
	tokens TokenSource
}

var _ playback.Spotify = (*Player)(nil)

// NewPlayer returns a Player that authenticates with tokens.
func NewPlayer(api *API, tokens TokenSource) *Player {
	return &Player{api: api, tokens: tokens}
}

// do sends req with the account's token. After a 401 it invalidates the token
// and tries once more with a fresh one.
func (p *Player) do(ctx context.Context, req request) (int, error) {
	for attempt := 0; ; attempt++ {
		tok, err := p.tokens.AccessToken(ctx)
		if err != nil {
			return 0, err
		}
		status, err := p.api.do(ctx, tok, req)
		if !errors.Is(err, errUnauthorized) || attempt > 0 {
			return status, err
		}
		if err := p.tokens.Invalidate(ctx, tok); err != nil {
			return status, err
		}
	}
}

type wireDevice struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	IsActive     bool   `json:"is_active"`
	IsRestricted bool   `json:"is_restricted"`
}

func (d wireDevice) device() playback.Device {
	return playback.Device{
		ID: playback.DeviceID(d.ID), Name: d.Name, Type: d.Type,
		Active: d.IsActive, Restricted: d.IsRestricted,
	}
}

type wireName struct {
	Name string `json:"name"`
}

type wireItem struct {
	Type    string     `json:"type"`
	Name    string     `json:"name"`
	URI     string     `json:"uri"`
	Artists []wireName `json:"artists"`
	Album   *wireName  `json:"album"`
	Show    *wireName  `json:"show"`
	Owner   *struct {
		DisplayName string `json:"display_name"`
	} `json:"owner"`
	DurationMS int `json:"duration_ms"`
}

// item maps a wire item; ok is false for items this server can't play.
func (w wireItem) item() (playback.Item, bool) {
	uri, err := playback.ParseURI(w.URI)
	if err != nil {
		return playback.Item{}, false
	}
	it := playback.Item{
		Kind: uri.Kind(), Name: w.Name, URI: uri,
		Duration: time.Duration(w.DurationMS) * time.Millisecond,
	}
	for _, a := range w.Artists {
		it.Artists = append(it.Artists, a.Name)
	}
	switch {
	case w.Album != nil:
		it.Album = w.Album.Name
	case w.Show != nil:
		it.Album = w.Show.Name
	}
	if w.Owner != nil {
		it.Owner = w.Owner.DisplayName
	}
	return it, true
}

// Devices implements playback.Spotify.
func (p *Player) Devices(ctx context.Context) ([]playback.Device, error) {
	var out struct {
		Devices []wireDevice `json:"devices"`
	}
	if _, err := p.do(ctx, request{method: http.MethodGet, path: "/me/player/devices", out: &out}); err != nil {
		return nil, err
	}
	devs := make([]playback.Device, len(out.Devices))
	for i, d := range out.Devices {
		devs[i] = d.device()
	}
	return devs, nil
}

// Search implements playback.Spotify.
func (p *Player) Search(ctx context.Context, query string, kind playback.Kind, limit int) ([]playback.Item, error) {
	limit = min(max(limit, 1), playback.MaxSearchResults)
	q := url.Values{"q": {query}, "type": {string(kind)}, "limit": {strconv.Itoa(limit)}}
	var out map[string]struct {
		Items []*wireItem `json:"items"`
	}
	if _, err := p.do(ctx, request{method: http.MethodGet, path: "/search", query: q, out: &out}); err != nil {
		return nil, err
	}
	var items []playback.Item
	for _, w := range out[string(kind)+"s"].Items {
		if w == nil { // Spotify returns null entries for some playlists
			continue
		}
		if it, ok := w.item(); ok {
			items = append(items, it)
		}
	}
	return items, nil
}

func deviceQuery(id playback.DeviceID) url.Values {
	if id == "" {
		return nil
	}
	return url.Values{"device_id": {string(id)}}
}

// Play implements playback.Spotify. A track or episode plays on its own; an
// album, artist, playlist or show plays as a context.
func (p *Player) Play(ctx context.Context, device playback.DeviceID, uri playback.URI) error {
	var body any = map[string]any{"uris": []string{uri.String()}}
	if uri.Kind().IsContext() {
		body = map[string]any{"context_uri": uri.String()}
	}
	_, err := p.do(ctx, request{method: http.MethodPut, path: "/me/player/play", query: deviceQuery(device), body: body})
	return err
}

func (p *Player) command(ctx context.Context, method, path string, device playback.DeviceID) error {
	_, err := p.do(ctx, request{method: method, path: path, query: deviceQuery(device)})
	return err
}

// Resume implements playback.Spotify.
func (p *Player) Resume(ctx context.Context, device playback.DeviceID) error {
	return p.command(ctx, http.MethodPut, "/me/player/play", device)
}

// Pause implements playback.Spotify.
func (p *Player) Pause(ctx context.Context, device playback.DeviceID) error {
	return p.command(ctx, http.MethodPut, "/me/player/pause", device)
}

// Next implements playback.Spotify.
func (p *Player) Next(ctx context.Context, device playback.DeviceID) error {
	return p.command(ctx, http.MethodPost, "/me/player/next", device)
}

// Previous implements playback.Spotify.
func (p *Player) Previous(ctx context.Context, device playback.DeviceID) error {
	return p.command(ctx, http.MethodPost, "/me/player/previous", device)
}

// Transfer implements playback.Spotify.
func (p *Player) Transfer(ctx context.Context, device playback.DeviceID, play bool) error {
	body := map[string]any{"device_ids": []string{string(device)}, "play": play}
	_, err := p.do(ctx, request{method: http.MethodPut, path: "/me/player", body: body})
	return err
}

// State implements playback.Spotify.
func (p *Player) State(ctx context.Context) (*playback.State, error) {
	var out struct {
		IsPlaying  bool        `json:"is_playing"`
		ProgressMS int         `json:"progress_ms"`
		Device     *wireDevice `json:"device"`
		Item       *wireItem   `json:"item"`
	}
	q := url.Values{"additional_types": {"track,episode"}}
	status, err := p.do(ctx, request{method: http.MethodGet, path: "/me/player", query: q, out: &out})
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	st := &playback.State{Playing: out.IsPlaying, Progress: time.Duration(out.ProgressMS) * time.Millisecond}
	if out.Device != nil {
		d := out.Device.device()
		st.Device = &d
	}
	if out.Item != nil {
		if it, ok := out.Item.item(); ok {
			st.Item = &it
		}
	}
	return st, nil
}
