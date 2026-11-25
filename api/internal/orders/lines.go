package orders

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/inventory"
)

// pricedLine is a validated NewItem ready to snapshot into order_items.
type pricedLine struct {
	item      sqlc.MenuItem
	variant   *sqlc.ItemVariant
	modifiers []sqlc.Modifier
	// unitPrice is the variant (or item) price plus every chosen modifier's delta:
	// the per-unit amount the guest pays, snapshotted on the line.
	unitPrice int64
}

// lineRejection is a per-line refusal (partial accept), not a command failure.
type lineRejection struct {
	reason restaurantv1.ErrorReason
	msg    string
}

func invalidLine(format string, args ...any) *lineRejection {
	return &lineRejection{reason: restaurantv1.ErrorReason_ERROR_REASON_INVALID, msg: fmt.Sprintf(format, args...)}
}

// priceLine validates a NewItem against the live menu and prices it. It rejects
// (per line) an unknown or inactive item, an 86'd item, a variant that is not the
// item's (or a missing one when the item has variants), an unknown or duplicated
// modifier, a modifier from a group not linked to the item, and any linked
// group's min/max violation. A non-nil error is an infrastructure failure.
func priceLine(ctx context.Context, q *sqlc.Queries, ni *restaurantv1.NewItem) (*pricedLine, *lineRejection, error) {
	if ni.GetQuantity() < 1 {
		return nil, invalidLine("quantity must be at least 1"), nil
	}
	itemID, err := uuid.Parse(ni.GetMenuItemId())
	if err != nil {
		return nil, invalidLine("item not found"), nil
	}
	item, err := q.GetItem(ctx, itemID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, invalidLine("item not found"), nil
	}
	if err != nil {
		return nil, nil, connecterr.Internal(err)
	}
	if !item.Active {
		return nil, invalidLine("%s is not on the menu", item.NameEn), nil
	}
	if item.Is86d {
		return nil, &lineRejection{reason: restaurantv1.ErrorReason_ERROR_REASON_ITEM_86D, msg: item.NameEn + " is out of stock"}, nil
	}
	out := &pricedLine{item: item, unitPrice: item.PricePaisa}

	// Variant: must be one of this item's; required when the item has any.
	variants, err := q.ListVariantsForItem(ctx, item.ID)
	if err != nil {
		return nil, nil, connecterr.Internal(err)
	}
	if ni.VariantId != nil {
		for i := range variants {
			if variants[i].ID.String() == ni.GetVariantId() {
				out.variant = &variants[i]
				break
			}
		}
		if out.variant == nil {
			return nil, invalidLine("variant is not an option of %s", item.NameEn), nil
		}
		out.unitPrice = out.variant.PricePaisa
	} else if len(variants) > 0 {
		return nil, invalidLine("choose a variant of %s", item.NameEn), nil
	}

	// Modifiers: each must exist, be chosen once, and belong to a group linked to
	// the item; then every linked group's min/max must hold.
	groups, err := q.ListModifierGroupsForItem(ctx, item.ID)
	if err != nil {
		return nil, nil, connecterr.Internal(err)
	}
	linked := make(map[uuid.UUID]bool, len(groups))
	for _, g := range groups {
		linked[g.ID] = true
	}
	chosen := map[uuid.UUID]int32{}
	seen := map[uuid.UUID]bool{}
	for _, mid := range ni.GetModifierIds() {
		id, perr := uuid.Parse(mid)
		if perr != nil {
			return nil, invalidLine("modifier not found"), nil
		}
		if seen[id] {
			return nil, invalidLine("a modifier was chosen twice"), nil
		}
		seen[id] = true
		m, merr := q.GetModifierByID(ctx, id)
		if errors.Is(merr, pgx.ErrNoRows) {
			return nil, invalidLine("modifier not found"), nil
		}
		if merr != nil {
			return nil, nil, connecterr.Internal(merr)
		}
		if !linked[m.GroupID] {
			return nil, invalidLine("%s is not an option of %s", m.NameEn, item.NameEn), nil
		}
		chosen[m.GroupID]++
		out.modifiers = append(out.modifiers, m)
		out.unitPrice += m.PriceDeltaPaisa
	}
	for _, g := range groups {
		n := chosen[g.ID]
		if n < g.MinSelect {
			return nil, invalidLine("%s: choose at least %d", g.NameEn, g.MinSelect), nil
		}
		if n > g.MaxSelect {
			return nil, invalidLine("%s: choose at most %d", g.NameEn, g.MaxSelect), nil
		}
	}
	return out, nil, nil
}

