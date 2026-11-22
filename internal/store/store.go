// Package store keeps the Spotify tokens and invite links in a data directory,
// usually a small persistent volume. Everything is written owner-only (0600)
// and replaced atomically, so a crash never leaves half a file behind.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// ErrNotFound is returned when a profile has no token yet.
var ErrNotFound = errors.New("not found")

// ErrInvalidInvite covers unknown, used and expired invites alike, so a caller
// can't tell which one it was.
var ErrInvalidInvite = errors.New("invite link is invalid or has expired")

var profileRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidProfile reports whether name can be used as a profile name. Profile
// names end up in file names, so they are restricted to a safe alphabet.
func ValidProfile(name string) bool { return profileRe.MatchString(name) }

// Token is what is kept per profile.
type Token struct {
	RefreshToken string    `json:"refresh_token"`
	AccessToken  string    `json:"access_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	// Who logged in, shown on the connect page.
	SpotifyUserID string    `json:"spotify_user_id,omitempty"`
	DisplayName   string    `json:"display_name,omitempty"`
	ConnectedAt   time.Time `json:"connected_at,omitempty"`
}

type invite struct {
	Profile string    `json:"profile"`
	Expires time.Time `json:"expires"`
}

// Store is safe for concurrent use within one process. The invite command runs
// as a second process (kubectl exec); it only adds invites, and the server
// re-reads the file on every check, so the two don't need a shared lock beyond
// the atomic rename.
type Store struct {
	dir string
	mu  sync.Mutex
	now func() time.Time
}

// Open creates the data directory if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tokens"), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return &Store{dir: dir, now: time.Now}, nil
}

func (s *Store) tokenPath(profile string) string {
	return filepath.Join(s.dir, "tokens", profile+".json")
}

// Token returns the stored token of a profile, or ErrNotFound.
func (s *Store) Token(profile string) (Token, error) {
	if !ValidProfile(profile) {
		return Token{}, fmt.Errorf("invalid profile %q", profile)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var t Token
	if err := readJSON(s.tokenPath(profile), &t); err != nil {
		return Token{}, err
	}
	return t, nil
}

// SaveToken replaces the token of a profile.
func (s *Store) SaveToken(profile string, t Token) error {
	if !ValidProfile(profile) {
		return fmt.Errorf("invalid profile %q", profile)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(s.tokenPath(profile), t)
}

// DeleteToken forgets a profile's token. Deleting a missing token is not an error.
func (s *Store) DeleteToken(profile string) error {
	if !ValidProfile(profile) {
		return fmt.Errorf("invalid profile %q", profile)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.tokenPath(profile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) invitePath() string { return filepath.Join(s.dir, "invites.json") }

// hashInvite: only the SHA-256 of an invite is stored, so reading the volume
// doesn't give anyone a usable link.
func hashInvite(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

func (s *Store) loadInvites() (map[string]invite, error) {
	inv := map[string]invite{}
	err := readJSON(s.invitePath(), &inv)
	if errors.Is(err, ErrNotFound) {
		return map[string]invite{}, nil
	}
	return inv, err
}

// saveInvites drops expired entries on the way.
func (s *Store) saveInvites(inv map[string]invite) error {
	now := s.now()
	for k, v := range inv {
		if now.After(v.Expires) {
			delete(inv, k)
		}
	}
	return writeJSON(s.invitePath(), inv)
}

// CreateInvite returns a new one-time invite code for profile, valid for ttl.
func (s *Store) CreateInvite(profile string, ttl time.Duration) (string, error) {
	if !ValidProfile(profile) {
		return "", fmt.Errorf("invalid profile %q", profile)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, err := s.loadInvites()
	if err != nil {
		return "", err
	}
	inv[hashInvite(code)] = invite{Profile: profile, Expires: s.now().Add(ttl)}
	if err := s.saveInvites(inv); err != nil {
		return "", err
	}
	return code, nil
}

// CheckInvite returns the profile an invite belongs to without using it up.
func (s *Store) CheckInvite(code string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, err := s.loadInvites()
	if err != nil {
		return "", err
	}
	v, ok := inv[hashInvite(code)]
	if !ok || s.now().After(v.Expires) {
		return "", ErrInvalidInvite
	}
	return v.Profile, nil
}

// UseInvite checks an invite and removes it, so it works only once.
func (s *Store) UseInvite(code string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, err := s.loadInvites()
	if err != nil {
		return "", err
	}
	h := hashInvite(code)
	v, ok := inv[h]
	if !ok || s.now().After(v.Expires) {
		return "", ErrInvalidInvite
	}
	delete(inv, h)
	if err := s.saveInvites(inv); err != nil {
		return "", err
	}
	return v.Profile, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
