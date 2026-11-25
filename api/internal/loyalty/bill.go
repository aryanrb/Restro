package loyalty

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
)

// Request is a bill's loyalty input (BillRequest customer_id / redeem_*).
type Request struct {
	CustomerID     string
	RedeemReward   bool
	RedeemBirthday bool
}

// Snapshot is the card as printed on a bill, stored on the invoice (immutable)
// so reprints match the original.
type Snapshot struct {
	MaskedPhone  string `json:"masked_phone"`
	FirstName    string `json:"first_name"`
	StampsAfter  int32  `json:"stamps_after"`
	Pending      bool   `json:"pending"`
	PerReward    int32  `json:"per_reward"`
	RewardValue  int64  `json:"reward_value_paisa"`
	ValidUntilBs string `json:"valid_until_bs"`
	CardToken    string `json:"card_token"`
}

// SnapshotProto decodes an invoice's stored snapshot (nil when there is none).
func SnapshotProto(raw []byte) *restaurantv1.LoyaltySnapshot {
	if len(raw) == 0 {
		return nil
	}
	var s Snapshot
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return &restaurantv1.LoyaltySnapshot{
		MaskedPhone: s.MaskedPhone, FirstName: s.FirstName, StampsAfter: s.StampsAfter, Pending: s.Pending,
		PerReward: s.PerReward, RewardValuePaisa: s.RewardValue, ValidUntilBs: s.ValidUntilBs, CardToken: s.CardToken,
	}
}

// Bill is the member and reward on a bill, checked in the billing transaction
// (Check), then what it earns (Plan), then recorded with the invoice (Record).
type Bill struct {
	Customer sqlc.Customer
	Reward   int64  // paisa: a discount before VAT
	Kind     string // "", KindRedeem or KindBirthday
	card     card

	Earn     bool
	Pending  bool
	Snapshot Snapshot
}

// CustomerID is the member's id (uuid.Nil for a nil bill).
func (b *Bill) CustomerID() uuid.UUID {
	if b == nil {
		return uuid.Nil
	}
	return b.Customer.ID
}

// SnapshotJSON is the snapshot to store on the invoice (nil for a nil bill).
func (b *Bill) SnapshotJSON() []byte {
	if b == nil {
		return nil
	}
	raw, _ := json.Marshal(b.Snapshot)
	return raw
}

// Check validates a bill's member and reward for a bill whose subtotal −
// discount is base. It returns nil when no member is attached. With lock the
// member's row is locked FOR UPDATE, so two tills can't both spend the last
// reward: the reward check and the invoice number commit together. The reward
// is used whole or not at all, one per bill, never on the biller's own number.
func Check(ctx context.Context, q *sqlc.Queries, actor *auth.Actor, req Request, base int64, lock bool) (*Bill, error) {
	if req.CustomerID == "" {
		if req.RedeemReward || req.RedeemBirthday {
			return nil, errNotMember()
		}
		return nil, nil
	}
	st, err := settings(ctx, q)
	if err != nil {
		return nil, err
	}
	if !st.Enabled {
		return nil, errOff()
	}
	if req.RedeemReward && req.RedeemBirthday {
		return nil, connecterr.Invalid("redeem_reward", "one reward per bill")
	}
	id, err := parseID("customer_id", req.CustomerID)
	if err != nil {
		return nil, err
	}
	c, err := member(ctx, q, id, lock)
	if err != nil {
		return nil, err
	}
	if actor != nil && actor.User.Phone != "" && c.Phone == actor.User.Phone {
		return nil, errOwnNumber()
	}
	k, err := loadCard(ctx, q, c, st, time.Now())
	if err != nil {
		return nil, err
	}
	b := &Bill{Customer: c, card: k}
	switch {
	case req.RedeemReward:
		if k.rewardsReady() < 1 {
			return nil, errNoReward()
		}
		b.Reward, b.Kind = st.RewardValuePaisa, KindRedeem
	case req.RedeemBirthday:
		if !k.birthdayReady() {
			return nil, errNoReward()
		}
		b.Reward, b.Kind = st.BirthdayRewardPaisa, KindBirthday
	}
	if b.Reward > base {
		return nil, errRewardExceedsBill()
	}
	return b, nil
}

