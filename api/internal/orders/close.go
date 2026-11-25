package orders

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// CloseIfDone applies the order close rule inside the caller's transaction and
// reports whether it closed the order.
//
// An OPEN or BILLED order becomes CLOSED when it has at least one non-voided line
// and every non-voided line is both
//   - billed: on an ISSUED invoice, and
//   - handed over: SERVED, or READY for a TAKEAWAY order.
//
// Payment is not part of the rule (pay-later and pay-first both close on the
// last of bill / hand-over). A fully billed order whose food is not yet handed
// over stays BILLED (billing sets that) and is still servable; one billed through
// split invoices stays OPEN until billing marks it BILLED. Either way the last
// hand-over (MarkServed, or kitchen READY for a takeaway) or the last bill closes it.
//
// It locks the order row first so the evaluation sees every change committed by a
// concurrent transaction that also touches the order (they all lock or update the
// row), so "the last of bill / hand-over" closes the order exactly once.
// Call it after the transaction's own item/invoice changes.
func CloseIfDone(ctx context.Context, q *sqlc.Queries, orderID uuid.UUID) (bool, error) {
	o, err := q.LockOrderForUpdate(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, connecterr.Internal(err)
	}
	if o.Status != "OPEN" && o.Status != "BILLED" {
		return false, nil
	}
	st, err := q.GetOrderCloseState(ctx, orderID)
	if err != nil {
		return false, connecterr.Internal(err)
	}
	if st.Live == 0 || st.Done != st.Live {
		return false, nil
	}
	n, err := q.CloseOrder(ctx, orderID)
	if err != nil {
		return false, connecterr.Internal(err)
	}
	return n > 0, nil
}
