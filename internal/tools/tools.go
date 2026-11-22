// Package tools registers the MCP tools that control Spotify playback.
package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/subpathdev/spotify-mcp/internal/spotify"
)

// Player is the part of the Spotify client the tools use.
type Player interface {
	Devices(ctx context.Context) ([]spotify.Device, error)
	Search(ctx context.Context, query, typ string, limit int) ([]spotify.Item, error)
	Play(ctx context.Context, deviceID, uri string) error
	Pause(ctx context.Context, deviceID string) error
	Next(ctx context.Context, deviceID string) error
	Previous(ctx context.Context, deviceID string) error
	Transfer(ctx context.Context, deviceID string, play bool) error
	NowPlaying(ctx context.Context) (*spotify.Playback, error)
}

// DeviceArg is shared by every tool that can target a device.
type DeviceArg struct {
	Device string `json:"device,omitempty" jsonschema:"Device name (e.g. 'Pixel 8') or ID from the devices tool. Empty: the active device."`
}

// PlayArgs are the play tool's arguments.
type PlayArgs struct {
	URI    string `json:"uri,omitempty" jsonschema:"Spotify URI to play, e.g. spotify:track:..., spotify:album:..., spotify:playlist:... Takes precedence over query."`
	Query  string `json:"query,omitempty" jsonschema:"Search text; the first result of the given type is played, e.g. 'Bohemian Rhapsody Queen'."`
	Type   string `json:"type,omitempty" jsonschema:"What the query searches for: track (default), album, artist, playlist, show or episode."`
	Device string `json:"device,omitempty" jsonschema:"Device name or ID. Empty: the active device."`
}

// SearchArgs are the search tool's arguments.
type SearchArgs struct {
	Query string `json:"query" jsonschema:"Search text, e.g. an artist, song or album name."`
	Type  string `json:"type,omitempty" jsonschema:"track (default), album, artist, playlist, show or episode."`
	Limit int    `json:"limit,omitempty" jsonschema:"Number of results, 1-10 (Spotify development mode allows at most 10). Default 5."`
}

// TransferArgs are the transfer tool's arguments.
type TransferArgs struct {
	Device string `json:"device" jsonschema:"Target device name (e.g. 'Pixel 8') or ID from the devices tool."`
	Play   *bool  `json:"play,omitempty" jsonschema:"Start playing on the new device. Default true."`
}

type empty struct{}

func text(format string, a ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, a...)}}}
}

func boolPtr(b bool) *bool { return &b }

// Register adds the tools to s.
func Register(s *mcp.Server, p Player) {
	t := &toolset{p: p}

	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(true)}
	ctl := &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(true)}
	step := &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), OpenWorldHint: boolPtr(true)}

	mcp.AddTool(s, &mcp.Tool{Name: "devices", Description: "List the Spotify Connect devices (phone, computer, speakers) that Spotify currently sees, and which one is active.", Annotations: ro}, t.devices)
	mcp.AddTool(s, &mcp.Tool{Name: "now_playing", Description: "Show what is playing on Spotify right now, on which device, and whether it is paused.", Annotations: ro}, t.nowPlaying)
	mcp.AddTool(s, &mcp.Tool{Name: "search", Description: "Search Spotify. Returns up to 10 results with their URIs, which the play tool accepts.", Annotations: ro}, t.search)
	mcp.AddTool(s, &mcp.Tool{Name: "play", Description: "Play a Spotify URI, or the first search result for a query, on a device (default: the active one). Without uri and query it resumes playback.", Annotations: step}, t.play)
	mcp.AddTool(s, &mcp.Tool{Name: "pause", Description: "Pause Spotify playback.", Annotations: ctl}, t.pause)
	mcp.AddTool(s, &mcp.Tool{Name: "resume", Description: "Resume Spotify playback where it was paused.", Annotations: ctl}, t.resume)
	mcp.AddTool(s, &mcp.Tool{Name: "next", Description: "Skip to the next track.", Annotations: step}, t.next)
	mcp.AddTool(s, &mcp.Tool{Name: "previous", Description: "Go back to the previous track.", Annotations: step}, t.previous)
	mcp.AddTool(s, &mcp.Tool{Name: "transfer", Description: "Move playback to another device, e.g. from the computer to the phone.", Annotations: ctl}, t.transfer)
}

