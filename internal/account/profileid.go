package account

import (
	"fmt"
	"regexp"
)

// Value types in this package (ProfileID, InviteCode, InviteHash) are opaque
// structs: their fields are unexported, so other packages get a valid value
// only from a Parse function. The only value they can make themselves is the
// zero value, which every function here rejects (see IsZero). Two values are
// equal with == exactly when they hold the same text.

// ProfileID identifies a profile: 1-63 lower-case letters, digits and dashes.
// Adapters may use String() in file and key names. Get one from
// ParseProfileID.
type ProfileID struct{ s string }

var profileRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ParseProfileID checks a profile ID.
func ParseProfileID(s string) (ProfileID, error) {
	if !profileRe.MatchString(s) {
		return ProfileID{}, fmt.Errorf("invalid profile %q: use 1-63 lower-case letters, digits and dashes", s)
	}
	return ProfileID{s: s}, nil
}

func (p ProfileID) String() string { return p.s }

// IsZero reports whether p is the zero value, which is not a valid ID.
func (p ProfileID) IsZero() bool { return p.s == "" }
