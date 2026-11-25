package alerts

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/loyalty"
)

// Loyalty rules (loyalty design "Fraud controls"), and the business day loyalty
// uses for "one stamp per member per business day" (the alerts' opening-hours day).
func init() {
	loyalty.BusinessDay = func(ctx context.Context, q *sqlc.Queries, at time.Time) (string, error) {
		cfg, err := loadConfig(ctx, q)
		if err != nil {
			return "", err
		}
		return businessDate(at, cfg.OpeningHours), nil
	}
	registry = append(registry, loyaltyRules...)
}

// memberOnBill is the loyalty member on the invoice an invoice.issue row is about.
func memberOnBill(c *ruleCtx) (sqlc.Invoice, sqlc.Customer, bool, error) {
	inv, err := c.q.GetInvoice(c.ctx, c.entityID())
	if err != nil || !inv.LoyaltyCustomerID.Valid {
		return inv, sqlc.Customer{}, false, err
	}
	m, err := c.q.LoyaltyGetCustomer(c.ctx, inv.LoyaltyCustomerID.Bytes)
	if err != nil {
		return inv, m, false, err
	}
	return inv, m, true, nil
}

var loyaltyRules = []ruleDef{
	{
		// A member whose phone is a staff member's (a biller's own number is blocked at billing).
		rule: "LOYALTY_STAFF_NUMBER", triggers: []string{"invoice.issue"},
		eval: func(c *ruleCtx) ([]firing, error) {
			inv, m, ok, err := memberOnBill(c)
			if err != nil || !ok || m.Phone == "" {
				return nil, err
			}
			staff, err := c.q.LoyaltyStaffByPhone(c.ctx, m.Phone)
			if err != nil || len(staff) == 0 {
				return nil, err
			}
			s := staff[0]
			return one(firing{subject: pgID(inv.CreatedByUserID), involved: []uuid.UUID{s.ID}, text: inv.DisplayNumber,
				str: map[string]string{"staff": s.Name, "bill": inv.DisplayNumber}, invoiceID: inv.ID})
		},
	},
	{
		// In 30 days a member earns >= stamps stamps and >= shareBp of them on bills issued by one cashier.
		rule: "LOYALTY_ONE_CASHIER", triggers: []string{"invoice.issue"},
		eval: func(c *ruleCtx) ([]firing, error) {
			inv, m, ok, err := memberOnBill(c)
			if err != nil || !ok {
				return nil, err
			}
			earned, err := c.q.AlertLoyaltyEarnsSince(c.ctx, sqlc.AlertLoyaltyEarnsSinceParams{CustomerID: m.ID, CreatedAt: c.at.Add(-30 * 24 * time.Hour)})
			if err != nil {
				return nil, err
			}
			total, mine := int64(len(earned)), int64(0)
			for _, by := range earned {
				if by == inv.CreatedByUserID {
					mine++
				}
			}
			if total < c.t["stamps"] || mine*10_000 < c.t["shareBp"]*total {
				return nil, nil
			}
			name := loyalty.FirstName(m.Name) + " · " + loyalty.MaskPhone(m.Phone)
			return one(firing{subject: pgID(inv.CreatedByUserID), text: name,
				num: map[string]int64{"stamps": mine, "total": total, "share": (mine*100*2 + total) / (2 * total)},
				str: map[string]string{"member": name}, groupKey: "member:" + m.ID.String()})
		},
	},
	{
		// One cashier redeems >= count rewards in a business day (bills still issued).
		rule: "LOYALTY_MANY_REDEMPTIONS", triggers: []string{loyalty.ActionRedeemed},
		eval: func(c *ruleCtx) ([]firing, error) {
			from, to := businessDayRange(c.day, c.cfg.OpeningHours)
			r, err := c.q.AlertLoyaltyRedemptions(c.ctx, sqlc.AlertLoyaltyRedemptionsParams{CreatedByUserID: c.row.ActorUserID, FromTs: from, ToTs: to})
			if err != nil || int64(r.Count) < c.t["count"] {
				return nil, err
			}
			return one(firing{subject: c.actor(), text: c.userName(c.actor()), num: map[string]int64{"count": int64(r.Count), "total": r.Total},
				amount: amountOf(r.Total), groupKey: "day:" + c.day})
		},
	},
	{
		// A manual stamp adjustment of >= stamps (either way).
		rule: "LOYALTY_STAMP_ADJUSTMENT", triggers: []string{loyalty.ActionAdjusted},
		eval: func(c *ruleCtx) ([]firing, error) {
			n, _ := c.num("stamps")
			if abs(n) < c.t["stamps"] {
				return nil, nil
			}
			subject := c.str("subject")
			return one(firing{subject: c.actor(), approvers: []uuid.UUID{c.approver()}, text: subject,
				num: map[string]int64{"stamps": n}, str: map[string]string{"member": subject, "reason": c.str("reason")}})
		},
	},
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
