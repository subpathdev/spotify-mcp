// Package mcpserver exposes playback control as MCP tools over Streamable
// HTTP. It only turns tool calls into calls on a Player and results into text.
package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/subpathdev/spotify-mcp/internal/playback"
)

// Player is what the tools need; *playback.Service implements it.
type Player interface {
	Devices(ctx context.Context) ([]playback.Device, error)
	NowPlaying(ctx context.Context) (*playback.State, error)
	Search(ctx context.Context, query string, kind playback.Kind, limit int) ([]playback.Item, error)
	Play(ctx context.Context, selector string, uri playback.URI) (*playback.Device, error)
	PlayFirst(ctx context.Context, selector, query string, kind playback.Kind) (playback.Item, *playback.Device, error)
	Resume(ctx context.Context, selector string) (*playback.Device, error)
	Pause(ctx context.Context, selector string) (*playback.Device, error)
	Next(ctx context.Context, selector string) (*playback.Device, error)
	Previous(ctx context.Context, selector string) (*playback.Device, error)
	Transfer(ctx context.Context, selector string, play bool) (playback.Device, error)
}

const instructions = "Controls Spotify playback on the user's devices (Spotify Connect). " +
	"Use devices to find a device name, search for URIs, and play/pause/resume/next/previous/transfer to control playback."

// NewServer returns an MCP server with the playback tools.
func NewServer(p Player, version string) *mcp.Server {
	s := mcp.NewServer(
		&mcp.Implementation{Name: "spotify-mcp", Title: "Spotify", Version: version},
		&mcp.ServerOptions{Instructions: instructions},
	)
	register(s, p)
	return s
}

// Handler serves s at /mcp and a liveness check at /healthz.
func Handler(s *mcp.Server, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Logger: log, SessionTimeout: 30 * time.Minute},
	))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return mux
}

type deviceArgs struct {
	Device string `json:"device,omitempty" jsonschema:"Device name (e.g. 'Pixel 8'), type (e.g. 'smartphone') or ID from the devices tool. Empty: the active device."`
}

type playArgs struct {
	URI    string `json:"uri,omitempty" jsonschema:"Spotify URI to play, e.g. spotify:track:..., spotify:album:..., spotify:playlist:... Takes precedence over query."`
	Query  string `json:"query,omitempty" jsonschema:"Search text; the first result of the given type is played, e.g. 'Bohemian Rhapsody Queen'."`
	Type   string `json:"type,omitempty" jsonschema:"What the query searches for: track (default), album, artist, playlist, show or episode."`
	Device string `json:"device,omitempty" jsonschema:"Device name, type or ID. Empty: the active device."`
}

type searchArgs struct {
	Query string `json:"query" jsonschema:"Search text, e.g. an artist, song or album name."`
	Type  string `json:"type,omitempty" jsonschema:"track (default), album, artist, playlist, show or episode."`
	Limit int    `json:"limit,omitempty" jsonschema:"Number of results, 1-10 (Spotify development mode allows at most 10). Default 5."`
}

type transferArgs struct {
	Device string `json:"device" jsonschema:"Target device name (e.g. 'Pixel 8'), type or ID from the devices tool."`
	Play   *bool  `json:"play,omitempty" jsonschema:"Start playing on the new device. Default true."`
}

type noArgs struct{}

func register(s *mcp.Server, p Player) {
	t := tools{p: p}
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}
	ctl := &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}
	step := &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}

	mcp.AddTool(s, &mcp.Tool{Name: "devices", Annotations: ro,
		Description: "List the Spotify Connect devices (phone, computer, speakers) that Spotify currently sees, and which one is active."}, t.devices)
	mcp.AddTool(s, &mcp.Tool{Name: "now_playing", Annotations: ro,
		Description: "Show what is playing on Spotify right now, on which device, and whether it is paused."}, t.nowPlaying)
	mcp.AddTool(s, &mcp.Tool{Name: "search", Annotations: ro,
		Description: "Search Spotify. Returns up to 10 results with their URIs, which the play tool accepts."}, t.search)
	mcp.AddTool(s, &mcp.Tool{Name: "play", Annotations: step,
		Description: "Play a Spotify URI, or the first search result for a query, on a device (default: the active one). Without uri and query it resumes playback."}, t.play)
	mcp.AddTool(s, &mcp.Tool{Name: "pause", Annotations: ctl, Description: "Pause Spotify playback."}, t.pause)
	mcp.AddTool(s, &mcp.Tool{Name: "resume", Annotations: ctl, Description: "Resume Spotify playback where it was paused."}, t.resume)
	mcp.AddTool(s, &mcp.Tool{Name: "next", Annotations: step, Description: "Skip to the next track."}, t.next)
	mcp.AddTool(s, &mcp.Tool{Name: "previous", Annotations: step, Description: "Go back to the previous track."}, t.previous)
	mcp.AddTool(s, &mcp.Tool{Name: "transfer", Annotations: ctl,
		Description: "Move playback to another device, e.g. from the computer to the phone."}, t.transfer)
}

// tools holds the handlers. A returned error becomes a tool result with
// IsError set, so its message reaches the model.
type tools struct{ p Player }

