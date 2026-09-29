package reading

import "time"

// SetClock replaces the service's clock, so tests can step through
// retry backoff and stale-claim windows without sleeping.
func SetClock(s *Service, now func() time.Time) { s.now = now }
