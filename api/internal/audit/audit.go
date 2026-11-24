// Package audit writes the append-only audit_log. Always call Write with the same
// *sqlc.Queries (tx) as the change it records, so the two commit together.
package audit

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
)

// JSON marshals v for an audit before/after column (best-effort; nil on error).
func JSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

type Entry struct {
	ActorID     uuid.UUID // uuid.Nil = none
	Action      string    // e.g. "menu.item.update", "order.void", "invoice.cancel"
	Entity      string
	EntityID    string
	Before      []byte // JSON
	After       []byte // JSON
	Reason      string
	ApproverID  uuid.UUID // manager who approved, if any
	DeviceLabel string
}

func Write(ctx context.Context, q *sqlc.Queries, e Entry) error {
	return q.InsertAudit(ctx, sqlc.InsertAuditParams{
		ActorUserID:    dbx.UUIDOpt(e.ActorID),
		Action:         e.Action,
		Entity:         e.Entity,
		EntityID:       dbx.TextOpt(e.EntityID),
		Before:         e.Before,
		After:          e.After,
		Reason:         dbx.TextOpt(e.Reason),
		ApproverUserID: dbx.UUIDOpt(e.ApproverID),
		DeviceLabel:    dbx.TextOpt(e.DeviceLabel),
	})
}
