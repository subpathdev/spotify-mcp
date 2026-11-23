// Package playback is the domain of controlling Spotify playback: devices,
// URIs, search results and the rules for choosing a device and turning
// Spotify's failures into messages a person can act on.
//
// It talks to Spotify through the Spotify port, which an adapter implements.
package playback

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The value types below (Kind, URI, DeviceID) are interfaces with an
// unexported marker method, so only this package implements them and every
// value went through one of its constructors. Their implementations are small
// comparable structs: two values are equal with == exactly when they hold the
// same text.

// Kind is the type of a Spotify item. Get one from ParseKind, Kinds or the
// Kind* functions.
type Kind interface {
	String() string
	// IsContext reports whether items of this kind play as a context (a list
	// Spotify continues through) rather than on their own.
	IsContext() bool
	isKind()
}

type kind struct{ s string }

func (k kind) String() string  { return k.s }
func (k kind) IsContext() bool { return k.s != "track" && k.s != "episode" }
func (kind) isKind()           {}

// The kinds this server can search for and play.
func KindTrack() Kind    { return kind{"track"} }
func KindAlbum() Kind    { return kind{"album"} }
func KindArtist() Kind   { return kind{"artist"} }
func KindPlaylist() Kind { return kind{"playlist"} }
func KindShow() Kind     { return kind{"show"} }
func KindEpisode() Kind  { return kind{"episode"} }

// Kinds lists every Kind, in the order shown to people.
func Kinds() []Kind {
	return []Kind{KindTrack(), KindAlbum(), KindArtist(), KindPlaylist(), KindShow(), KindEpisode()}
}

// ParseKind checks a kind name. An empty name means KindTrack.
func ParseKind(s string) (Kind, error) {
	if s == "" {
		return KindTrack(), nil
	}
	kinds := Kinds()
	names := make([]string, len(kinds))
	for i, k := range kinds {
		if k.String() == s {
			return k, nil
		}
		names[i] = k.String()
	}
	return nil, fmt.Errorf("unknown type %q (use one of %s)", s, strings.Join(names, ", "))
}

// URI is a parsed spotify:<kind>:<id> URI. Get one from ParseURI.
type URI interface {
	// Kind of the URI's item.
	Kind() Kind
	String() string
	isURI()
}

type uri struct {
	kind Kind
	id   string
}

func (u uri) Kind() Kind     { return u.kind }
func (u uri) String() string { return "spotify:" + u.kind.String() + ":" + u.id }
func (uri) isURI()           {}

// ParseURI parses a spotify:<kind>:<id> URI.
func ParseURI(s string) (URI, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 || parts[0] != "spotify" || parts[2] == "" {
		return nil, fmt.Errorf("%q is not a Spotify URI like spotify:track:<id>", s)
	}
	if parts[1] == "" {
		return nil, fmt.Errorf("unsupported Spotify URI type %q", parts[1])
	}
	k, err := ParseKind(parts[1])
	if err != nil {
		return nil, fmt.Errorf("unsupported Spotify URI type %q", parts[1])
	}
	return uri{kind: k, id: parts[2]}, nil
}

// DeviceID is Spotify's ID of a Connect device. Get one from ParseDeviceID.
// A nil DeviceID means the active device.
type DeviceID interface {
	String() string
	isDeviceID()
}

type deviceID struct{ s string }

func (d deviceID) String() string { return d.s }
func (deviceID) isDeviceID()      {}

// ParseDeviceID checks a device ID.
func ParseDeviceID(s string) (DeviceID, error) {
	if strings.TrimSpace(s) == "" || strings.ContainsAny(s, " \t\r\n") {
		return nil, fmt.Errorf("invalid device ID %q", s)
	}
	return deviceID{s: s}, nil
}

// Device is a Spotify Connect device. Get one from NewDevice.
type Device interface {
	// ID is nil for devices Spotify gives no ID; they can't be addressed.
	ID() DeviceID
	Name() string
	// Type is Spotify's device type, e.g. "Computer" or "Smartphone".
	Type() string
	Active() bool
	// Restricted devices don't accept commands from the Web API.
	Restricted() bool
}

type device struct {
	id                 DeviceID
	name, typ          string
	active, restricted bool
}

