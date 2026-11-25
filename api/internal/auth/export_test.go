package auth

import "time"

// SetNow replaces the service clock (tests only) so PIN lockout expiry can be
// exercised without sleeping.
func SetNow(s *Service, now func() time.Time) { s.now = now }
