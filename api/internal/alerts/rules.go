package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/loyalty"
	"github.com/oneaccord/restro/api/internal/shift"
)

// ruleCtx is what a rule sees: the audit row just written and the transaction.
type ruleCtx struct {
	ctx   context.Context
	q     *sqlc.Queries
	row   sqlc.AuditLog
	after map[string]any // the row's "after" JSON
	t     map[string]int64
	cfg   Config
	day   string // business date of the row
	at    time.Time
}

func (c *ruleCtx) actor() uuid.UUID    { return pgID(c.row.ActorUserID) }
func (c *ruleCtx) approver() uuid.UUID { return pgID(c.row.ApproverUserID) }
func (c *ruleCtx) entityID() uuid.UUID {
	id, _ := uuid.Parse(c.row.EntityID.String)
	return id
}

func (c *ruleCtx) num(key string) (int64, bool) {
	v, ok := c.after[key].(float64)
	return int64(v), ok
}

func (c *ruleCtx) str(key string) string {
	s, _ := c.after[key].(string)
	return s
}

func (c *ruleCtx) userName(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	u, err := c.q.GetUserByID(c.ctx, id)
	if err != nil {
		return ""
	}
	return u.Name
}

func pgID(u pgtype.UUID) uuid.UUID {
	if !u.Valid {
		return uuid.Nil
	}
	return u.Bytes
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func minutesBetween(a, b time.Time) int64 { return int64(math.Round(b.Sub(a).Minutes())) }

func noRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// ruleDef: add a rule by adding an entry here and its defaults to ruleMetas.
// Order matters only for which alert is written first when one row fires several.
type ruleDef struct {
	rule     string
	triggers []string // backend audit actions
	eval     func(c *ruleCtx) ([]firing, error)
}

func one(f firing) ([]firing, error) { return []firing{f}, nil }

var registry = []ruleDef{
	{
		// One person's post-send voids in their open shift (or business day for
		// staff without a till).
		rule: "REPEATED_VOIDS", triggers: []string{"order.void"},
		eval: func(c *ruleCtx) ([]firing, error) {
			item, err := c.q.GetOrderItem(c.ctx, c.entityID())
			if err != nil || !item.SentAt.Valid {
				return nil, err
			}
			since := businessDayStart(c.day, c.cfg.OpeningHours)
			if sh, err := c.q.AlertOpenShiftOf(c.ctx, c.actor()); err == nil {
				since = sh.OpenedAt
			} else if !noRows(err) {
				return nil, err
			}
			n, err := c.q.AlertCountSentVoidsSince(c.ctx, sqlc.AlertCountSentVoidsSinceParams{ActorID: c.row.ActorUserID, Since: since})
			if err != nil || int64(n) < c.t["count"] {
				return nil, err
			}
			return one(firing{subject: c.actor(), text: c.userName(c.actor()), num: map[string]int64{"count": int64(n)}})
		},
	},
	{
		// A void needs a manager approval exactly when the kitchen had started
		// (PREPARING / READY / SERVED).
		rule: "VOID_AFTER_KITCHEN", triggers: []string{"order.void"},
		eval: func(c *ruleCtx) ([]firing, error) {
			if c.approver() == uuid.Nil {
				return nil, nil
			}
			item, err := c.q.GetOrderItem(c.ctx, c.entityID())
			if err != nil {
				return nil, err
			}
			value := item.UnitPricePaisa * int64(item.Quantity)
			if value < c.t["minValue"] {
				return nil, nil
			}
			name := itemName(item)
			return one(firing{subject: c.actor(), approvers: []uuid.UUID{c.approver()}, text: name,
				num: map[string]int64{"value": value}, str: map[string]string{"item": name}, amount: amountOf(value)})
		},
	},
	{
		// A manager approving their own void / bill cancel / discount with their own PIN.
		rule: "SELF_APPROVED", triggers: []string{"order.void", "invoice.cancel", "invoice.issue", loyalty.ActionAdjusted},
		eval: func(c *ruleCtx) ([]firing, error) {
			actor, approver, text := c.actor(), c.approver(), ""
			var invoiceID uuid.UUID
			action := c.row.Action
			switch c.row.Action {
			case loyalty.ActionAdjusted:
				// A manager's own stamp adjustment (no one else's PIN) is self-approved.
				if approver == uuid.Nil || approver != actor {
					return nil, nil
				}
				text = c.str("subject")
			case "order.void":
				if approver == uuid.Nil || approver != actor {
					return nil, nil
				}
				item, err := c.q.GetOrderItem(c.ctx, c.entityID())
				if err != nil {
					return nil, err
				}
				text = itemName(item)
			default:
				inv, err := c.q.GetInvoice(c.ctx, c.entityID())
				if err != nil {
					return nil, err
				}
				if c.row.Action == "invoice.issue" {
					if inv.DiscountPaisa == 0 {
						return nil, nil
					}
					actor, approver, action = pgID(inv.CreatedByUserID), pgID(inv.DiscountApprovedByUserID), "discount"
				}
				if approver == uuid.Nil || approver != actor {
					return nil, nil
				}
				text, invoiceID = inv.DisplayNumber, inv.ID
			}
			return one(firing{subject: actor, approvers: []uuid.UUID{approver}, text: text, invoiceID: invoiceID,
				str: map[string]string{"action": action, "subject": text}})
		},
	},
	{
		rule: "HIGH_DISCOUNT", triggers: []string{"invoice.issue"},
		eval: func(c *ruleCtx) ([]firing, error) {
			inv, err := c.q.GetInvoice(c.ctx, c.entityID())
			if err != nil || inv.DiscountPaisa == 0 {
				return nil, err
			}
			var bp int64
			if inv.SubtotalPaisa > 0 {
				bp = (inv.DiscountPaisa*10_000*2 + inv.SubtotalPaisa) / (2 * inv.SubtotalPaisa) // half-up
			}
			if bp < c.t["percentBp"] && inv.DiscountPaisa < c.t["amount"] {
				return nil, nil
			}
			return one(firing{subject: pgID(inv.CreatedByUserID), approvers: []uuid.UUID{pgID(inv.DiscountApprovedByUserID)},
				text: inv.DisplayNumber, num: map[string]int64{"discount": inv.DiscountPaisa, "percent": (bp + 50) / 100},
				str: map[string]string{"bill": inv.DisplayNumber}, amount: amountOf(inv.DiscountPaisa), invoiceID: inv.ID})
		},
	},
	{
		// One person's discounts over the business day.
		rule: "DISCOUNT_TOTAL", triggers: []string{"invoice.issue"},
		eval: func(c *ruleCtx) ([]firing, error) {
			inv, err := c.q.GetInvoice(c.ctx, c.entityID())
			if err != nil || inv.DiscountPaisa == 0 {
				return nil, err
			}
			from, to := businessDayRange(c.day, c.cfg.OpeningHours)
			total, err := c.q.AlertDiscountTotal(c.ctx, sqlc.AlertDiscountTotalParams{UserID: inv.CreatedByUserID, FromTs: from, ToTs: to})
			if err != nil || total < c.t["amount"] {
				return nil, err
			}
			who := pgID(inv.CreatedByUserID)
			return one(firing{subject: who, text: c.userName(who), num: map[string]int64{"total": total},
				amount: amountOf(total), groupKey: "day:" + c.day})
		},
	},
	{
		// A bill cancelled, then some of its items billed again for less within the window.
		rule: "CANCEL_REBILL_LOWER", triggers: []string{"invoice.issue"},
		eval: func(c *ruleCtx) ([]firing, error) {
			inv, err := c.q.GetInvoice(c.ctx, c.entityID())
			if err != nil || !inv.OrderID.Valid {
				return nil, err
			}
			src, err := c.q.AlertRebillSource(c.ctx, sqlc.AlertRebillSourceParams{
				OrderID: inv.OrderID, Since: ts(c.at.Add(-time.Duration(c.t["windowMinutes"]) * time.Minute)),
				Total: inv.TotalPaisa, NewInvoiceID: inv.ID,
			})
			if noRows(err) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return one(firing{subject: pgID(inv.CreatedByUserID), approvers: []uuid.UUID{pgID(src.CancelApprovedByUserID)},
				involved: []uuid.UUID{pgID(src.CancelledByUserID)}, text: src.DisplayNumber + " → " + inv.DisplayNumber,
				num:    map[string]int64{"from": src.TotalPaisa, "to": inv.TotalPaisa},
				str:    map[string]string{"fromBill": src.DisplayNumber, "toBill": inv.DisplayNumber},
				amount: amountOf(src.TotalPaisa - inv.TotalPaisa), invoiceID: inv.ID, dedupKey: "rebill:" + src.ID.String()})
		},
	},
	{
		rule: "LATE_CANCELLATION", triggers: []string{"invoice.cancel"},
		eval: func(c *ruleCtx) ([]firing, error) {
			inv, err := c.q.GetInvoice(c.ctx, c.entityID())
			if err != nil || !inv.CancelledAt.Valid {
				return nil, err
			}
			minutes := minutesBetween(inv.AdDatetime, inv.CancelledAt.Time)
			if minutes <= c.t["minutes"] {
				return nil, nil
			}
			return one(firing{subject: c.actor(), approvers: []uuid.UUID{pgID(inv.CancelApprovedByUserID)}, text: inv.DisplayNumber,
				num: map[string]int64{"minutes": minutes, "total": inv.TotalPaisa}, str: map[string]string{"bill": inv.DisplayNumber},
				amount: amountOf(inv.TotalPaisa), invoiceID: inv.ID})
		},
	},
	{
		// Reprint of an old bill, or the Nth copy (print_count - 1 = copy number).
		rule: "OLD_REPRINT", triggers: []string{"invoice.reprint"},
		eval: func(c *ruleCtx) ([]firing, error) {
			inv, err := c.q.GetInvoice(c.ctx, c.entityID())
			if err != nil {
				return nil, err
			}
			prints, _ := c.num("print_count")
			copyNo := max(prints-1, 0)
			minutes := minutesBetween(inv.AdDatetime, c.at)
			if minutes <= c.t["ageMinutes"] && copyNo < c.t["copy"] {
				return nil, nil
			}
			return one(firing{subject: c.actor(), text: inv.DisplayNumber, num: map[string]int64{"copy": copyNo, "minutes": minutes},
				str: map[string]string{"bill": inv.DisplayNumber}, amount: amountOf(inv.TotalPaisa), invoiceID: inv.ID})
		},
	},
	{
		rule: "CASH_SHORT", triggers: []string{"shift.close"},
		eval: func(c *ruleCtx) ([]firing, error) {
			s, err := closedShift(c, c.entityID())
			if err != nil || s.diff > -c.t["amount"] {
				return nil, err
			}
			return one(s.firing(c, map[string]int64{"short": -s.diff}, -s.diff))
		},
	},
	{
		rule: "CASH_OVER", triggers: []string{"shift.close"},
		eval: func(c *ruleCtx) ([]firing, error) {
			s, err := closedShift(c, c.entityID())
			if err != nil || s.diff < c.t["amount"] {
				return nil, err
			}
			return one(s.firing(c, map[string]int64{"over": s.diff}, s.diff))
		},
	},
	{
		// The cashier's shifts closed short in the last 7 days.
		rule: "REPEATED_SHORTAGES", triggers: []string{"shift.close"},
		eval: func(c *ruleCtx) ([]firing, error) {
			cur, err := closedShift(c, c.entityID())
			if err != nil || cur.diff > -c.t["minShort"] {
				return nil, err
			}
			shifts, err := c.q.AlertClosedShiftsSince(c.ctx, sqlc.AlertClosedShiftsSinceParams{
				CashierUserID: cur.sh.CashierUserID, Since: ts(c.at.Add(-7 * 24 * time.Hour)),
			})
			if err != nil {
				return nil, err
			}
			var n int64
			for _, sh := range shifts {
				s, err := shiftDiff(c, sh)
				if err != nil {
					return nil, err
				}
				if s.diff <= -c.t["minShort"] {
					n++
				}
			}
			if n < c.t["count"] {
				return nil, nil
			}
			return one(firing{subject: cur.sh.CashierUserID, text: c.userName(cur.sh.CashierUserID), num: map[string]int64{"count": n},
				shiftID: cur.sh.ID, groupKey: "week"})
		},
	},
	{
		// The till closes for the day (last open shift) while served food is on no bill.
		rule: "SERVED_NOT_BILLED", triggers: []string{"shift.close"},
		eval: func(c *ruleCtx) ([]firing, error) {
			open, err := c.q.AlertCountOpenShifts(c.ctx)
			if err != nil || open > 0 {
				return nil, err
			}
			rows, err := c.q.AlertServedUnbilled(c.ctx)
			if err != nil {
				return nil, err
			}
			out := make([]firing, 0, len(rows))
			for _, r := range rows {
				key := "order:" + r.ID.String()
				out = append(out, firing{subject: r.WaiterUserID, text: r.Place, num: map[string]int64{"value": r.Value, "items": int64(r.Items)},
					str: map[string]string{"place": r.Place}, amount: amountOf(r.Value), groupKey: key, dedupKey: key})
			}
			return out, nil
		},
	},
	{
		rule: "AFTER_HOURS_LOGIN", triggers: []string{"login"},
		eval: func(c *ruleCtx) ([]firing, error) {
			if c.cfg.OpeningHours == nil || withinOpeningHours(c.at, c.cfg.OpeningHours, c.cfg.Grace) {
				return nil, nil
			}
			return one(firing{subject: c.actor(), text: c.userName(c.actor()), str: map[string]string{"time": fmtHHMM(c.at)},
				groupKey: "day:" + c.day})
		},
	},
	{
		// Whoever was trying the manager's PIN (none for a PIN-login lockout).
		rule: "MANAGER_PIN_LOCKED", triggers: []string{"manager_pin.locked"},
		eval: func(c *ruleCtx) ([]firing, error) {
			name := c.userName(c.entityID())
			return one(firing{subject: c.actor(), involved: []uuid.UUID{c.entityID()}, text: name, str: map[string]string{"manager": name}})
		},
	},
	{
		// A price lowered, food sold at the low price, then raised back the same business day.
		rule: "PRICE_DIP", triggers: []string{"menu.item.save"},
		eval: priceDip,
	},
	{
		// credit.give carries an approver only when the bill went over the available credit.
		rule: "CREDIT_OVER_LIMIT", triggers: []string{"credit.give"},
		eval: func(c *ruleCtx) ([]firing, error) {
			if c.approver() == uuid.Nil {
				return nil, nil
			}
			f, err := creditFiring(c)
			if err != nil {
				return nil, err
			}
			f.approvers = []uuid.UUID{c.approver()}
			f.text = f.str["bill"]
			return one(f)
		},
	},
	{
		// Credit on an account the same person opened this business day.
		rule: "CREDIT_NEW_ACCOUNT", triggers: []string{"credit.give"},
		eval: func(c *ruleCtx) ([]firing, error) {
			cust, err := c.q.AlertCustomer(c.ctx, c.entityID())
			if err != nil {
				return nil, err
			}
			if pgID(cust.CreatedByUserID) != c.actor() || businessDate(cust.CreatedAt, c.cfg.OpeningHours) != c.day {
				return nil, nil
			}
			f, err := creditFiring(c)
			if err != nil {
				return nil, err
			}
			f.approvers = []uuid.UUID{c.approver()}
			f.dedupKey = "new-account:" + cust.ID.String()
			return one(f)
		},
	},
	{
		// Supplier cash from the till: one payment above the till limit, or one
		// person's till paid-outs in a shift reaching shiftTotal (one alert per shift, updated).
		rule: "LARGE_PAID_OUT", triggers: []string{"SUPPLIER_PAID"},
		eval: func(c *ruleCtx) ([]firing, error) {
			p, err := c.q.GetSupplierPaymentRow(c.ctx, c.entityID())
			if err != nil || p.Method != "TILL" || !p.ShiftID.Valid {
				return nil, err
			}
			shiftID := uuid.UUID(p.ShiftID.Bytes)
			var out []firing
			if p.AmountPaisa > c.t["amount"] {
				key := "payout:" + p.ID.String()
				out = append(out, firing{subject: p.PaidByUserID, approvers: []uuid.UUID{c.approver()}, text: "SP-" + itoa(int64(p.Number)),
					num: map[string]int64{"paid": p.AmountPaisa}, str: map[string]string{"variant": "single", "supplier": p.SupplierName},
					amount: amountOf(p.AmountPaisa), shiftID: shiftID, groupKey: key, dedupKey: key})
			}
			sum, err := c.q.AlertTillPaidOutsInShift(c.ctx, sqlc.AlertTillPaidOutsInShiftParams{ShiftID: shiftID, PaidByUserID: p.PaidByUserID})
			if err != nil {
				return nil, err
			}
			if sum.Total >= c.t["shiftTotal"] {
				out = append(out, firing{subject: p.PaidByUserID, text: p.PaidByName,
					num: map[string]int64{"total": sum.Total, "payments": int64(sum.Payments)}, str: map[string]string{"variant": "shift"},
					amount: amountOf(sum.Total), shiftID: shiftID, groupKey: "shift:" + shiftID.String()})
			}
			return out, nil
		},
	},
	{
		// The restaurant's waste over the business day (one alert, updated), or one posted count's loss.
		rule: "HIGH_WASTE", triggers: []string{"WASTE_LOGGED", "STOCK_COUNT_POSTED"},
		eval: func(c *ruleCtx) ([]firing, error) {
			if c.row.Action == "WASTE_LOGGED" {
				from, to := businessDayRange(c.day, c.cfg.OpeningHours)
				total, err := c.q.AlertWasteTotal(c.ctx, sqlc.AlertWasteTotalParams{FromTs: from, ToTs: to})
				if err != nil || total < c.t["dayWaste"] {
					return nil, err
				}
				name, _, _ := strings.Cut(c.row.Reason.String, " · ")
				return one(firing{text: name, num: map[string]int64{"waste": total}, str: map[string]string{"variant": "day"},
					amount: amountOf(total), groupKey: "day:" + c.day})
			}
			// Losses only: items found over don't hide the ones that went missing.
			loss, err := c.q.AlertCountLoss(c.ctx, c.entityID())
			if err != nil || loss < c.t["countLoss"] {
				return nil, err
			}
			number, _ := c.num("number")
			key := "count:" + c.entityID().String()
			return one(firing{involved: []uuid.UUID{c.actor()}, text: "Count #" + itoa(number),
				num: map[string]int64{"loss": loss, "number": number}, str: map[string]string{"variant": "count"},
				amount: amountOf(loss), groupKey: key, dedupKey: key})
		},
	},
}

func itemName(it sqlc.OrderItem) string {
	if it.VariantNameEn != "" {
		return it.NameEn + " (" + it.VariantNameEn + ")"
	}
	return it.NameEn
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

type shiftResult struct {
	sh                      sqlc.Shift
	expected, counted, diff int64
}

func shiftDiff(c *ruleCtx, sh sqlc.Shift) (shiftResult, error) {
	expected, err := shift.ExpectedCash(c.ctx, c.q, sh)
	if err != nil {
		return shiftResult{}, err
	}
	counted := sh.CountedCashPaisa.Int64
	return shiftResult{sh: sh, expected: expected, counted: counted, diff: counted - expected}, nil
}

func closedShift(c *ruleCtx, id uuid.UUID) (shiftResult, error) {
	sh, err := c.q.GetShift(c.ctx, id)
	if err != nil {
		return shiftResult{}, err
	}
	return shiftDiff(c, sh)
}

// firing for a closed shift: about the shift's cashier (whoever closed it is involved).
func (s shiftResult) firing(c *ruleCtx, num map[string]int64, amount int64) firing {
	num["expected"], num["counted"] = s.expected, s.counted
	key := "shift:" + s.sh.ID.String()
	return firing{subject: s.sh.CashierUserID, involved: []uuid.UUID{c.actor()}, text: c.userName(s.sh.CashierUserID), num: num,
		amount: amountOf(amount), shiftID: s.sh.ID, groupKey: key, dedupKey: key}
}

func creditFiring(c *ruleCtx) (firing, error) {
	cust, err := c.q.AlertCustomer(c.ctx, c.entityID())
	if err != nil {
		return firing{}, err
	}
	value, _ := c.num("amount")
	invoiceID, _ := uuid.Parse(c.str("invoice_id"))
	bill := c.str("detail")
	return firing{subject: c.actor(), text: cust.Name, num: map[string]int64{"value": value},
		str: map[string]string{"customer": cust.Name, "bill": bill}, amount: amountOf(value), invoiceID: invoiceID}, nil
}

// priceDip reads the item's price history from its menu.item.save audit rows
// (their "after" is the saved item, with price_paisa).
func priceDip(c *ruleCtx) ([]firing, error) {
	rows, err := c.q.AlertEntityAudit(c.ctx, sqlc.AlertEntityAuditParams{Action: c.row.Action, EntityID: c.row.EntityID.String, UpToID: c.row.ID})
	if err != nil || len(rows) < 3 {
		return nil, err
	}
	type saved struct {
		Name struct {
			En string `json:"en"`
		} `json:"name"`
		Price int64 `json:"price_paisa"`
	}
	prices := make([]saved, len(rows))
	for i, r := range rows {
		_ = json.Unmarshal(r.After, &prices[i])
	}
	last := len(rows) - 1
	if prices[last].Price <= prices[last-1].Price {
		return nil, nil
	}
	lowering := -1
	for i := last - 1; i >= 1; i-- {
		if prices[i].Price < prices[i-1].Price && businessDate(rows[i].CreatedAt, c.cfg.OpeningHours) == c.day {
			lowering = i
			break
		}
	}
	if lowering < 0 {
		return nil, nil
	}
	from, low := prices[lowering-1].Price, prices[lowering].Price
	if prices[last].Price < from {
		return nil, nil
	}
	sold, err := c.q.AlertSoldBetween(c.ctx, sqlc.AlertSoldBetweenParams{
		MenuItemID: c.entityID(), FromTs: ts(rows[lowering].CreatedAt), ToTs: ts(c.at),
	})
	if err != nil || sold == 0 {
		return nil, err
	}
	name := prices[last].Name.En
	return one(firing{subject: c.actor(), involved: []uuid.UUID{pgID(rows[lowering].ActorUserID)}, text: name,
		num: map[string]int64{"from": from, "low": low, "sold": int64(sold)}, str: map[string]string{"item": name},
		amount: amountOf((from - low) * int64(sold)), dedupKey: "dip:" + itoa(rows[lowering].ID)})
}
