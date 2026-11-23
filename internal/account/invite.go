package account

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// InviteCode is the secret part of an invite link. Service.CreateInvite
// creates new codes; ParseInviteCode reads one from a link. It is only ever
// shown once; repositories see its Hash. Like ProfileID it is an opaque
// value: the zero value is not a valid code.
type InviteCode struct{ s string }

// A code is 32 random bytes, base64url-encoded without padding.
var inviteCodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// ParseInviteCode checks a code taken from a link. Anything malformed is
// ErrInvalidInvite, like an unknown code.
func ParseInviteCode(s string) (InviteCode, error) {
	if !inviteCodeRe.MatchString(s) {
		return InviteCode{}, ErrInvalidInvite
	}
	return InviteCode{s: s}, nil
}

func newInviteCode() InviteCode { return InviteCode{s: randomString()} }

// Secret is the code itself, for building the link. Keep it out of logs.
func (c InviteCode) Secret() string { return c.s }

// String keeps the code out of logs and error messages.
func (c InviteCode) String() string { return "InviteCode(redacted)" }

// Hash is what repositories store instead of the code: its hex SHA-256, so
// reading the storage doesn't give anyone a usable link.
func (c InviteCode) Hash() InviteHash {
	sum := sha256.Sum256([]byte(c.s))
	return InviteHash{s: hex.EncodeToString(sum[:])}
}

// IsZero reports whether c is the zero value, which is not a valid code.
func (c InviteCode) IsZero() bool { return c.s == "" }

// InviteHash identifies an invite in a repository. Get one from
// InviteCode.Hash or, for stored data, ParseInviteHash.
type InviteHash struct{ s string }

var inviteHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseInviteHash checks a hash read back from a repository.
func ParseInviteHash(s string) (InviteHash, error) {
	if !inviteHashRe.MatchString(s) {
		return InviteHash{}, fmt.Errorf("invalid invite hash %q", s)
	}
	return InviteHash{s: s}, nil
}

func (h InviteHash) String() string { return h.s }

// IsZero reports whether h is the zero value, which is not a valid hash.
func (h InviteHash) IsZero() bool { return h.s == "" }

// Invite lets whoever holds its code connect or disconnect one profile.
type Invite interface {
	Hash() InviteHash
	ProfileID() ProfileID
	Expires() time.Time
}

type invite struct {
	hash      InviteHash
	profileID ProfileID
	expires   time.Time
}

func (i *invite) Hash() InviteHash     { return i.hash }
func (i *invite) ProfileID() ProfileID { return i.profileID }
func (i *invite) Expires() time.Time   { return i.expires }

// RestoreInvite rebuilds an invite a repository stored, checking each part.
func RestoreInvite(hash, profileID string, expires time.Time) (Invite, error) {
	h, err := ParseInviteHash(hash)
	if err != nil {
		return nil, err
	}
	id, err := ParseProfileID(profileID)
	if err != nil {
		return nil, err
	}
	if expires.IsZero() {
		return nil, errors.New("invite has no expiry")
	}
	return &invite{hash: h, profileID: id, expires: expires}, nil
}