type toolset struct{ p Player }

// explain turns the common Spotify errors into something a person can act on.
func (t *toolset) explain(ctx context.Context, err error) error {
	if !errors.Is(err, spotify.ErrNoActiveDevice) {
		return err
	}
	msg := "No active Spotify device. Open the Spotify app on your phone, computer or speaker (playing anything once is enough), then try again"
	if devs, derr := t.p.Devices(ctx); derr == nil && len(devs) > 0 {
		names := make([]string, 0, len(devs))
		for _, d := range devs {
			names = append(names, fmt.Sprintf("%q", d.Name))
		}
		msg += ", or pass one of these as device: " + strings.Join(names, ", ")
	}
	return errors.New(msg + ".")
}

// resolveDevice turns a name or ID into an ID. An empty name means the
// active device.
func (t *toolset) resolveDevice(ctx context.Context, name string) (string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", nil
	}
	devs, err := t.p.Devices(ctx)
	if err != nil {
		return "", "", err
	}
	for _, d := range devs {
		if d.ID == name {
			return d.ID, d.Name, nil
		}
	}
	var matches []spotify.Device
	for _, d := range devs {
		if strings.EqualFold(d.Name, name) {
			return d.ID, d.Name, nil
		}
		if strings.Contains(strings.ToLower(d.Name), strings.ToLower(name)) ||
			strings.EqualFold(d.Type, name) {
			matches = append(matches, d)
		}
	}
	if len(matches) == 1 {
		return matches[0].ID, matches[0].Name, nil
	}
	names := make([]string, 0, len(devs))
	for _, d := range devs {
		names = append(names, fmt.Sprintf("%q (%s)", d.Name, d.Type))
	}
	if len(devs) == 0 {
		return "", "", fmt.Errorf("no Spotify devices are online. Open the Spotify app on the device %q first", name)
	}
	if len(matches) > 1 {
		return "", "", fmt.Errorf("%q matches more than one device: %s", name, strings.Join(names, ", "))
	}
	return "", "", fmt.Errorf("no device called %q. Online devices: %s. If yours is missing, open the Spotify app on it", name, strings.Join(names, ", "))
}

func formatDevice(d spotify.Device) string {
	s := fmt.Sprintf("%s (%s, id %s)", d.Name, d.Type, d.ID)
	if d.IsActive {
		s += " [active]"
	}
	if d.IsRestricted {
		s += " [can't be controlled]"
	}
	return s
}

func formatItem(it spotify.Item) string {
	s := it.Name
	if it.Artists != "" {
		s += " by " + it.Artists
	}
	if it.Album != "" && it.Type != "album" {
		s += " (" + it.Album + ")"
	}
	if it.Owner != "" {
		s += " (by " + it.Owner + ")"
	}
	return s + " - " + it.URI
}

func (t *toolset) devices(ctx context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, any, error) {
	devs, err := t.p.Devices(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(devs) == 0 {
		return text("No Spotify devices are online. Open the Spotify app on a phone, computer or speaker; it then shows up here."), nil, nil
	}
	lines := make([]string, 0, len(devs))
	for _, d := range devs {
		lines = append(lines, "- "+formatDevice(d))
	}
	return text("Devices:\n%s", strings.Join(lines, "\n")), nil, nil
}

func mmss(ms int) string { return fmt.Sprintf("%d:%02d", ms/60000, ms/1000%60) }

func (t *toolset) nowPlaying(ctx context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, any, error) {
	pb, err := t.p.NowPlaying(ctx)
	if err != nil {
		return nil, nil, err
	}
	if pb == nil || pb.Item == nil {
		return text("Nothing is playing on Spotify right now."), nil, nil
	}
	state := "Paused"
	if pb.IsPlaying {
		state = "Playing"
	}
	dev := "unknown device"
	if pb.Device != nil {
		dev = pb.Device.Name
	}
	return text("%s on %s: %s [%s / %s]", state, dev, formatItem(*pb.Item), mmss(pb.ProgressMS), mmss(pb.DurationMS)), nil, nil
}

func (t *toolset) search(ctx context.Context, _ *mcp.CallToolRequest, in SearchArgs) (*mcp.CallToolResult, any, error) {
	items, err := t.p.Search(ctx, in.Query, in.Type, in.Limit)
	if err != nil {
		return nil, nil, err
	}
	if len(items) == 0 {
		return text("No results for %q.", in.Query), nil, nil
	}
	lines := make([]string, 0, len(items))
	for i, it := range items {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, formatItem(it)))
	}
	return text("Results for %q:\n%s", in.Query, strings.Join(lines, "\n")), nil, nil
}

