package account

import "time"

// Clock tells the time. The domain never calls time.Now itself, so tests can
// set the time and invite expiry, login timeouts and token refresh are
// checked without waiting.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SystemClock is the real clock, for main.
func SystemClock() Clock { return systemClock{} }