// NewDevice builds a Device; id may be nil.
func NewDevice(id DeviceID, name, typ string, active, restricted bool) Device {
	return &device{id: id, name: name, typ: typ, active: active, restricted: restricted}
}

func (d *device) ID() DeviceID     { return d.id }
func (d *device) Name() string     { return d.name }
func (d *device) Type() string     { return d.typ }
func (d *device) Active() bool     { return d.active }
func (d *device) Restricted() bool { return d.restricted }

// Item is a search result or the playing item. Get one from NewItem.
type Item interface {
	Kind() Kind
	Name() string
	URI() URI
	Artists() []string
	// Album is the album of a track, or the show of an episode.
	Album() string
	// Owner is the owner of a playlist.
	Owner() string
	Duration() time.Duration
}

type item struct {
	name         string
	uri          URI
	artists      []string
	album, owner string
	duration     time.Duration
}

// ItemOption sets an optional part of an Item.
type ItemOption func(*item)

// WithArtists sets the artists of a track or album.
func WithArtists(names ...string) ItemOption {
	return func(it *item) { it.artists = append([]string(nil), names...) }
}

// WithAlbum sets the album of a track, or the show of an episode.
func WithAlbum(name string) ItemOption { return func(it *item) { it.album = name } }

// WithOwner sets the owner of a playlist.
func WithOwner(name string) ItemOption { return func(it *item) { it.owner = name } }

// WithDuration sets the length of a track or episode.
func WithDuration(d time.Duration) ItemOption { return func(it *item) { it.duration = d } }

// NewItem builds an Item; its kind comes from uri, which is required.
func NewItem(u URI, name string, opts ...ItemOption) (Item, error) {
	if u == nil {
		return nil, errors.New("item has no URI")
	}
	it := &item{name: name, uri: u}
	for _, o := range opts {
		o(it)
	}
	if it.duration < 0 {
		return nil, fmt.Errorf("item %s has a negative duration", u)
	}
	return it, nil
}

func (it *item) Kind() Kind              { return it.uri.Kind() }
func (it *item) Name() string            { return it.name }
func (it *item) URI() URI                { return it.uri }
func (it *item) Artists() []string       { return append([]string(nil), it.artists...) }
func (it *item) Album() string           { return it.album }
func (it *item) Owner() string           { return it.owner }
func (it *item) Duration() time.Duration { return it.duration }

// State is what is playing right now. Get one from NewState.
type State interface {
	Playing() bool
	// Device is nil if Spotify doesn't say.
	Device() Device
	// Item is nil between items or for unsupported types.
	Item() Item
	Progress() time.Duration
}

type state struct {
	playing  bool
	device   Device
	item     Item
	progress time.Duration
}

// NewState builds a State; dev and it may be nil.
func NewState(playing bool, progress time.Duration, dev Device, it Item) State {
	return &state{playing: playing, progress: progress, device: dev, item: it}
}

func (s *state) Playing() bool           { return s.playing }
func (s *state) Device() Device          { return s.device }
func (s *state) Item() Item              { return s.item }
func (s *state) Progress() time.Duration { return s.progress }

// MaxSearchResults is the most results Spotify returns per search for apps in
// development mode (February 2026 rules).
const MaxSearchResults = 10

const defaultSearchResults = 5

// Errors the Spotify port returns for Spotify's documented failures.
var (
	ErrNoActiveDevice  = errors.New("no active Spotify device")
	ErrPremiumRequired = errors.New("playback control needs Spotify Premium (both for the app owner and the connected account)")
)

// Spotify is the port to the Spotify Web API.
type Spotify interface {
	Devices(ctx context.Context) ([]Device, error)
	// Search returns up to limit (1..MaxSearchResults) items of one kind.
	Search(ctx context.Context, query string, kind Kind, limit int) ([]Item, error)
	// Play plays uri; a nil device means the active one.
	Play(ctx context.Context, device DeviceID, uri URI) error
	Resume(ctx context.Context, device DeviceID) error
	Pause(ctx context.Context, device DeviceID) error
	Next(ctx context.Context, device DeviceID) error
	Previous(ctx context.Context, device DeviceID) error
	Transfer(ctx context.Context, device DeviceID, play bool) error
	// State returns nil when nothing is playing on any device.
	State(ctx context.Context) (State, error)
}