// Plan decides whether the bill earns a stamp and builds its snapshot. own is
// this bill's subtotal − discount − reward (menu prices, no service charge);
// today's other bills of the same order for the member count with it, so a split
// bill earns once across its parts. An on-account bill's stamp is pending.
func (b *Bill) Plan(ctx context.Context, q *sqlc.Queries, orderID uuid.UUID, own int64, onAccount bool) error {
	if b == nil {
		return nil
	}
	k := b.card
	st := k.st
	if (!onAccount || st.CreditBillsEarn) && !k.stats.StampedToday {
		total := own
		if orderID != uuid.Nil {
			rows, err := q.LoyaltyOrderSiblings(ctx, sqlc.LoyaltyOrderSiblingsParams{OrderID: dbx.UUID(orderID), LoyaltyCustomerID: dbx.UUID(b.Customer.ID)})
			if err != nil {
				return connecterr.Internal(err)
			}
			for _, r := range rows {
				day, err := BusinessDay(ctx, q, r.AdDatetime)
				if err != nil {
					return connecterr.Internal(err)
				}
				if day == k.day {
					total += r.Amount
				}
			}
		}
		b.Earn = total >= st.MinSpendPaisa
	}
	b.Pending = b.Earn && onAccount
	after := k.stats.Stamps
	if b.Kind == KindRedeem {
		after -= st.StampsPerReward
	}
	if b.Earn && !b.Pending {
		after++
	}
	b.Snapshot = Snapshot{
		MaskedPhone: MaskPhone(b.Customer.Phone), FirstName: FirstName(b.Customer.Name), StampsAfter: after, Pending: b.Pending,
		PerReward: st.StampsPerReward, RewardValue: st.RewardValuePaisa,
		ValidUntilBs: bsString(k.now.AddDate(0, int(st.ExpiryMonths), 0)), // this bill is a visit
		CardToken:    b.Customer.CardToken.String,
	}
	return nil
}

// PreviewStampsAfter is where the card ends up (as the preview shows it: a stamp
// earned counts even if the bill will go on account).
func (b *Bill) PreviewStampsAfter() int32 {
	after := b.card.stats.Stamps
	if b.Kind == KindRedeem {
		after -= b.card.st.StampsPerReward
	}
	if b.Earn {
		after++
	}
	return after
}

// Record writes the bill's ledger entries (reward used, stamp earned) and the
// LOYALTY_REDEEMED audit row, in the issuing transaction, before the bill's
// invoice.issue audit row (whose alert rules read them).
func Record(ctx context.Context, q *sqlc.Queries, actor *auth.Actor, invoiceID uuid.UUID, displayNumber string, b *Bill) error {
	if b == nil {
		return nil
	}
	st := b.card.st
	now := time.Now()
	base := entry{customer: b.Customer.ID, invoice: invoiceID, by: actor.User.ID, at: now}
	switch b.Kind {
	case KindRedeem:
		e := base
		e.kind, e.stamps, e.status, e.reward = KindRedeem, -st.StampsPerReward, StatusDone, optInt8(b.Reward)
		if _, err := insertEntry(ctx, q, e); err != nil {
			return err
		}
	case KindBirthday:
		e := base
		e.kind, e.stamps, e.status, e.reward = KindBirthday, 0, StatusDone, optInt8(b.Reward)
		if _, err := insertEntry(ctx, q, e); err != nil {
			return err
		}
	}
	if b.Earn {
		e := base
		e.kind, e.stamps, e.status = KindEarn, 1, StatusDone
		if b.Pending {
			e.status = StatusPending
		}
		if _, err := insertEntry(ctx, q, e); err != nil {
			return err
		}
	}
	if b.Kind != "" {
		detail := displayNumber
		if b.Kind == KindBirthday {
			detail += " · birthday"
		}
		if err := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionRedeemed, Entity: "customer", EntityID: b.Customer.ID.String(),
			After: audit.JSON(map[string]any{
				"subject": subjectOf(b.Customer), "invoice_id": invoiceID.String(), "bill": displayNumber, "value": b.Reward,
				"kind": b.Kind, "detail": detail,
			})}); err != nil {
			return err
		}
	}
	return nil
}

// Reverse runs in the cancelling transaction: every entry the bill made is
// reversed (its stamp taken back, the reward used on it restored), by the
// canceller, approved by the cancel's approver. A stamp already spent can leave
// the card at −1. It returns the member whose card changed (uuid.Nil = none).
func Reverse(ctx context.Context, q *sqlc.Queries, actor *auth.Actor, invoiceID, approverID uuid.UUID, reason string) (uuid.UUID, error) {
	own, err := q.ListInvoiceLoyaltyEntries(ctx, dbx.UUID(invoiceID))
	if err != nil {
		return uuid.Nil, connecterr.Internal(err)
	}
	now := time.Now()
	var who uuid.UUID
	for _, e := range own {
		if _, err := insertEntry(ctx, q, entry{
			customer: e.CustomerID, kind: KindReverse, stamps: -e.Stamps, status: e.Status, reward: e.RewardValuePaisa,
			invoice: invoiceID, by: actor.User.ID, approvedBy: approverID, reason: reason, at: now,
		}); err != nil {
			return uuid.Nil, err
		}
		who = e.CustomerID
	}
	return who, nil
}

// RewardPaisa is the reward used on the bill (0 for a nil bill).
func (b *Bill) RewardPaisa() int64 {
	if b == nil {
		return 0
	}
	return b.Reward
}