func (t *toolset) play(ctx context.Context, _ *mcp.CallToolRequest, in PlayArgs) (*mcp.CallToolResult, any, error) {
	devID, devName, err := t.resolveDevice(ctx, in.Device)
	if err != nil {
		return nil, nil, err
	}
	uri, what := strings.TrimSpace(in.URI), ""
	if uri == "" && strings.TrimSpace(in.Query) != "" {
		items, err := t.p.Search(ctx, in.Query, in.Type, 1)
		if err != nil {
			return nil, nil, err
		}
		if len(items) == 0 {
			return nil, nil, fmt.Errorf("nothing found for %q", in.Query)
		}
		uri, what = items[0].URI, formatItem(items[0])
	}
	if uri != "" {
		if _, err := spotify.URIType(uri); err != nil {
			return nil, nil, err
		}
	}
	if err := t.p.Play(ctx, devID, uri); err != nil {
		return nil, nil, t.explain(ctx, err)
	}
	where := ""
	if devName != "" {
		where = " on " + devName
	}
	switch {
	case what != "":
		return text("Playing %s%s.", what, where), nil, nil
	case uri != "":
		return text("Playing %s%s.", uri, where), nil, nil
	default:
		return text("Resumed playback%s.", where), nil, nil
	}
}

func (t *toolset) simple(ctx context.Context, device string, f func(context.Context, string) error, done string) (*mcp.CallToolResult, any, error) {
	devID, _, err := t.resolveDevice(ctx, device)
	if err != nil {
		return nil, nil, err
	}
	if err := f(ctx, devID); err != nil {
		return nil, nil, t.explain(ctx, err)
	}
	return text("%s", done), nil, nil
}

func (t *toolset) pause(ctx context.Context, _ *mcp.CallToolRequest, in DeviceArg) (*mcp.CallToolResult, any, error) {
	return t.simple(ctx, in.Device, t.p.Pause, "Paused.")
}

func (t *toolset) resume(ctx context.Context, _ *mcp.CallToolRequest, in DeviceArg) (*mcp.CallToolResult, any, error) {
	return t.simple(ctx, in.Device, func(ctx context.Context, id string) error { return t.p.Play(ctx, id, "") }, "Resumed.")
}

func (t *toolset) next(ctx context.Context, _ *mcp.CallToolRequest, in DeviceArg) (*mcp.CallToolResult, any, error) {
	return t.simple(ctx, in.Device, t.p.Next, "Skipped to the next track.")
}

func (t *toolset) previous(ctx context.Context, _ *mcp.CallToolRequest, in DeviceArg) (*mcp.CallToolResult, any, error) {
	return t.simple(ctx, in.Device, t.p.Previous, "Went back to the previous track.")
}

func (t *toolset) transfer(ctx context.Context, _ *mcp.CallToolRequest, in TransferArgs) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Device) == "" {
		return nil, nil, errors.New("device is required; call the devices tool to see the names")
	}
	devID, devName, err := t.resolveDevice(ctx, in.Device)
	if err != nil {
		return nil, nil, err
	}
	play := in.Play == nil || *in.Play
	if err := t.p.Transfer(ctx, devID, play); err != nil {
		return nil, nil, t.explain(ctx, err)
	}
	return text("Moved playback to %s.", devName), nil, nil
}
