package orders

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/inventory"
)

// deductSent writes the SALE stock movements for lines that just became SENT,
// inside the send transaction, at each line's sent_at. Stock never blocks a send.
func deductSent(ctx context.Context, q *sqlc.Queries, orderID uuid.UUID, ids []uuid.UUID, by uuid.UUID) error {
	items, err := q.ListOrderItems(ctx, orderID)
	if err != nil {
		return connecterr.Internal(err)
	}
	var sent []inventory.SaleLine
	for _, it := range items {
		if it.Status == "SENT" && slices.Contains(ids, it.ID) {
			sent = append(sent, inventory.SaleLineOf(it, by))
		}
	}
	return inventory.DeductSale(ctx, q, sent...)
}
