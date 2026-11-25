// Package loyalty is the stamp card (loyalty design): members are Customer
// records keyed by phone, the card is an append-only ledger (loyalty_entries),
// and stamps/rewards are server-computed from it (stamps = sum of DONE entries).
//
// Billing calls Check / Plan / Record inside the issuing transaction (the reward
// is a discount before VAT, the stamp and the invoice commit together) and
// Reverse inside the cancelling one. Credit calls SyncPending after a repayment
// (or its void): an on-account bill's stamp counts only while the bill is fully
// repaid. Cards with no visit for expiry_months expire lazily (EXPIRE entry) when
// they are next read or billed.
package loyalty

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/bsdate"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
)

// Audit actions (the web's vocabulary).
const (
	ActionJoined          = "LOYALTY_JOINED"
	ActionLeft            = "LOYALTY_LEFT"
	ActionRedeemed        = "LOYALTY_REDEEMED"
	ActionAdjusted        = "LOYALTY_ADJUSTED"
	ActionSettingsChanged = "LOYALTY_SETTINGS_CHANGED"
	ActionLinkReset       = "LOYALTY_LINK_RESET"
)

// EventCustomerUpdated is credit's customer.updated: it also fires on card changes.
const EventCustomerUpdated = "customer.updated"

// Ledger kinds and statuses (loyalty_entries).
const (
	KindEarn     = "EARN"
	KindRedeem   = "REDEEM"
	KindReverse  = "REVERSE"
	KindExpire   = "EXPIRE"
	KindAdjust   = "ADJUST"
	KindBirthday = "BIRTHDAY"

	StatusPending = "PENDING"
	StatusDone    = "DONE"
)

var nepal = time.FixedZone("NPT", 5*3600+45*60)

// BusinessDay is the business date (YYYY-MM-DD) an instant belongs to, for "one
// stamp per member per business day". The alerts package replaces it with the
// opening-hours business day (the same day the owner sees in alerts); by itself
// it is the Kathmandu calendar day.
var BusinessDay = func(_ context.Context, _ *sqlc.Queries, at time.Time) (string, error) {
	return at.In(nepal).Format(time.DateOnly), nil
}

// --- errors ---

func reasonErr(reason restaurantv1.ErrorReason, field, msg string) *connecterr.Error {
	e := connecterr.New(connect.CodeFailedPrecondition, reason, msg)
	e.Field = field
	return e
}

func errOff() *connecterr.Error {
	return reasonErr(restaurantv1.ErrorReason_ERROR_REASON_LOYALTY_OFF, "", "loyalty is switched off")
}

func errNotMember() *connecterr.Error {
	return reasonErr(restaurantv1.ErrorReason_ERROR_REASON_NOT_A_MEMBER, "customer_id", "not a loyalty member")
}

func errOwnNumber() *connecterr.Error {
	return reasonErr(restaurantv1.ErrorReason_ERROR_REASON_OWN_NUMBER, "customer_id", "you can't put your own number on your bill")
}

func errNoReward() *connecterr.Error {
	return reasonErr(restaurantv1.ErrorReason_ERROR_REASON_NO_REWARD, "redeem_reward", "no reward is ready on this card")
}

func errRewardExceedsBill() *connecterr.Error {
	return reasonErr(restaurantv1.ErrorReason_ERROR_REASON_REWARD_EXCEEDS_BILL, "redeem_reward", "the bill is too small for the reward")
}

func errLinkInvalid() *connecterr.Error {
	e := connecterr.NotFound("this card link is not valid")
	e.Field = "token"
	return e
}

func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, connecterr.Invalid(field, "invalid id")
	}
	return id, nil
}

// --- formatting ---

// MaskPhone: "9841234567" -> "98XXXXX567".
func MaskPhone(phone string) string {
	if len(phone) < 5 {
		return phone
	}
	return phone[:2] + strings.Repeat("X", len(phone)-5) + phone[len(phone)-3:]
}

