package auth

import (
	"context"

	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// Actor is who (and what device) is behind a request. The session-loading
// interceptor puts one in context for every request; User is nil when logged out,
// Device is nil when the request is from an un-enrolled device.
type Actor struct {
	User   *sqlc.User
	Token  string // session token, for logout
	Device *sqlc.Device
}

// LoggedIn reports whether a user session is present.
func (a *Actor) LoggedIn() bool { return a != nil && a.User != nil }

type ctxKey int

const actorKey ctxKey = iota

func withActor(ctx context.Context, a *Actor) context.Context {
	return context.WithValue(ctx, actorKey, a)
}

// ActorFrom returns the request's actor. It always returns a non-nil Actor (an
// empty one when nothing was resolved), so callers can check LoggedIn().
func ActorFrom(ctx context.Context) *Actor {
	if a, ok := ctx.Value(actorKey).(*Actor); ok && a != nil {
		return a
	}
	return &Actor{}
}
