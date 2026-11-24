package auth

import (
	"context"

	"github.com/oneaccord/restro/api/internal/connecterr"
)

// RequireActor returns the logged-in actor or an Unauthenticated error.
func RequireActor(ctx context.Context) (*Actor, error) {
	a := ActorFrom(ctx)
	if !a.LoggedIn() {
		return nil, connecterr.Unauthenticated("login required")
	}
	return a, nil
}

// RequireManager returns the actor if it is an OWNER or MANAGER, else NotAllowed.
func RequireManager(ctx context.Context) (*Actor, error) {
	a, err := RequireActor(ctx)
	if err != nil {
		return nil, err
	}
	if !isPrivileged(a.User.Role) {
		return nil, connecterr.NotAllowed("owner or manager required")
	}
	return a, nil
}

// RequireRole returns the actor if its role is one of the allowed roles.
func RequireRole(ctx context.Context, roles ...string) (*Actor, error) {
	a, err := RequireActor(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range roles {
		if a.User.Role == r {
			return a, nil
		}
	}
	return nil, connecterr.NotAllowed("not permitted for your role")
}