// FirstName is the first word of the name, for the card and the receipt.
func FirstName(name string) string {
	f := strings.Fields(name)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func subjectOf(c sqlc.Customer) string { return FirstName(c.Name) + " · " + MaskPhone(c.Phone) }

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func bsString(t time.Time) string {
	d, err := bsdate.FromAD(t)
	if err != nil {
		return ""
	}
	return d.String()
}

// bsMonth is "2083.08" (year and month) of an instant.
func bsMonth(t time.Time) string {
	s := bsString(t)
	if len(s) < 7 {
		return ""
	}
	return s[:7]
}

func dateOf(day string) pgtype.Date {
	t, _ := time.Parse(time.DateOnly, day)
	return pgtype.Date{Time: t, Valid: true}
}

func optInt8(v int64) pgtype.Int8 { return pgtype.Int8{Int64: v, Valid: true} }

// --- settings ---

func settings(ctx context.Context, q *sqlc.Queries) (sqlc.LoyaltySetting, error) {
	st, err := q.GetLoyaltySettings(ctx)
	if err != nil {
		return st, connecterr.Internal(err)
	}
	return st, nil
}

// --- the card ---

// card is a member's computed state at now.
type card struct {
	c     sqlc.Customer
	st    sqlc.LoyaltySetting
	stats sqlc.LoyaltyCardStatsRow
	day   string // today's business date
	now   time.Time
}

func readCard(ctx context.Context, q *sqlc.Queries, c sqlc.Customer, st sqlc.LoyaltySetting, now time.Time) (card, error) {
	day, err := BusinessDay(ctx, q, now)
	if err != nil {
		return card{}, connecterr.Internal(err)
	}
	stats, err := q.LoyaltyCardStats(ctx, sqlc.LoyaltyCardStatsParams{BusinessDate: dateOf(day), CustomerID: c.ID})
	if err != nil {
		return card{}, connecterr.Internal(err)
	}
	return card{c: c, st: st, stats: stats, day: day, now: now}, nil
}

// loadCard reads the card and first expires it when it had no visit for
// expiry_months (the daily job's work, done when the card is next touched).
func loadCard(ctx context.Context, q *sqlc.Queries, c sqlc.Customer, st sqlc.LoyaltySetting, now time.Time) (card, error) {
	k, err := readCard(ctx, q, c, st, now)
	if err != nil {
		return k, err
	}
	if until, ok := k.validUntil(); ok && until.Before(now) && k.stats.Stamps != 0 {
		if _, err := insertEntry(ctx, q, entry{customer: c.ID, kind: KindExpire, stamps: -k.stats.Stamps, status: StatusDone, at: now}); err != nil {
			return k, err
		}
		return readCard(ctx, q, c, st, now)
	}
	return k, nil
}

func (k card) validUntil() (time.Time, bool) {
	last := k.stats.LastVisitAt
	if last.Unix() <= 0 {
		return time.Time{}, false
	}
	return last.AddDate(0, int(k.st.ExpiryMonths), 0), true
}

func (k card) rewardsReady() int32 { return max(0, k.stats.Stamps/k.st.StampsPerReward) }

func (k card) birthdayReady() bool {
	if k.st.BirthdayRewardPaisa <= 0 || !k.c.BirthMonthBs.Valid {
		return false
	}
	month := bsMonth(k.now)
	if len(month) < 7 || month[5:] != twoDigits(int(k.c.BirthMonthBs.Int16)) {
		return false
	}
	used := k.stats.LastBirthdayAt
	return used.Unix() <= 0 || bsMonth(used) != month
}

func twoDigits(n int) string {
	return string([]byte{byte('0' + n/10), byte('0' + n%10)})
}

func (k card) summary() *restaurantv1.MemberSummary {
	out := &restaurantv1.MemberSummary{
		CustomerId: k.c.ID.String(), FirstName: FirstName(k.c.Name), MaskedPhone: MaskPhone(k.c.Phone),
		Stamps: k.stats.Stamps, PendingStamps: k.stats.Pending, RewardsReady: k.rewardsReady(),
		PerReward: k.st.StampsPerReward, RewardValuePaisa: k.st.RewardValuePaisa, MinSpendPaisa: k.st.MinSpendPaisa,
		StampedToday: k.stats.StampedToday, BirthdayReady: k.birthdayReady(), BirthdayRewardPaisa: k.st.BirthdayRewardPaisa,
	}
	if until, ok := k.validUntil(); ok {
		out.ValidUntilBs = bsString(until)
	}
	return out
}

// --- ledger ---

type entry struct {
	customer   uuid.UUID
	kind       string
	stamps     int32
	status     string
	reward     pgtype.Int8
	invoice    uuid.UUID
	by         uuid.UUID
	approvedBy uuid.UUID
	reason     string
	at         time.Time
}

func insertEntry(ctx context.Context, q *sqlc.Queries, e entry) (sqlc.LoyaltyEntry, error) {
	day, err := BusinessDay(ctx, q, e.at)
	if err != nil {
		return sqlc.LoyaltyEntry{}, connecterr.Internal(err)
	}
	row, err := q.InsertLoyaltyEntry(ctx, sqlc.InsertLoyaltyEntryParams{
		CustomerID: e.customer, Kind: e.kind, Stamps: e.stamps, Status: e.status, RewardValuePaisa: e.reward,
		InvoiceID: dbx.UUIDOpt(e.invoice), BusinessDate: dateOf(day), ByUserID: dbx.UUIDOpt(e.by),
		ApprovedByUserID: dbx.UUIDOpt(e.approvedBy), Reason: e.reason,
	})
	if err != nil {
		return row, connecterr.Internal(err)
	}
	return row, nil
}

// SyncPending sets every on-account bill's stamp to PENDING or DONE by whether the
// bill is fully repaid (credit's oldest-first allocation), in q's transaction.
// Credit calls it after a repayment and after voiding one. It returns the members
// whose card changed (for customer.updated).
func SyncPending(ctx context.Context, q *sqlc.Queries) ([]uuid.UUID, error) {
	ids, err := q.SyncLoyaltyPending(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	seen := map[uuid.UUID]bool{}
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// member returns a customer who is a member (NOT_A_MEMBER otherwise), locked when lock.
func member(ctx context.Context, q *sqlc.Queries, id uuid.UUID, lock bool) (sqlc.Customer, error) {
	var (
		c   sqlc.Customer
		err error
	)
	if lock {
		c, err = q.LoyaltyLockCustomer(ctx, id)
	} else {
		c, err = q.LoyaltyGetCustomer(ctx, id)
	}
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !c.LoyaltyMember) {
		return c, errNotMember()
	}
	if err != nil {
		return c, connecterr.Internal(err)
	}
	return c, nil
}

func uuidStr(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return uuid.UUID(id.Bytes).String()
}

func unixOf(t pgtype.Timestamptz) int64 {
	if !t.Valid {
		return 0
	}
	return t.Time.Unix()
}

var kindEnum = map[string]restaurantv1.LoyaltyEntryKind{
	KindEarn: restaurantv1.LoyaltyEntryKind_LOYALTY_ENTRY_KIND_EARN, KindRedeem: restaurantv1.LoyaltyEntryKind_LOYALTY_ENTRY_KIND_REDEEM,
	KindReverse: restaurantv1.LoyaltyEntryKind_LOYALTY_ENTRY_KIND_REVERSE, KindExpire: restaurantv1.LoyaltyEntryKind_LOYALTY_ENTRY_KIND_EXPIRE,
	KindAdjust: restaurantv1.LoyaltyEntryKind_LOYALTY_ENTRY_KIND_ADJUST, KindBirthday: restaurantv1.LoyaltyEntryKind_LOYALTY_ENTRY_KIND_BIRTHDAY,
}

func toEntry(e sqlc.ListLoyaltyEntriesRow) *restaurantv1.LoyaltyEntry {
	status := restaurantv1.LoyaltyEntryStatus_LOYALTY_ENTRY_STATUS_DONE
	if e.Status == StatusPending {
		status = restaurantv1.LoyaltyEntryStatus_LOYALTY_ENTRY_STATUS_PENDING
	}
	return &restaurantv1.LoyaltyEntry{
		Id: e.ID.String(), CustomerId: e.CustomerID.String(), Kind: kindEnum[e.Kind], Stamps: e.Stamps, Status: status,
		RewardValuePaisa: e.RewardValuePaisa.Int64, InvoiceId: uuidStr(e.InvoiceID), InvoiceNumber: e.InvoiceNumber,
		BusinessDate: e.BusinessDate.Time.Format(time.DateOnly), ByUserId: uuidStr(e.ByUserID), ByName: e.ByName,
		ApprovedByUserId: uuidStr(e.ApprovedByUserID), Reason: e.Reason, AtUnix: e.CreatedAt.Unix(),
	}
}

func toSettings(st sqlc.LoyaltySetting) *restaurantv1.LoyaltySettings {
	return &restaurantv1.LoyaltySettings{
		Enabled: st.Enabled, StampsPerReward: st.StampsPerReward, MinSpendPaisa: st.MinSpendPaisa, RewardValuePaisa: st.RewardValuePaisa,
		ExpiryMonths: st.ExpiryMonths, CreditBillsEarn: st.CreditBillsEarn, BirthdayRewardPaisa: st.BirthdayRewardPaisa,
	}
}

// outstanding is what the card is worth now: rewards ready x value + partial
// stamps x (value / N), rounded half up (paisa).
func outstanding(stamps int32, st sqlc.LoyaltySetting) int64 {
	s := int64(max(stamps, 0))
	n, v := int64(st.StampsPerReward), st.RewardValuePaisa
	return s/n*v + ((s%n)*v*2+n)/(2*n)
}
