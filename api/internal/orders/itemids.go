package orders

import (
	"context"

	"github.com/google/uuid"

	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// ResolveItemIDs lets item-level commands (VoidItem, MarkServed, SendItems, kitchen
// StartItems / MarkItemsReady) take a line's server id OR its client_id: client ids
// are replaced by the line's server id, anything else is passed through unchanged
// (so an unknown id still fails or is skipped as before). Both are UUIDs from
// disjoint generators, so one id never names two lines.
func ResolveItemIDs(ctx context.Context, q *sqlc.Queries, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return ids, nil
	}
	rows, err := q.OrderItemIDsByClientIDs(ctx, ids)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	if len(rows) == 0 {
		return ids, nil
	}
	server := make(map[uuid.UUID]uuid.UUID, len(rows))
	for _, r := range rows {
		server[r.ClientID] = r.ID
	}
	out := make([]uuid.UUID, len(ids))
	for i, id := range ids {
		if sid, ok := server[id]; ok {
			out[i] = sid
		} else {
			out[i] = id
		}
	}
	return out, nil
}
