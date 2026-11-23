package account

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// An invite lets whoever holds its code connect or disconnect one profile.
// InviteCode and InviteHash are sealed value interfaces like ProfileID.

// InviteCode is the secret part of an invite link. Service.CreateInvite
// creates new codes; ParseInviteCode reads one from a link. It is only ever shown once;
// repositories see its hash.
type InviteCode interface {
	// Secret is the code itself, for building the link. Keep it out of logs.
	Secret() string
	isInviteCode()
}

type inviteCode struct{ s string }

func (c inviteCode) Secret() string { return c.s }
func (inviteCode) isInviteCode()    {}

// String keeps the code out of logs and error messages.
func (inviteCode) String() string { return "InviteCode(redacted)" }

// A code is 32 random bytes, base64url-encoded without padding.
var inviteCodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// ParseInviteCode checks a code taken from a link. Anything malformed is
// ErrInvalidInvite, like an unknown code.
func ParseInviteCode(s string) (InviteCode, error) {
	if !inviteCodeRe.MatchString(s) {
		return nil, ErrInvalidInvite
	}
	return inviteCode{s: s}, nil
}

func newInviteCode() InviteCode { return inviteCode{s: randomString()} }

// InviteHash identifies an invite in a repository: the hex SHA-256 of its
// code, so reading the storage doesn't give anyone a usable link.
type InviteHash interface {
	String() string
	isInviteHash()
}

type inviteHash struct{ s string }

func (h inviteHash) String() string { return h.s }
func (inviteHash) isInviteHash()    {}

var inviteHashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseInviteHash checks a hash read back from a repository.
func ParseInviteHash(s string) (InviteHash, error) {
	if !inviteHashRe.MatchString(s) {
		return nil, fmt.Errorf("invalid invite hash %q", s)
	}
	return inviteHash{s: s}, nil
}

func hashInvite(code InviteCode) InviteHash {
	sum := sha256.Sum256([]byte(code.Secret()))
	return inviteHash{s: hex.EncodeToString(sum[:])}
}

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
