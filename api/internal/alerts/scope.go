package alerts

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
)

// ReaderScope tells another read model (the live view) whether the caller may see
// alerts — the owner, or a manager the owner made a recipient — and whose alerts
// are hidden from them (a manager never sees alerts involving themselves; Valid
// false = none). Same rule as the AlertService reads.
func ReaderScope(ctx context.Context, q *sqlc.Queries) (ok bool, hideFor pgtype.UUID, err error) {
	actor, err := auth.RequireActor(ctx)
	if err != nil {
		return false, pgtype.UUID{}, err
	}
	switch actor.User.Role {
	case auth.RoleOwner:
		return true, pgtype.UUID{}, nil
	case auth.RoleManager:
		cfg, err := loadConfig(ctx, q)
		if err != nil {
			return false, pgtype.UUID{}, err
		}
		if cfg.isRecipient(actor.User.ID) {
			return true, dbx.UUID(actor.User.ID), nil
		}
	}
	return false, pgtype.UUID{}, nil
}

// EventAuditCreated is sent (id = audit_log id) for every audit row written; the
// live view refetches its exceptions on it.
const EventAuditCreated = "audit.created"
