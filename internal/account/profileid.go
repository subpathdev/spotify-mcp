package account

import (
	"fmt"
	"regexp"
)

// Value types in this package are interfaces with an unexported marker method, so
// only this package implements them and every value went through one of its
// constructors. Their implementations are small comparable structs: two
// values are equal with == exactly when they hold the same text.

// ProfileID identifies a profile: 1-63 lower-case letters, digits and dashes.
// Adapters may use String() in file and key names. Get one from
// ParseProfileID.
type ProfileID interface {
	String() string
	isProfileID()
}

type profileID struct{ s string }

func (p profileID) String() string { return p.s }
func (profileID) isProfileID()     {}

var profileRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ParseProfileID checks a profile ID.
func ParseProfileID(s string) (ProfileID, error) {
	if !profileRe.MatchString(s) {
		return nil, fmt.Errorf("invalid profile %q: use 1-63 lower-case letters, digits and dashes", s)
	}
	return profileID{s: s}, nil
}
