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

// Kind is the type of a Spotify item.
type Kind string

// The kinds this server can search for and play.
const (
	KindTrack    Kind = "track"
	KindAlbum    Kind = "album"
	KindArtist   Kind = "artist"
	KindPlaylist Kind = "playlist"
	KindShow     Kind = "show"
	KindEpisode  Kind = "episode"
)

// Kinds lists every Kind, in the order shown to people.
var Kinds = []Kind{KindTrack, KindAlbum, KindArtist, KindPlaylist, KindShow, KindEpisode}

// ParseKind checks a kind name. An empty name means KindTrack.
func ParseKind(s string) (Kind, error) {
	if s == "" {
		return KindTrack, nil
	}
	for _, k := range Kinds {
		if string(k) == s {
			return k, nil
		}
	}
	names := make([]string, len(Kinds))
	for i, k := range Kinds {
		names[i] = string(k)
	}
	return "", fmt.Errorf("unknown type %q (use one of %s)", s, strings.Join(names, ", "))
}

// IsContext reports whether items of this kind play as a context (a list
// Spotify continues through) rather than on their own.
func (k Kind) IsContext() bool { return k != KindTrack && k != KindEpisode }

// URI is a parsed spotify:<kind>:<id> URI. The zero URI is not valid.
type URI struct {
	kind Kind
	id   string
}

// ParseURI parses a spotify:<kind>:<id> URI.
func ParseURI(s string) (URI, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 || parts[0] != "spotify" || parts[2] == "" {
		return URI{}, fmt.Errorf("%q is not a Spotify URI like spotify:track:<id>", s)
	}
	k, err := ParseKind(parts[1])
	if err != nil || parts[1] == "" {
		return URI{}, fmt.Errorf("unsupported Spotify URI type %q", parts[1])
	}
	return URI{kind: k, id: parts[2]}, nil
}

// Kind of the URI's item.
func (u URI) Kind() Kind { return u.kind }

// IsZero reports whether u is the zero URI.
func (u URI) IsZero() bool { return u.id == "" }

func (u URI) String() string {
	if u.IsZero() {
		return ""
	}
	return "spotify:" + string(u.kind) + ":" + u.id
}

// DeviceID is Spotify's ID of a Connect device.
type DeviceID string

// Device is a Spotify Connect device.
type Device struct {
	ID   DeviceID
	Name string
	// Type is Spotify's device type, e.g. "Computer" or "Smartphone".
	Type   string
	Active bool
	// Restricted devices don't accept commands from the Web API.
	Restricted bool
}

// Item is a search result or the playing item.
type Item struct {
	Kind    Kind
	Name    string
	URI     URI
	Artists []string
	// Album is the album of a track, or the show of an episode.
	Album    string
	Owner    string // of a playlist
	Duration time.Duration
}

// State is what is playing right now.
type State struct {
	Playing  bool
	Device   *Device // nil if Spotify doesn't say
	Item     *Item   // nil between items or for unsupported types
	Progress time.Duration
}

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
	// Play plays uri; an empty device means the active one.
	Play(ctx context.Context, device DeviceID, uri URI) error
	Resume(ctx context.Context, device DeviceID) error
	Pause(ctx context.Context, device DeviceID) error
	Next(ctx context.Context, device DeviceID) error
	Previous(ctx context.Context, device DeviceID) error
	Transfer(ctx context.Context, device DeviceID, play bool) error
	// State returns nil when nothing is playing on any device.
	State(ctx context.Context) (*State, error)
}
