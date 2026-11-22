package playback

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Service controls playback. Devices are chosen by a selector: a device ID,
// a name, a unique part of a name, or a device type ("smartphone"). An empty
// selector means the active device.
type Service struct {
	spotify Spotify
}

// NewService returns a Service using sp.
func NewService(sp Spotify) *Service { return &Service{spotify: sp} }

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
		names[i] = fmt.Sprintf("%q", d.Name)
		if withType {
			names[i] += " (" + d.Type + ")"
		}
	}
	return strings.Join(names, ", ")
}

// Devices lists the devices Spotify currently sees.
func (s *Service) Devices(ctx context.Context) ([]Device, error) {
	return s.spotify.Devices(ctx)
}

// NowPlaying returns the playback state, or nil when nothing is playing.
func (s *Service) NowPlaying(ctx context.Context) (*State, error) {
	return s.spotify.State(ctx)
}

// Search returns up to limit items; limit 0 means a default of 5, and more
// than MaxSearchResults is capped.
func (s *Service) Search(ctx context.Context, query string, kind Kind, limit int) ([]Item, error) {
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

// Play plays uri on the selected device and returns the device (nil for the
// active one).
func (s *Service) Play(ctx context.Context, selector string, uri URI) (*Device, error) {
	if uri.IsZero() {
		return nil, errors.New("no URI to play")
	}
	return s.control(ctx, selector, func(id DeviceID) error { return s.spotify.Play(ctx, id, uri) })
}

// PlayFirst plays the first search result for query.
func (s *Service) PlayFirst(ctx context.Context, selector, query string, kind Kind) (Item, *Device, error) {
	items, err := s.Search(ctx, query, kind, 1)
	if err != nil {
		return Item{}, nil, err
	}
	if len(items) == 0 {
		return Item{}, nil, fmt.Errorf("nothing found for %q", query)
	}
	dev, err := s.Play(ctx, selector, items[0].URI)
	return items[0], dev, err
}

// Resume continues playback.
func (s *Service) Resume(ctx context.Context, selector string) (*Device, error) {
	return s.control(ctx, selector, func(id DeviceID) error { return s.spotify.Resume(ctx, id) })
}

// Pause pauses playback.
func (s *Service) Pause(ctx context.Context, selector string) (*Device, error) {
	return s.control(ctx, selector, func(id DeviceID) error { return s.spotify.Pause(ctx, id) })
}

// Next skips to the next item.
func (s *Service) Next(ctx context.Context, selector string) (*Device, error) {
	return s.control(ctx, selector, func(id DeviceID) error { return s.spotify.Next(ctx, id) })
}

// Previous goes back to the previous item.
func (s *Service) Previous(ctx context.Context, selector string) (*Device, error) {
	return s.control(ctx, selector, func(id DeviceID) error { return s.spotify.Previous(ctx, id) })
}

// Transfer moves playback to the selected device, which is required here.
func (s *Service) Transfer(ctx context.Context, selector string, play bool) (Device, error) {
	if strings.TrimSpace(selector) == "" {
		return Device{}, errors.New("a device is required; call the devices tool to see the names")
	}
	dev, err := s.control(ctx, selector, func(id DeviceID) error { return s.spotify.Transfer(ctx, id, play) })
	if err != nil {
		return Device{}, err
	}
	return *dev, nil
}

// control resolves the selector, runs cmd on the device and explains a
// missing active device.
func (s *Service) control(ctx context.Context, selector string, cmd func(DeviceID) error) (*Device, error) {
	dev, err := s.resolve(ctx, selector)
	if err != nil {
		return nil, err
	}
	var id DeviceID
	if dev != nil {
		id = dev.ID
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
func (s *Service) resolve(ctx context.Context, selector string) (*Device, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, nil
	}
	devs, err := s.spotify.Devices(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		if string(d.ID) == selector || strings.EqualFold(d.Name, selector) {
			return &d, nil
		}
	}
	var matches []Device
	lower := strings.ToLower(selector)
	for _, d := range devs {
		if strings.Contains(strings.ToLower(d.Name), lower) || strings.EqualFold(d.Type, selector) {
			matches = append(matches, d)
		}
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}
	return nil, &DeviceNotFoundError{Selector: selector, Online: devs, Ambiguous: len(matches) > 1}
}
