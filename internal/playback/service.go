package playback

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Service controls playback. Devices are chosen by a selector: a device ID,
// a name, a unique part of a name, or a device type ("smartphone"). An empty
// selector means the active device; methods then return a nil Device.
type Service interface {
	// Devices lists the devices Spotify currently sees.
	Devices(ctx context.Context) ([]Device, error)
	// NowPlaying returns the playback state, or nil when nothing is playing.
	NowPlaying(ctx context.Context) (State, error)
	// Search returns up to limit items; limit 0 means a default of 5, and
	// more than MaxSearchResults is capped.
	Search(ctx context.Context, query string, kind Kind, limit int) ([]Item, error)
	// Play plays uri on the selected device.
	Play(ctx context.Context, selector string, uri URI) (Device, error)
	// PlayFirst plays the first search result for query.
	PlayFirst(ctx context.Context, selector, query string, kind Kind) (Item, Device, error)
	// Resume continues playback.
	Resume(ctx context.Context, selector string) (Device, error)
	// Pause pauses playback.
	Pause(ctx context.Context, selector string) (Device, error)
	// Next skips to the next item.
	Next(ctx context.Context, selector string) (Device, error)
	// Previous goes back to the previous item.
	Previous(ctx context.Context, selector string) (Device, error)
	// Transfer moves playback to the selected device, which is required.
	Transfer(ctx context.Context, selector string, play bool) (Device, error)
}

type service struct {
	spotify Spotify
}

// NewService returns a Service using sp.
func NewService(sp Spotify) Service { return &service{spotify: sp} }

// NoActiveDeviceError is returned when Spotify sees no active device. It lists
// the devices that could be passed instead. It matches ErrNoActiveDevice.
type NoActiveDeviceError struct {
	Online []Device
}

func (e *NoActiveDeviceError) Error() string {
	msg := "No active Spotify device. Open the Spotify app on your phone, computer or speaker (playing anything once is enough), then try again"
	if len(e.Online) > 0 {
		msg += ", or pass one of these as device: " + quotedNames(e.Online, false)
	}
	return msg + "."
}

func (e *NoActiveDeviceError) Unwrap() error { return ErrNoActiveDevice }

// DeviceNotFoundError is returned when a selector matches no device or more
// than one.
type DeviceNotFoundError struct {
	Selector string
	Online   []Device
	// Ambiguous is set when the selector matched several devices.
	Ambiguous bool
}

func (e *DeviceNotFoundError) Error() string {
	switch {
	case len(e.Online) == 0:
		return fmt.Sprintf("No Spotify devices are online. Open the Spotify app on the device %q first.", e.Selector)
	case e.Ambiguous:
		return fmt.Sprintf("%q matches more than one device: %s.", e.Selector, quotedNames(e.Online, true))
	default:
		return fmt.Sprintf("No device called %q. Online devices: %s. If yours is missing, open the Spotify app on it.", e.Selector, quotedNames(e.Online, true))
	}
}

func quotedNames(devs []Device, withType bool) string {
	names := make([]string, len(devs))
	for i, d := range devs {
		names[i] = fmt.Sprintf("%q", d.Name())
		if withType {
			names[i] += " (" + d.Type() + ")"
		}
	}
	return strings.Join(names, ", ")
}

func (s *service) Devices(ctx context.Context) ([]Device, error) {
	return s.spotify.Devices(ctx)
}

func (s *service) NowPlaying(ctx context.Context) (State, error) {
	return s.spotify.State(ctx)
}

func (s *service) Search(ctx context.Context, query string, kind Kind, limit int) ([]Item, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("search query is empty")
	}
	switch {
	case limit <= 0:
		limit = defaultSearchResults
	case limit > MaxSearchResults:
		limit = MaxSearchResults
	}
	return s.spotify.Search(ctx, query, kind, limit)
}

func (s *service) Play(ctx context.Context, selector string, uri URI) (Device, error) {
	if uri == nil {
		return nil, errors.New("no URI to play")
	}
	return s.control(ctx, selector, func(id DeviceID) error { return s.spotify.Play(ctx, id, uri) })
}

func (s *service) PlayFirst(ctx context.Context, selector, query string, kind Kind) (Item, Device, error) {
	items, err := s.Search(ctx, query, kind, 1)
	if err != nil {
		return nil, nil, err
	}
	if len(items) == 0 {
		return nil, nil, fmt.Errorf("nothing found for %q", query)
	}
	dev, err := s.Play(ctx, selector, items[0].URI())
	return items[0], dev, err
}

func (s *service) Resume(ctx context.Context, selector string) (Device, error) {
	return s.control(ctx, selector, func(id DeviceID) error { return s.spotify.Resume(ctx, id) })
}

func (s *service) Pause(ctx context.Context, selector string) (Device, error) {
	return s.control(ctx, selector, func(id DeviceID) error { return s.spotify.Pause(ctx, id) })
}

func (s *service) Next(ctx context.Context, selector string) (Device, error) {
	return s.control(ctx, selector, func(id DeviceID) error { return s.spotify.Next(ctx, id) })
}

func (s *service) Previous(ctx context.Context, selector string) (Device, error) {
	return s.control(ctx, selector, func(id DeviceID) error { return s.spotify.Previous(ctx, id) })
}

func (s *service) Transfer(ctx context.Context, selector string, play bool) (Device, error) {
	if strings.TrimSpace(selector) == "" {
		return nil, errors.New("a device is required; call the devices tool to see the names")
	}
	return s.control(ctx, selector, func(id DeviceID) error { return s.spotify.Transfer(ctx, id, play) })
}

// control resolves the selector, runs cmd on the device and explains a
// missing active device.
func (s *service) control(ctx context.Context, selector string, cmd func(DeviceID) error) (Device, error) {
	dev, err := s.resolve(ctx, selector)
	if err != nil {
		return nil, err
	}
	var id DeviceID // nil: the active device
	if dev != nil {
		if id = dev.ID(); id == nil {
			return nil, fmt.Errorf("the device %q has no ID and can't be controlled", dev.Name())
		}
	}
	if err := cmd(id); err != nil {
		if errors.Is(err, ErrNoActiveDevice) {
			online, _ := s.spotify.Devices(ctx) // best effort, only for the message
			return nil, &NoActiveDeviceError{Online: online}
		}
		return nil, err
	}
	return dev, nil
}

// resolve finds the device a selector means: exact ID, then exact name
// (ignoring case), then a single device whose name contains the selector or
// whose type equals it. An empty selector returns nil, the active device.
func (s *service) resolve(ctx context.Context, selector string) (Device, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, nil
	}
	devs, err := s.spotify.Devices(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		if (d.ID() != nil && d.ID().String() == selector) || strings.EqualFold(d.Name(), selector) {
			return d, nil
		}
	}
	var matches []Device
	lower := strings.ToLower(selector)
	for _, d := range devs {
		if strings.Contains(strings.ToLower(d.Name()), lower) || strings.EqualFold(d.Type(), selector) {
			matches = append(matches, d)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return nil, &DeviceNotFoundError{Selector: selector, Online: devs, Ambiguous: len(matches) > 1}
}