// InsertLines validates, prices and inserts NewItems on an order inside the
// caller's transaction, with the given status (DRAFT, or SENT for one-step
// add+send). It is the shared core of AddItems and billing's CounterSale.
//
// Per-line rejections (invalid/86'd item, bad variant/modifiers, a client_id
// already used on another order) are returned, not failed; the caller decides
// whether a partial accept is acceptable. A line created SENT deducts its recipe
// from stock (inventory.DeductSale) in the same transaction. Lines record the signed-in user from
// ctx (auth.ActorFrom) as added_by_user_id, and a line created SENT gets sent_at. A line whose client_id is already on
// this order is an idempotent no-op (neither applied nor rejected). applied is the
// number of lines inserted; the caller bumps the order version when it is > 0.
// A non-nil error is an infrastructure failure.
func InsertLines(ctx context.Context, q *sqlc.Queries, orderID uuid.UUID, items []*restaurantv1.NewItem, status string) (applied int, rejected []*restaurantv1.RejectedLine, err error) {
	var addedBy pgtype.UUID
	if a := auth.ActorFrom(ctx); a.LoggedIn() {
		addedBy = dbx.UUID(a.User.ID)
	}
	reject := func(clientID string, reason restaurantv1.ErrorReason, msg string) {
		rejected = append(rejected, &restaurantv1.RejectedLine{ClientId: clientID, Reason: reason, Message: msg})
	}
	var sent []inventory.SaleLine // lines created SENT: deducted together at the end
	for _, ni := range items {
		lineCID, perr := uuid.Parse(ni.GetClientId())
		if perr != nil {
			reject(ni.GetClientId(), restaurantv1.ErrorReason_ERROR_REASON_INVALID, "line client_id is not a valid id")
			continue
		}
		// Per-line idempotency: a line already on this order (a re-sent queue entry
		// under a new command id) is a no-op, checked before validation so a later
		// 86 or menu edit cannot turn a replay into a rejection.
		if existing, ge := q.GetOrderItemByClientID(ctx, lineCID); ge == nil {
			if existing.OrderID != orderID {
				reject(ni.GetClientId(), restaurantv1.ErrorReason_ERROR_REASON_INVALID, "line client_id is already used on another order")
			}
			continue
		} else if !errors.Is(ge, pgx.ErrNoRows) {
			return 0, nil, connecterr.Internal(ge)
		}

		pl, rej, ve := priceLine(ctx, q, ni)
		if ve != nil {
			return 0, nil, ve
		}
		if rej != nil {
			reject(ni.GetClientId(), rej.reason, rej.msg)
			continue
		}
		varEn, varNe, hasVar := "", "", false
		if pl.variant != nil {
			varEn, varNe, hasVar = pl.variant.NameEn, pl.variant.NameNe, true
		}
		oi, ce := q.CreateOrderItem(ctx, sqlc.CreateOrderItemParams{
			OrderID: orderID, ClientID: lineCID, MenuItemID: pl.item.ID,
			NameEn: pl.item.NameEn, NameNe: pl.item.NameNe, VariantNameEn: varEn, VariantNameNe: varNe, HasVariant: hasVar,
			UnitPricePaisa: pl.unitPrice, Quantity: ni.GetQuantity(), Note: ni.GetNote(), StationID: pl.item.StationID,
			Status: status, AddedByUserID: addedBy,
		})
		if errors.Is(ce, pgx.ErrNoRows) {
			// Inserted concurrently (or earlier in this same request): same rule as above.
			existing, ge := q.GetOrderItemByClientID(ctx, lineCID)
			if ge != nil {
				return 0, nil, connecterr.Internal(ge)
			}
			if existing.OrderID != orderID {
				reject(ni.GetClientId(), restaurantv1.ErrorReason_ERROR_REASON_INVALID, "line client_id is already used on another order")
			}
			continue
		}
		if ce != nil {
			return 0, nil, connecterr.Internal(ce)
		}
		modIDs := make([]uuid.UUID, 0, len(pl.modifiers))
		for _, m := range pl.modifiers {
			if e := q.CreateOrderItemModifier(ctx, sqlc.CreateOrderItemModifierParams{
				OrderItemID: oi.ID, NameEn: m.NameEn, NameNe: m.NameNe, PriceDeltaPaisa: m.PriceDeltaPaisa,
			}); e != nil {
				return 0, nil, connecterr.Internal(e)
			}
			modIDs = append(modIDs, m.ID)
		}
		// Stock: remember what the line was ordered as; a line created SENT (one-step
		// send, counter sale) deducts its recipe in this transaction.
		variantID := uuid.Nil
		if pl.variant != nil {
			variantID = pl.variant.ID
		}
		if e := inventory.StockKeys(ctx, q, oi.ID, variantID, modIDs); e != nil {
			return 0, nil, e
		}
		oi.VariantID, oi.ModifierIds = dbx.UUIDOpt(variantID), modIDs
		if status == "SENT" {
			sent = append(sent, inventory.SaleLineOf(oi, uuid.UUID(addedBy.Bytes)))
		}
		applied++
	}
	if e := inventory.DeductSale(ctx, q, sent...); e != nil {
		return 0, nil, e
	}
	return applied, rejected, nil
}
