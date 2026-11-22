package account

import "time"

// Option configures a Service, Inviter or TokenSource.
type Option func(*settings)

type settings struct {
	now func() time.Time
}

// WithClock replaces time.Now, e.g. in tests.
func WithClock(now func() time.Time) Option { return func(s *settings) { s.now = now } }

func apply(opts []Option) settings {
	s := settings{now: time.Now}
	for _, o := range opts {
		o(&s)
	}
	return s
}
