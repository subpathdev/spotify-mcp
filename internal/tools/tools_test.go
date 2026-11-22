package tools

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/subpathdev/spotify-mcp/internal/spotify"
)

type fakePlayer struct {
	devices  []spotify.Device
	noActive bool
	calls    []string
}

func (f *fakePlayer) Devices(context.Context) ([]spotify.Device, error) { return f.devices, nil }
func (f *fakePlayer) Search(_ context.Context, q, typ string, limit int) ([]spotify.Item, error) {
	f.calls = append(f.calls, "search "+q+" "+typ)
	return []spotify.Item{{Type: "track", Name: "Song", URI: "spotify:track:1", Artists: "Band"}}, nil
}
func (f *fakePlayer) Play(_ context.Context, dev, uri string) error {
	f.calls = append(f.calls, "play "+dev+" "+uri)
	if f.noActive && dev == "" {
		return spotify.ErrNoActiveDevice
	}
	return nil
}
func (f *fakePlayer) Pause(_ context.Context, dev string) error {
	f.calls = append(f.calls, "pause "+dev)
	if f.noActive && dev == "" {
		return spotify.ErrNoActiveDevice
	}
	return nil
}
func (f *fakePlayer) Next(_ context.Context, dev string) error {
	f.calls = append(f.calls, "next "+dev)
	return nil
}
func (f *fakePlayer) Previous(_ context.Context, dev string) error {
	f.calls = append(f.calls, "previous "+dev)
	return nil
}
func (f *fakePlayer) Transfer(_ context.Context, dev string, play bool) error {
	f.calls = append(f.calls, "transfer "+dev)
	return nil
}
func (f *fakePlayer) NowPlaying(context.Context) (*spotify.Playback, error) {
	return &spotify.Playback{IsPlaying: true, Device: &f.devices[0], Item: &spotify.Item{Name: "Song", URI: "spotify:track:1"}, ProgressMS: 61000, DurationMS: 180000}, nil
}

func session(t *testing.T, p Player) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	s := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	Register(s, p)
	ct, st := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func devices() []spotify.Device {
	return []spotify.Device{
		{ID: "pc1", Name: "mobile", Type: "Computer"},
		{ID: "ph1", Name: "Jan's Pixel 8", Type: "Smartphone"},
	}
}

func TestToolList(t *testing.T) {
	cs := session(t, &fakePlayer{devices: devices()})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	want := "devices,next,now_playing,pause,play,previous,resume,search,transfer"
	if strings.Join(names, ",") != want {
		t.Fatalf("tools %v", names)
	}
}

func TestPlayByQueryOnNamedDevice(t *testing.T) {
	f := &fakePlayer{devices: devices()}
	cs := session(t, f)
	out, isErr := call(t, cs, "play", map[string]any{"query": "song", "device": "pixel"})
	if isErr || !strings.Contains(out, "Playing Song by Band") || !strings.Contains(out, "Pixel 8") {
		t.Fatalf("%v %s", isErr, out)
	}
	if got := f.calls[len(f.calls)-1]; got != "play ph1 spotify:track:1" {
		t.Fatalf("last call %q", got)
	}
}

func TestTransferByType(t *testing.T) {
	f := &fakePlayer{devices: devices()}
	cs := session(t, f)
	out, isErr := call(t, cs, "transfer", map[string]any{"device": "smartphone"})
	if isErr || !strings.Contains(out, "Pixel 8") {
		t.Fatalf("%v %s", isErr, out)
	}
	out, isErr = call(t, cs, "transfer", map[string]any{"device": "kitchen"})
	if !isErr || !strings.Contains(out, "no device called") || !strings.Contains(out, "mobile") {
		t.Fatalf("unknown device: %v %s", isErr, out)
	}
}

func TestNoActiveDeviceMessage(t *testing.T) {
	f := &fakePlayer{devices: devices(), noActive: true}
	cs := session(t, f)
	out, isErr := call(t, cs, "pause", nil)
	if !isErr || !strings.Contains(out, "No active Spotify device") || !strings.Contains(out, `"Jan's Pixel 8"`) {
		t.Fatalf("%v %s", isErr, out)
	}
	out, _ = call(t, cs, "resume", nil)
	if !strings.Contains(out, "Open the Spotify app") {
		t.Fatalf("resume: %s", out)
	}
}

func TestSimpleControls(t *testing.T) {
	f := &fakePlayer{devices: devices()}
	cs := session(t, f)
	for name, want := range map[string]string{"next": "next", "previous": "previous", "pause": "Paused", "resume": "Resumed"} {
		out, isErr := call(t, cs, name, nil)
		if isErr || !strings.Contains(out, want) {
			t.Errorf("%s: %v %s", name, isErr, out)
		}
	}
	out, _ := call(t, cs, "now_playing", nil)
	if !strings.Contains(out, "Playing on mobile: Song") || !strings.Contains(out, "1:01 / 3:00") {
		t.Fatalf("now_playing: %s", out)
	}
	out, _ = call(t, cs, "devices", nil)
	if !strings.Contains(out, "Jan's Pixel 8 (Smartphone, id ph1)") {
		t.Fatalf("devices: %s", out)
	}
	if _, isErr := call(t, cs, "play", map[string]any{"uri": "spotify:bogus:1"}); !isErr {
		t.Fatal("bogus URI accepted")
	}
}
