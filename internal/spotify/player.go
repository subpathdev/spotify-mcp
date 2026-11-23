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

type player struct {
	api    API
	tokens TokenSource
}

// NewPlayer returns playback.Spotify for one connected account, which
// authenticates with tokens.
func NewPlayer(api API, tokens TokenSource) playback.Spotify {
	return &player{api: api, tokens: tokens}
}

// do sends req with the account's token. After a 401 it invalidates the token
// and tries once more with a fresh one.
func (p *player) do(ctx context.Context, req request) (int, error) {
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
	id, err := playback.ParseDeviceID(d.ID)
	if err != nil {
		id = nil // Spotify sends no ID for some devices
	}
	return playback.NewDevice(id, d.Name, d.Type, d.IsActive, d.IsRestricted)
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
		return nil, false
	}
	opts := []playback.ItemOption{playback.WithDuration(time.Duration(w.DurationMS) * time.Millisecond)}
	if len(w.Artists) > 0 {
		names := make([]string, len(w.Artists))
		for i, a := range w.Artists {
			names[i] = a.Name
		}
		opts = append(opts, playback.WithArtists(names...))
	}
	switch {
	case w.Album != nil:
		opts = append(opts, playback.WithAlbum(w.Album.Name))
	case w.Show != nil:
		opts = append(opts, playback.WithAlbum(w.Show.Name))
	}
	if w.Owner != nil {
		opts = append(opts, playback.WithOwner(w.Owner.DisplayName))
	}
	it, err := playback.NewItem(uri, w.Name, opts...)
	return it, err == nil
}

// Devices implements playback.Spotify.
func (p *player) Devices(ctx context.Context) ([]playback.Device, error) {
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
func (p *player) Search(ctx context.Context, query string, kind playback.Kind, limit int) ([]playback.Item, error) {
	limit = min(max(limit, 1), playback.MaxSearchResults)
	if kind == nil {
		return nil, errors.New("search needs a type")
	}
	q := url.Values{"q": {query}, "type": {kind.String()}, "limit": {strconv.Itoa(limit)}}
	var out map[string]struct {
		Items []*wireItem `json:"items"`
	}
	if _, err := p.do(ctx, request{method: http.MethodGet, path: "/search", query: q, out: &out}); err != nil {
		return nil, err
	}
	var items []playback.Item
	for _, w := range out[kind.String()+"s"].Items {
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
	if id == nil {
		return nil
	}
	return url.Values{"device_id": {id.String()}}
}

// Play implements playback.Spotify. A track or episode plays on its own; an
// album, artist, playlist or show plays as a context.
func (p *player) Play(ctx context.Context, device playback.DeviceID, uri playback.URI) error {
	if uri == nil {
		return errors.New("no URI to play")
	}
	var body any = map[string]any{"uris": []string{uri.String()}}
	if uri.Kind().IsContext() {
		body = map[string]any{"context_uri": uri.String()}
	}
	_, err := p.do(ctx, request{method: http.MethodPut, path: "/me/player/play", query: deviceQuery(device), body: body})
	return err
}

func (p *player) command(ctx context.Context, method, path string, device playback.DeviceID) error {
	_, err := p.do(ctx, request{method: method, path: path, query: deviceQuery(device)})
	return err
}

// Resume implements playback.Spotify.
func (p *player) Resume(ctx context.Context, device playback.DeviceID) error {
	return p.command(ctx, http.MethodPut, "/me/player/play", device)
}

// Pause implements playback.Spotify.
func (p *player) Pause(ctx context.Context, device playback.DeviceID) error {
	return p.command(ctx, http.MethodPut, "/me/player/pause", device)
}

// Next implements playback.Spotify.
func (p *player) Next(ctx context.Context, device playback.DeviceID) error {
	return p.command(ctx, http.MethodPost, "/me/player/next", device)
}

// Previous implements playback.Spotify.
func (p *player) Previous(ctx context.Context, device playback.DeviceID) error {
	return p.command(ctx, http.MethodPost, "/me/player/previous", device)
}

// Transfer implements playback.Spotify.
func (p *player) Transfer(ctx context.Context, device playback.DeviceID, play bool) error {
	if device == nil {
		return errors.New("transfer needs a device")
	}
	body := map[string]any{"device_ids": []string{device.String()}, "play": play}
	_, err := p.do(ctx, request{method: http.MethodPut, path: "/me/player", body: body})
	return err
}

// State implements playback.Spotify.
func (p *player) State(ctx context.Context) (playback.State, error) {
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
	var dev playback.Device
	if out.Device != nil {
		dev = out.Device.device()
	}
	var it playback.Item
	if out.Item != nil {
		if i, ok := out.Item.item(); ok {
			it = i
		}
	}
	return playback.NewState(out.IsPlaying, time.Duration(out.ProgressMS)*time.Millisecond, dev, it), nil
}
