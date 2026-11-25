package credit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
)

// OnAccount is one customer's checked share of a bill's on-account payments.
type OnAccount struct {
	Customer      sqlc.Customer
	Amount        int64     // this bill's total on account for the customer
	BalanceBefore int64     // the balance before this bill
	ApproverID    uuid.UUID // who allowed going over the limit (uuid.Nil = within it)
}

// BalanceAfter is the customer's balance once this bill is issued.
func (o OnAccount) BalanceAfter() int64 { return o.BalanceBefore + o.Amount }

// VerifyApproval checks a manager PIN approval (auth.Service.VerifyManagerApproval).
type VerifyApproval func(ctx context.Context, a auth.ManagerApproval) (sqlc.User, error)

// CheckOnAccount runs inside the billing transaction, before the invoice is
// numbered: it locks each customer's account (so two tills can't both use the
// last of a limit), requires it to be active, and checks the amount against the
// available credit (limit - balance). Going over needs credit_approval from a
// cashier; an owner or manager billing is their own approval. amounts is the
// on-account total per customer.
func CheckOnAccount(ctx context.Context, q *sqlc.Queries, actor *auth.Actor, amounts map[uuid.UUID]int64,
	approval *restaurantv1.ManagerApproval, verify VerifyApproval) ([]OnAccount, error) {
	ids := make([]uuid.UUID, 0, len(amounts))
	for id := range amounts {
		ids = append(ids, id)
	}
	// A fixed lock order, so bills for the same customers can't deadlock.
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
	out := make([]OnAccount, 0, len(ids))
	for _, id := range ids {
		c, err := q.LockCustomer(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connecterr.Invalid("customer_id", "customer not found")
		}
		if err != nil {
			return nil, connecterr.Internal(err)
		}
		if !c.Active {
			return nil, customerInactive()
		}
		// A loyalty-only member has no credit account: one must be opened first.
		if !c.CreditAccount {
			return nil, connecterr.Invalid("customer_id", "this customer has no credit account")
		}
		balance, err := q.CustomerBalance(ctx, id)
		if err != nil {
			return nil, connecterr.Internal(err)
		}
		oa := OnAccount{Customer: c, Amount: amounts[id], BalanceBefore: balance}
		if available := c.CreditLimitPaisa - balance; oa.Amount > available {
			switch {
			case isManager(actor):
				oa.ApproverID = actor.User.ID
			case approval == nil || approval.GetManagerUserId() == "":
				return nil, CreditLimitExceeded(fmt.Sprintf("%s has %d paisa of credit left; a manager PIN is required", c.Name, max(available, 0)))
			default:
				mgr, err := verify(ctx, auth.ManagerApproval{ManagerUserID: approval.GetManagerUserId(), Pin: approval.GetPin()})
				if err != nil {
					// Tell the till it was the credit approval (not a discount approval) that failed.
					var ce *connecterr.Error
					if errors.As(err, &ce) {
						cp := *ce
						cp.Field = "credit_approval"
						return nil, &cp
					}
					return nil, err
				}
				oa.ApproverID = mgr.ID
			}
		}
		out = append(out, oa)
	}
	return out, nil
}

// BusinessBuyer returns the name and PAN of the first business account (one with
// a PAN) among the checks, for a bill with no buyer details of its own.
func BusinessBuyer(checks []OnAccount) (name, pan string, ok bool) {
	for _, c := range checks {
		if c.Customer.Pan != "" {
			return c.Customer.Name, c.Customer.Pan, true
		}
	}
	return "", "", false
}

// RecordOnAccount writes the bill's on-account payment rows (with the receipt
// snapshot: customer name and balance after) and a credit.give audit row per
// customer, in the billing transaction. tenders are (customer, amount, reference)
// in the bill's payment order.
func RecordOnAccount(ctx context.Context, q *sqlc.Queries, actor *auth.Actor, invoiceID, shiftID uuid.UUID, displayNumber string,
	checks []OnAccount, tenders []Tender) error {
	byID := make(map[uuid.UUID]OnAccount, len(checks))
	for _, c := range checks {
		byID[c.Customer.ID] = c
	}
	for _, t := range tenders {
		c := byID[t.CustomerID]
		if _, err := q.CreateOnAccountPayment(ctx, sqlc.CreateOnAccountPaymentParams{
			InvoiceID: invoiceID, AmountPaisa: t.Amount, Reference: t.Reference, ConfirmedByUserID: actor.User.ID, ShiftID: shiftID,
			CustomerID: dbx.UUID(t.CustomerID), CustomerName: c.Customer.Name, BalanceAfterPaisa: c.BalanceAfter(),
			CreditApprovedByUserID: dbx.UUIDOpt(c.ApproverID),
		}); err != nil {
			return connecterr.Internal(err)
		}
	}
	for _, c := range checks {
		if err := audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: ActionCreditGiven, Entity: "customer", EntityID: c.Customer.ID.String(), ApproverID: c.ApproverID,
			After: audit.JSON(map[string]any{
				"invoice_id": invoiceID.String(), "amount": c.Amount, "balance_after": c.BalanceAfter(),
				"detail": displayNumber,
			}),
		}); err != nil {
			return err
		}
	}
	return nil
}

// Tender is one on-account payment of a bill.
type Tender struct {
	CustomerID uuid.UUID
	Amount     int64
	Reference  string
}
