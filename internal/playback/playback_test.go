package playback

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeSpotify struct {
	devices  []Device
	noActive bool
	results  []Item
	calls    []string
}

func (f *fakeSpotify) Devices(context.Context) ([]Device, error) { return f.devices, nil }
func (f *fakeSpotify) Search(_ context.Context, q string, k Kind, limit int) ([]Item, error) {
	f.calls = append(f.calls, "search "+q+" "+string(k))
	return f.results[:min(limit, len(f.results))], nil
}
func (f *fakeSpotify) cmd(name string, id DeviceID) error {
	f.calls = append(f.calls, name+" "+string(id))
	if f.noActive && id == "" {
		return ErrNoActiveDevice
	}
	return nil
}
func (f *fakeSpotify) Play(_ context.Context, id DeviceID, u URI) error {
	return f.cmd("play "+u.String(), id)
}
func (f *fakeSpotify) Resume(_ context.Context, id DeviceID) error   { return f.cmd("resume", id) }
func (f *fakeSpotify) Pause(_ context.Context, id DeviceID) error    { return f.cmd("pause", id) }
func (f *fakeSpotify) Next(_ context.Context, id DeviceID) error     { return f.cmd("next", id) }
func (f *fakeSpotify) Previous(_ context.Context, id DeviceID) error { return f.cmd("previous", id) }
func (f *fakeSpotify) Transfer(_ context.Context, id DeviceID, _ bool) error {
	return f.cmd("transfer", id)
}
func (f *fakeSpotify) State(context.Context) (*State, error) { return nil, nil }

func (f *fakeSpotify) lastCall() string { return f.calls[len(f.calls)-1] }

func devices() []Device {
	return []Device{
		{ID: "pc1", Name: "mobile", Type: "Computer"},
		{ID: "ph1", Name: "Jan's Pixel 8", Type: "Smartphone"},
		{ID: "sp1", Name: "Kitchen speaker", Type: "Speaker"},
		{ID: "sp2", Name: "Bath speaker", Type: "Speaker"},
	}
}

func TestResolveDevice(t *testing.T) {
	ctx := context.Background()
	f := &fakeSpotify{devices: devices()}
	s := NewService(f)
	for selector, want := range map[string]DeviceID{
		"ph1": "ph1", "MOBILE": "pc1", "pixel": "ph1", "smartphone": "ph1", "kitchen": "sp1", "": "",
	} {
		if _, err := s.Pause(ctx, selector); err != nil {
			t.Fatalf("%q: %v", selector, err)
		}
		if got := f.lastCall(); got != "pause "+string(want) {
			t.Errorf("%q: called %q", selector, got)
		}
	}
	var nf *DeviceNotFoundError
	if _, err := s.Pause(ctx, "speaker"); !errors.As(err, &nf) || !nf.Ambiguous {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := s.Pause(ctx, "car"); !errors.As(err, &nf) || nf.Ambiguous || !strings.Contains(err.Error(), `"mobile" (Computer)`) {
		t.Errorf("unknown: %v", err)
	}
}

func TestNoActiveDeviceListsDevices(t *testing.T) {
	f := &fakeSpotify{devices: devices(), noActive: true}
	_, err := NewService(f).Resume(context.Background(), "")
	if !errors.Is(err, ErrNoActiveDevice) || !strings.Contains(err.Error(), "Open the Spotify app") || !strings.Contains(err.Error(), `"Jan's Pixel 8"`) {
		t.Fatalf("got %v", err)
	}
}

func TestPlayFirst(t *testing.T) {
	uri, _ := ParseURI("spotify:track:1")
	f := &fakeSpotify{devices: devices(), results: []Item{{Kind: KindTrack, Name: "Song", URI: uri}}}
	item, dev, err := NewService(f).PlayFirst(context.Background(), "pixel", "song", KindTrack)
	if err != nil || item.Name != "Song" || dev.ID != "ph1" || f.lastCall() != "play spotify:track:1 ph1" {
		t.Fatalf("%+v %+v %v %v", item, dev, err, f.calls)
	}
	f.results = nil
	if _, _, err := NewService(f).PlayFirst(context.Background(), "", "nothing", KindTrack); err == nil {
		t.Fatal("empty search played something")
	}
}

func TestTransferNeedsDevice(t *testing.T) {
	s := NewService(&fakeSpotify{devices: devices()})
	if _, err := s.Transfer(context.Background(), " ", true); err == nil {
		t.Fatal("transfer without device accepted")
	}
	if d, err := s.Transfer(context.Background(), "pixel", true); err != nil || d.ID != "ph1" {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestParseURI(t *testing.T) {
	u, err := ParseURI(" spotify:album:abc ")
	if err != nil || u.Kind() != KindAlbum || u.String() != "spotify:album:abc" || !u.Kind().IsContext() {
		t.Fatalf("%v %v", u, err)
	}
	for _, s := range []string{"", "spotify:track:", "spotify::1", "spotify:local:1", "https://open.spotify.com/track/1", "spotify:track:1:2"} {
		if _, err := ParseURI(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestSearchLimits(t *testing.T) {
	f := &fakeSpotify{results: make([]Item, 20)}
	s := NewService(f)
	for limit, want := range map[int]int{0: 5, 3: 3, 50: MaxSearchResults} {
		items, err := s.Search(context.Background(), "x", KindTrack, limit)
		if err != nil || len(items) != want {
			t.Errorf("limit %d: %d items, %v", limit, len(items), err)
		}
	}
	if _, err := s.Search(context.Background(), "  ", KindTrack, 1); err == nil {
		t.Error("empty query accepted")
	}
}
