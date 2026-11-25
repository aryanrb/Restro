package orders

import (
	"context"
	"strings"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// PriceLines prices new lines against the live menu exactly as InsertLines would
// snapshot them, without writing anything (a counter-sale preview). Any rejected
// line rejects the whole set (INVALID field "lines", as CounterSale).
func PriceLines(ctx context.Context, q *sqlc.Queries, items []*restaurantv1.NewItem) (int64, error) {
	if len(items) == 0 {
		return 0, connecterr.Invalid("lines", "add at least one item")
	}
	var subtotal int64
	var msgs []string
	for _, ni := range items {
		pl, rej, err := priceLine(ctx, q, ni)
		if err != nil {
			return 0, err
		}
		if rej != nil {
			msgs = append(msgs, rej.msg)
			continue
		}
		subtotal += pl.unitPrice * int64(ni.GetQuantity())
	}
	if len(msgs) > 0 {
		return 0, connecterr.Invalid("lines", strings.Join(msgs, "; "))
	}
	return subtotal, nil
}
