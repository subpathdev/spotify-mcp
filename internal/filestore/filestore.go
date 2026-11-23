// Package filestore keeps connections and invites as JSON files in a data
// directory, usually a small persistent volume. It implements account.Tokens
// and account.Invites.
//
// Files are owner-only (0600) and replaced atomically (temp file + rename), so
// a crash never leaves half a file behind. Every operation holds a lock that
// covers both goroutines (a mutex) and other processes (flock on a lock file),
// because the invite command runs as a second process next to the server.
package filestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/subpathdev/spotify-mcp/internal/account"
)

// Store is safe for concurrent use, also by several processes on one directory.
type Store struct {
	dir string
	mu  sync.Mutex
}

var (
	_ account.Tokens  = (*Store)(nil)
	_ account.Invites = (*Store)(nil)
)

// Open creates the data directory if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tokens"), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// connectionRecord is the on-disk format of a connection.
type connectionRecord struct {
	RefreshToken  string    `json:"refresh_token"`
	AccessToken   string    `json:"access_token,omitempty"`
	Expiry        time.Time `json:"expiry,omitzero"`
	SpotifyUserID string    `json:"spotify_user_id,omitempty"`
	DisplayName   string    `json:"display_name,omitempty"`
	ConnectedAt   time.Time `json:"connected_at,omitzero"`
}

func toRecord(c account.Connection) connectionRecord {
	return connectionRecord{
		RefreshToken: c.Token.RefreshToken, AccessToken: c.Token.AccessToken, Expiry: c.Token.Expiry,
		SpotifyUserID: c.User.ID, DisplayName: c.User.DisplayName, ConnectedAt: c.ConnectedAt,
	}
}

func (r connectionRecord) connection() account.Connection {
	return account.Connection{
		Token:       account.Token{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, Expiry: r.Expiry},
		User:        account.User{ID: r.SpotifyUserID, DisplayName: r.DisplayName},
		ConnectedAt: r.ConnectedAt,
	}
}

// inviteRecord is the on-disk format of an invite; invites.json maps the
// invite hash to it.
type inviteRecord struct {
	Profile string    `json:"profile"`
	Expires time.Time `json:"expires"`
}

func (r inviteRecord) invite(h account.InviteHash) (account.Invite, error) {
	p, err := account.ParseProfile(r.Profile)
	if err != nil {
		return account.Invite{}, fmt.Errorf("invite %s: %w", h, err)
	}
	return account.Invite{Hash: h, Profile: p, Expires: r.Expires}, nil
}

// lock takes the in-process and the cross-process lock.
func (s *Store) lock() (unlock func(), err error) {
	s.mu.Lock()
	release, err := lockFile(filepath.Join(s.dir, ".lock"))
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("lock data dir: %w", err)
	}
	return func() { release(); s.mu.Unlock() }, nil
}

func (s *Store) connectionPath(p account.Profile) (string, error) {
	if p == nil {
		return "", errors.New("no profile given")
	}
	// ParseProfile validates IDs, but Profile is an interface that other
	// code may implement; check again because the ID ends up in a file name.
	id := p.ID()
	if _, err := account.ParseProfile(id); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, "tokens", id+".json"), nil
}

// Connection implements account.Tokens.
func (s *Store) Connection(_ context.Context, p account.Profile) (account.Connection, error) {
	path, err := s.connectionPath(p)
	if err != nil {
		return account.Connection{}, err
	}
	unlock, err := s.lock()
	if err != nil {
		return account.Connection{}, err
	}
	defer unlock()
	var r connectionRecord
	if err := readJSON(path, &r); err != nil {
		return account.Connection{}, err
	}
	return r.connection(), nil
}

// SaveConnection implements account.Tokens.
func (s *Store) SaveConnection(_ context.Context, p account.Profile, c account.Connection) error {
	path, err := s.connectionPath(p)
	if err != nil {
		return err
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return writeJSON(path, toRecord(c))
}

// UpdateConnection implements account.Tokens. fn runs under the lock.
func (s *Store) UpdateConnection(_ context.Context, p account.Profile, fn func(*account.Connection) error) error {
	path, err := s.connectionPath(p)
	if err != nil {
		return err
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	var r connectionRecord
	if err := readJSON(path, &r); err != nil {
		return err
	}
	c := r.connection()
	if err := fn(&c); err != nil {
		return err
	}
	return writeJSON(path, toRecord(c))
}

// DeleteConnection implements account.Tokens.
func (s *Store) DeleteConnection(_ context.Context, p account.Profile) error {
	path, err := s.connectionPath(p)
	if err != nil {
		return err
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) invitePath() string { return filepath.Join(s.dir, "invites.json") }

func (s *Store) loadInvites() (map[account.InviteHash]inviteRecord, error) {
	inv := map[account.InviteHash]inviteRecord{}
	if err := readJSON(s.invitePath(), &inv); err != nil && !errors.Is(err, account.ErrNotFound) {
		return nil, err
	}
	return inv, nil
}

// changeInvites runs fn on the invites under the lock and writes them back.
func (s *Store) changeInvites(fn func(map[account.InviteHash]inviteRecord) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	inv, err := s.loadInvites()
	if err != nil {
		return err
	}
	if err := fn(inv); err != nil {
		return err
	}
	return writeJSON(s.invitePath(), inv)
}

// AddInvite implements account.Invites.
func (s *Store) AddInvite(_ context.Context, i account.Invite) error {
	return s.changeInvites(func(inv map[account.InviteHash]inviteRecord) error {
		inv[i.Hash] = inviteRecord{Profile: i.Profile.ID(), Expires: i.Expires}
		return nil
	})
}

// Invite implements account.Invites.
func (s *Store) Invite(_ context.Context, h account.InviteHash) (account.Invite, error) {
	unlock, err := s.lock()
	if err != nil {
		return account.Invite{}, err
	}
	defer unlock()
	inv, err := s.loadInvites()
	if err != nil {
		return account.Invite{}, err
	}
	r, ok := inv[h]
	if !ok {
		return account.Invite{}, account.ErrNotFound
	}
	return r.invite(h)
}

// TakeInvite implements account.Invites.
func (s *Store) TakeInvite(_ context.Context, h account.InviteHash) (account.Invite, error) {
	var out account.Invite
	err := s.changeInvites(func(inv map[account.InviteHash]inviteRecord) error {
		r, ok := inv[h]
		if !ok {
			return account.ErrNotFound
		}
		i, err := r.invite(h)
		if err != nil {
			return err // nothing is written, so the invite stays until it expires
		}
		delete(inv, h)
		out = i
		return nil
	})
	return out, err
}

// PruneInvites implements account.Invites.
func (s *Store) PruneInvites(_ context.Context, t time.Time) error {
	return s.changeInvites(func(inv map[account.InviteHash]inviteRecord) error {
		for h, r := range inv {
			if r.Expires.Before(t) {
				delete(inv, h)
			}
		}
		return nil
	})
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return account.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return nil
}

func writeJSON(path string, v any) (err error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
