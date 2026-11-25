package inventory

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// PurchaseMove is one stock line of a purchase bill or return: quantity in
// thousandths of the item's base unit (positive), cost per base unit ×10,000, and
// the purchase line it belongs to (the movement's ref).
type PurchaseMove struct {
	ItemID   uuid.UUID
	QtyMilli int64
	CostE4   int64
	LineID   uuid.UUID
}

// PostPurchase writes a PURCHASE movement per line (inside the caller's
// transaction) and moves each item's unit cost to the moving weighted average:
// (max(on hand, 0) × old + qty × cost) / (max(on hand, 0) + qty).
func PostPurchase(ctx context.Context, q *sqlc.Queries, lines []PurchaseMove, by uuid.UUID, at time.Time) error {
	ordered := slices.Clone(lines)
	// Lock items in id order, like record, so concurrent writers can't deadlock.
	slices.SortStableFunc(ordered, func(a, b PurchaseMove) int { return slices.Compare(a.ItemID[:], b.ItemID[:]) })
	for _, l := range ordered {
		if l.QtyMilli <= 0 {
			continue
		}
		it, err := q.GetInventoryItemForUpdate(ctx, l.ItemID)
		if errors.Is(err, pgx.ErrNoRows) {
			return connecterr.Invalid("lines", "stock item not found")
		}
		if err != nil {
			return connecterr.Internal(err)
		}
		avg := MovingAverageE4(it.OnHandMilli, it.UnitCostE4, l.QtyMilli, l.CostE4)
		if err := q.SetInventoryUnitCost(ctx, sqlc.SetInventoryUnitCostParams{ID: it.ID, UnitCostE4: avg}); err != nil {
			return connecterr.Internal(err)
		}
		if err := record(ctx, q, []move{{itemID: l.ItemID, qtyMilli: l.QtyMilli, typ: TypePurchase, ref: l.LineID, by: by, at: at, costE4: l.CostE4}}); err != nil {
			return err
		}
	}
	if len(lines) > 0 {
		queue(ctx, realtime.Event{Type: "inventory.updated", ID: "inventory"})
	}
	return nil
}

// PostPurchaseReturn writes PURCHASE_RETURN movements (−qty) at each line's own
// cost; the average cost is left as it is. reason is "void" for a voided bill.
func PostPurchaseReturn(ctx context.Context, q *sqlc.Queries, lines []PurchaseMove, by uuid.UUID, at time.Time, reason string) error {
	moves := make([]move, 0, len(lines))
	for _, l := range lines {
		if l.QtyMilli <= 0 {
			continue
		}
		moves = append(moves, move{itemID: l.ItemID, qtyMilli: -l.QtyMilli, typ: TypePurchaseReturn, ref: l.LineID, reason: reason, by: by, at: at, costE4: l.CostE4})
	}
	return record(ctx, q, moves)
}
