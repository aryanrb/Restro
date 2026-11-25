package orders

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
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