func (t tools) devices(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
	devs, err := t.p.Devices(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(devs) == 0 {
		return text("No Spotify devices are online. Open the Spotify app on a phone, computer or speaker; it then shows up here."), nil, nil
	}
	lines := make([]string, len(devs))
	for i, d := range devs {
		lines[i] = "- " + formatDevice(d)
	}
	return text("Devices:\n" + strings.Join(lines, "\n")), nil, nil
}

func (t tools) nowPlaying(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
	st, err := t.p.NowPlaying(ctx)
	if err != nil {
		return nil, nil, err
	}
	if st == nil || st.Item == nil {
		return text("Nothing is playing on Spotify right now."), nil, nil
	}
	state := "Paused"
	if st.Playing {
		state = "Playing"
	}
	dev := "unknown device"
	if st.Device != nil {
		dev = st.Device.Name
	}
	return text(fmt.Sprintf("%s on %s: %s [%s / %s]", state, dev, formatItem(*st.Item), mmss(st.Progress), mmss(st.Item.Duration))), nil, nil
}

func (t tools) search(ctx context.Context, _ *mcp.CallToolRequest, in searchArgs) (*mcp.CallToolResult, any, error) {
	kind, err := playback.ParseKind(in.Type)
	if err != nil {
		return nil, nil, err
	}
	items, err := t.p.Search(ctx, in.Query, kind, in.Limit)
	if err != nil {
		return nil, nil, err
	}
	if len(items) == 0 {
		return text(fmt.Sprintf("No results for %q.", in.Query)), nil, nil
	}
	lines := make([]string, len(items))
	for i, it := range items {
		lines[i] = fmt.Sprintf("%d. %s", i+1, formatItem(it))
	}
	return text(fmt.Sprintf("Results for %q:\n%s", in.Query, strings.Join(lines, "\n"))), nil, nil
}

func (t tools) play(ctx context.Context, _ *mcp.CallToolRequest, in playArgs) (*mcp.CallToolResult, any, error) {
	switch {
	case strings.TrimSpace(in.URI) != "":
		uri, err := playback.ParseURI(in.URI)
		if err != nil {
			return nil, nil, err
		}
		dev, err := t.p.Play(ctx, in.Device, uri)
		if err != nil {
			return nil, nil, err
		}
		return text("Playing " + uri.String() + on(dev) + "."), nil, nil
	case strings.TrimSpace(in.Query) != "":
		kind, err := playback.ParseKind(in.Type)
		if err != nil {
			return nil, nil, err
		}
		item, dev, err := t.p.PlayFirst(ctx, in.Device, in.Query, kind)
		if err != nil {
			return nil, nil, err
		}
		return text("Playing " + formatItem(item) + on(dev) + "."), nil, nil
	default:
		return t.resume(ctx, nil, deviceArgs{Device: in.Device})
	}
}

func (t tools) resume(ctx context.Context, _ *mcp.CallToolRequest, in deviceArgs) (*mcp.CallToolResult, any, error) {
	dev, err := t.p.Resume(ctx, in.Device)
	return done(dev, err, "Resumed playback")
}

func (t tools) pause(ctx context.Context, _ *mcp.CallToolRequest, in deviceArgs) (*mcp.CallToolResult, any, error) {
	dev, err := t.p.Pause(ctx, in.Device)
	return done(dev, err, "Paused")
}

func (t tools) next(ctx context.Context, _ *mcp.CallToolRequest, in deviceArgs) (*mcp.CallToolResult, any, error) {
	dev, err := t.p.Next(ctx, in.Device)
	return done(dev, err, "Skipped to the next track")
}

func (t tools) previous(ctx context.Context, _ *mcp.CallToolRequest, in deviceArgs) (*mcp.CallToolResult, any, error) {
	dev, err := t.p.Previous(ctx, in.Device)
	return done(dev, err, "Went back to the previous track")
}

func (t tools) transfer(ctx context.Context, _ *mcp.CallToolRequest, in transferArgs) (*mcp.CallToolResult, any, error) {
	dev, err := t.p.Transfer(ctx, in.Device, in.Play == nil || *in.Play)
	if err != nil {
		return nil, nil, err
	}
	return text("Moved playback to " + dev.Name + "."), nil, nil
}

// done turns the result of a simple command into a tool result.
func done(dev *playback.Device, err error, msg string) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return nil, nil, err
	}
	return text(msg + on(dev) + "."), nil, nil
}

func on(dev *playback.Device) string {
	if dev == nil {
		return ""
	}
	return " on " + dev.Name
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func formatDevice(d playback.Device) string {
	s := fmt.Sprintf("%s (%s, id %s)", d.Name, d.Type, d.ID)
	if d.Active {
		s += " [active]"
	}
	if d.Restricted {
		s += " [can't be controlled]"
	}
	return s
}

func formatItem(it playback.Item) string {
	s := it.Name
	if len(it.Artists) > 0 {
		s += " by " + strings.Join(it.Artists, ", ")
	}
	if it.Album != "" && it.Kind != playback.KindAlbum {
		s += " (" + it.Album + ")"
	}
	if it.Owner != "" {
		s += " (by " + it.Owner + ")"
	}
	return s + " - " + it.URI.String()
}

func mmss(d time.Duration) string {
	sec := int(d / time.Second)
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

func ptr[T any](v T) *T { return &v }
