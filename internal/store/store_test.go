package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token("default"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	want := Token{RefreshToken: "r1", DisplayName: "Jan"}
	if err := s.SaveToken("default", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Token("default")
	if err != nil || got.RefreshToken != "r1" || got.DisplayName != "Jan" {
		t.Fatalf("got %+v, %v", got, err)
	}
	fi, err := os.Stat(filepath.Join(dir, "tokens", "default.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v, want 0600", fi.Mode().Perm())
	}
	// A second store on the same dir (e.g. after a pod restart) sees the token.
	s2, _ := Open(dir)
	if got, _ := s2.Token("default"); got.RefreshToken != "r1" {
		t.Fatalf("reopened store lost the token: %+v", got)
	}
	if err := s.DeleteToken("default"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteToken("default"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestProfileNames(t *testing.T) {
	s, _ := Open(t.TempDir())
	for _, p := range []string{"../x", "", "A", "a/b", strings.Repeat("a", 64)} {
		if err := s.SaveToken(p, Token{}); err == nil {
			t.Errorf("profile %q accepted", p)
		}
	}
}

func TestInvites(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	code, err := s.CreateInvite("default", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "invites.json"))
	if strings.Contains(string(raw), code) {
		t.Fatal("invite code stored in clear text")
	}
	if p, err := s.CheckInvite(code); err != nil || p != "default" {
		t.Fatalf("check: %q %v", p, err)
	}
	if _, err := s.CheckInvite("wrong"); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("wrong code: %v", err)
	}
	if p, err := s.UseInvite(code); err != nil || p != "default" {
		t.Fatalf("use: %q %v", p, err)
	}
	if _, err := s.UseInvite(code); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("second use: %v", err)
	}

	old, _ := s.CreateInvite("default", time.Hour)
	now = now.Add(2 * time.Hour)
	if _, err := s.CheckInvite(old); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("expired invite accepted: %v", err)
	}
}
