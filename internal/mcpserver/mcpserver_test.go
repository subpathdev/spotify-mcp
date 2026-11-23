package mcpserver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/subpathdev/spotify-mcp/internal/playback"
)

// fakePlayer records calls; err, if set, is returned by every command.
type fakePlayer struct {
	calls []string
	err   error
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

var (
	pixel = playback.NewDevice(must(playback.ParseDeviceID("ph1")), "Jan's Pixel 8", "Smartphone", true, false)
	song  = must(playback.NewItem(must(playback.ParseURI("spotify:track:s1")), "Song",
		playback.WithArtists("Band"), playback.WithAlbum("LP"), playback.WithDuration(3*time.Minute)))
)

func (f *fakePlayer) record(s string) { f.calls = append(f.calls, s) }

func (f *fakePlayer) Devices(context.Context) ([]playback.Device, error) {
	return []playback.Device{pixel}, nil
}
func (f *fakePlayer) NowPlaying(context.Context) (playback.State, error) {
	return playback.NewState(true, 61*time.Second, pixel, song), nil
}
func (f *fakePlayer) Search(_ context.Context, q string, k playback.Kind, limit int) ([]playback.Item, error) {
	f.record("search " + q + " " + k.String())
	return []playback.Item{song}, nil
}
func (f *fakePlayer) Play(_ context.Context, sel string, u playback.URI) (playback.Device, error) {
	f.record("play " + sel + " " + u.String())
	return pixel, f.err
}
func (f *fakePlayer) PlayFirst(_ context.Context, sel, q string, k playback.Kind) (playback.Item, playback.Device, error) {
	f.record("playfirst " + sel + " " + q + " " + k.String())
	return song, nil, f.err
}
func (f *fakePlayer) Resume(_ context.Context, sel string) (playback.Device, error) {
	f.record("resume " + sel)
	return nil, f.err
}
func (f *fakePlayer) Pause(_ context.Context, sel string) (playback.Device, error) {
	f.record("pause " + sel)
	return nil, f.err
}
func (f *fakePlayer) Next(_ context.Context, sel string) (playback.Device, error) {
	f.record("next " + sel)
	return nil, f.err
}
func (f *fakePlayer) Previous(_ context.Context, sel string) (playback.Device, error) {
	f.record("previous " + sel)
	return nil, f.err
}
func (f *fakePlayer) Transfer(_ context.Context, sel string, play bool) (playback.Device, error) {
	f.record("transfer " + sel)
	return pixel, f.err
}

func session(t *testing.T, p Player) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := NewServer(p, "test").Connect(ctx, st, nil); err != nil {
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

func TestToolList(t *testing.T) {
	res, err := session(t, &fakePlayer{}).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	if got := strings.Join(names, ","); got != "devices,next,now_playing,pause,play,previous,resume,search,transfer" {
		t.Fatalf("tools %s", got)
	}
}

func TestPlayVariants(t *testing.T) {
	f := &fakePlayer{}
	cs := session(t, f)
	for _, tc := range []struct {
		args     map[string]any
		wantCall string
		wantText string
	}{
		{map[string]any{"uri": "spotify:album:1", "device": "pixel"}, "play pixel spotify:album:1", "Playing spotify:album:1 on Jan's Pixel 8."},
		{map[string]any{"query": "song", "type": "track"}, "playfirst  song track", "Playing Song by Band (LP)"},
		{nil, "resume ", "Resumed playback."},
	} {
		out, isErr := call(t, cs, "play", tc.args)
		if isErr || !strings.Contains(out, tc.wantText) || f.calls[len(f.calls)-1] != tc.wantCall {
			t.Errorf("%v: %v %q, calls %v", tc.args, isErr, out, f.calls)
		}
	}
	for _, args := range []map[string]any{{"uri": "spotify:bogus:1"}, {"query": "x", "type": "podcast"}} {
		if _, isErr := call(t, cs, "play", args); !isErr {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestErrorsReachTheModel(t *testing.T) {
	f := &fakePlayer{err: errors.New("No active Spotify device. Open the Spotify app")}
	out, isErr := call(t, session(t, f), "pause", nil)
	if !isErr || !strings.Contains(out, "Open the Spotify app") {
		t.Fatalf("%v %q", isErr, out)
	}
}

func TestReadTools(t *testing.T) {
	cs := session(t, &fakePlayer{})
	if out, _ := call(t, cs, "now_playing", nil); !strings.Contains(out, "Playing on Jan's Pixel 8: Song by Band (LP)") || !strings.Contains(out, "1:01 / 3:00") {
		t.Errorf("now_playing: %s", out)
	}
	if out, _ := call(t, cs, "devices", nil); !strings.Contains(out, "Jan's Pixel 8 (Smartphone, id ph1) [active]") {
		t.Errorf("devices: %s", out)
	}
	if out, _ := call(t, cs, "transfer", map[string]any{"device": "pixel"}); out != "Moved playback to Jan's Pixel 8." {
		t.Errorf("transfer: %s", out)
	}
}
