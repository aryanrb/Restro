package loyalty_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/alerts"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/billing"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/credit"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/loyalty"
	"github.com/oneaccord/restro/api/internal/shift"
)

// Menu: one item at Rs 300 (VAT-inclusive). Card: 10 stamps, min spend Rs 500,
// reward Rs 300 (the defaults).
const price = 30000

type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	authSvc *auth.Service
	svc     *loyalty.Service
	billing *billing.Service
	credit  *credit.Service
	shifts  *shift.Service
	item    uuid.UUID

	owner, manager, cashier       context.Context
	ownerID, managerID, cashierID uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run loyalty integration tests")
	}
	ctx := context.Background()
	pool, err := db.New(ctx, url)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE loyalty_entries, alerts, repayments, repayment_counters, customers, shift_adjustments, payments, cbms_outbox,
		invoice_prints, invoice_lines, order_item_modifiers, order_items, invoices, invoice_counters, orders, shifts,
		processed_commands, audit_log, sessions, devices, modifier_groups, menu_items, menu_categories, stations, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for _, sql := range []string{
		`UPDATE settings SET vat_registered = true, prices_include_vat = true, vat_rate_bp = 1300,
			service_charge_enabled = false, round_total_to_rupee = false WHERE id = 1`,
		`UPDATE loyalty_settings SET enabled = true, stamps_per_reward = 10, min_spend_paisa = 50000, reward_value_paisa = 30000,
			expiry_months = 12, credit_bills_earn = true, birthday_reward_paisa = 0`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("settings: %v", err)
		}
	}
	t.Cleanup(alerts.Install(zap.NewNop()))
	f := &fixture{t: t, pool: pool, authSvc: auth.NewService(pool, zap.NewNop())}
	f.svc = loyalty.NewService(pool, nil, f.authSvc, zap.NewNop())
	f.billing = billing.NewService(pool, nil, f.authSvc, zap.NewNop())
	f.credit = credit.NewService(pool, nil, zap.NewNop())
	f.shifts = shift.NewService(pool, nil, zap.NewNop())
	f.owner, f.ownerID = f.user("Ram Owner", auth.RoleOwner, "1111", "")
	f.manager, f.managerID = f.user("Sita Gurung", auth.RoleManager, "2222", "9841000002")
	f.cashier, f.cashierID = f.user("Hari Thapa", auth.RoleCashier, "3333", "9841000003")

	q := sqlc.New(pool)
	st, err := q.CreateStation(ctx, sqlc.CreateStationParams{NameEn: "Kitchen"})
	if err != nil {
		t.Fatalf("station: %v", err)
	}
	cat, err := q.CreateCategory(ctx, sqlc.CreateCategoryParams{NameEn: "Food", Active: true})
	if err != nil {
		t.Fatalf("category: %v", err)
	}
	it, err := q.CreateItem(ctx, sqlc.CreateItemParams{CategoryID: cat.ID, NameEn: "Thali", PricePaisa: price, StationID: st.ID, Active: true})
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	f.item = it.ID
	for _, c := range []context.Context{f.cashier, f.manager} {
		if _, err := f.shifts.OpenShift(c, 0, ""); err != nil {
			t.Fatalf("open shift: %v", err)
		}
	}
	return f
}

func (f *fixture) user(name, role, pin, phone string) (context.Context, uuid.UUID) {
	f.t.Helper()
	ctx := context.Background()
	hash, _ := auth.HashSecret("secret123")
	pinHash, _ := auth.HashSecret(pin)
	username := "u-" + uuid.NewString()[:8]
	u, err := sqlc.New(f.pool).CreateUser(ctx, sqlc.CreateUserParams{
		Name: name, Role: role, Username: dbx.Text(username), PasswordHash: dbx.Text(hash), PinHash: dbx.Text(pinHash),
		Language: "en", Active: true, Phone: phone,
	})
	if err != nil {
		f.t.Fatalf("create user: %v", err)
	}
	_, token, err := f.authSvc.PasswordLogin(ctx, username, "secret123")
	if err != nil {
		f.t.Fatalf("login: %v", err)
	}
	req := connect.NewRequest(&restaurantv1.GetSessionRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	var out context.Context
	_, _ = f.authSvc.Interceptor()(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		out = c
		return nil, nil
	})(ctx, req)
	if !auth.ActorFrom(out).LoggedIn() {
		f.t.Fatal("actor not resolved")
	}
	return out, u.ID
}

func (f *fixture) join(phone, name string) *restaurantv1.MemberSummary {
	f.t.Helper()
	m, err := f.svc.JoinLoyalty(f.cashier, &restaurantv1.JoinLoyaltyRequest{Phone: phone, Name: name, Consent: true})
	if err != nil {
		f.t.Fatalf("join: %v", err)
	}
	return m
}

func (f *fixture) card(id string) *restaurantv1.MemberSummary {
	f.t.Helper()
	c, err := f.svc.GetMember(f.owner, &restaurantv1.GetMemberRequest{CustomerId: id})
	if err != nil {
		f.t.Fatalf("member: %v", err)
	}
	return c.Summary
}

type sale struct {
	qty      int32
	customer string
	redeem   bool
	discount int64
	pay      []*restaurantv1.PaymentInput // default: cash for the total
	total    int64                        // with pay nil: the expected total to pay in cash
}

func (f *fixture) sell(ctx context.Context, s sale) (*restaurantv1.Invoice, error) {
	f.t.Helper()
	req := &restaurantv1.CounterSaleRequest{
		CommandId: uuid.NewString(),
		Lines:     []*restaurantv1.NewItem{{ClientId: uuid.NewString(), MenuItemId: f.item.String(), Quantity: s.qty}},
		Payments:  s.pay, CustomerId: s.customer, RedeemReward: s.redeem,
	}
	if s.discount > 0 {
		req.Discount = &restaurantv1.DiscountInput{Type: restaurantv1.DiscountType_DISCOUNT_TYPE_AMOUNT, Value: s.discount, Reason: "regular"}
	}
	if req.Payments == nil {
		total := s.total
		if total == 0 {
			total = int64(s.qty)*price - s.discount
		}
		req.Payments = []*restaurantv1.PaymentInput{{Method: restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH, AmountPaisa: total}}
	}
	return f.billing.CounterSale(ctx, req)
}

func (f *fixture) mustSell(ctx context.Context, s sale) *restaurantv1.Invoice {
	f.t.Helper()
	inv, err := f.sell(ctx, s)
	if err != nil {
		f.t.Fatalf("sale: %v", err)
	}
	return inv
}

func wantReason(t *testing.T, err error, reason restaurantv1.ErrorReason) {
	t.Helper()
	var e *connecterr.Error
	if !errors.As(err, &e) || e.Reason != reason {
		t.Fatalf("want %v, got %v", reason, err)
	}
}

// A bill earns one stamp from the minimum spend, at most once per business day,
// and the invoice keeps the member and the printed card.
func TestEarnMinSpendOncePerDay(t *testing.T) {
	f := newFixture(t)
	m := f.join("9851234567", "Ram Sharma")

	small := f.mustSell(f.cashier, sale{qty: 1, customer: m.CustomerId}) // Rs 300 < Rs 500
	if small.CustomerId != m.CustomerId || small.LoyaltySnapshot.GetStampsAfter() != 0 {
		t.Fatalf("small bill: customer=%q snapshot=%v", small.CustomerId, small.LoyaltySnapshot)
	}
	if got := f.card(m.CustomerId).Stamps; got != 0 {
		t.Fatalf("stamps after a small bill = %d, want 0", got)
	}

	big := f.mustSell(f.cashier, sale{qty: 2, customer: m.CustomerId}) // Rs 600
	snap := big.LoyaltySnapshot
	if snap.GetStampsAfter() != 1 || snap.GetFirstName() != "Ram" || snap.GetMaskedPhone() != "98XXXXX567" || snap.GetPending() || snap.GetValidUntilBs() == "" {
		t.Fatalf("snapshot = %v", snap)
	}
	again := f.mustSell(f.cashier, sale{qty: 3, customer: m.CustomerId})
	if again.LoyaltySnapshot.GetStampsAfter() != 1 {
		t.Fatalf("second qualifying bill the same day: stamps after = %d, want 1", again.LoyaltySnapshot.GetStampsAfter())
	}
	c := f.card(m.CustomerId)
	if c.Stamps != 1 || !c.StampedToday {
		t.Fatalf("card = %v, want 1 stamp, stamped today", c)
	}
	// The reprint (GetInvoice) shows the same card as the original.
	got, err := f.billing.GetInvoice(f.cashier, &restaurantv1.GetInvoiceRequest{Id: big.Id})
	if err != nil || got.LoyaltySnapshot.GetStampsAfter() != 1 {
		t.Fatalf("reprint snapshot = %v (%v)", got.GetLoyaltySnapshot(), err)
	}
}

// The reward is a discount before VAT, apart from the manual discount, outside
// the cashier's 10% limit; used whole, only when ready, never more than the bill.
func TestRedeemIsPreVatDiscount(t *testing.T) {
	f := newFixture(t)
	m := f.join("9851234567", "Ram Sharma")

	_, err := f.sell(f.cashier, sale{qty: 3, customer: m.CustomerId, redeem: true})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_NO_REWARD)

	if _, err := f.svc.AdjustStamps(f.owner, &restaurantv1.AdjustStampsRequest{CustomerId: m.CustomerId, Stamps: 10, Reason: "paper card"}); err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if c := f.card(m.CustomerId); c.RewardsReady != 1 {
		t.Fatalf("rewards ready = %d, want 1", c.RewardsReady)
	}

	// Rs 300 - Rs 10 discount < Rs 300 reward.
	_, err = f.sell(f.cashier, sale{qty: 1, customer: m.CustomerId, redeem: true, discount: 1000, total: 1})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_REWARD_EXCEEDS_BILL)

	// Rs 900, cashier's 10% discount (no PIN) + Rs 300 reward: taxable base Rs 510.
	inv, err := f.sell(f.cashier, sale{qty: 3, customer: m.CustomerId, redeem: true, discount: 9000, total: 51000})
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if inv.SubtotalPaisa != 90000 || inv.DiscountPaisa != 9000 || inv.RewardPaisa != 30000 || inv.TotalPaisa != 51000 {
		t.Fatalf("totals: subtotal=%d discount=%d reward=%d total=%d", inv.SubtotalPaisa, inv.DiscountPaisa, inv.RewardPaisa, inv.TotalPaisa)
	}
	if inv.VatPaisa != 5867 || inv.TaxablePaisa != 51000-5867 { // VAT 13/113 of 51000, half up
		t.Fatalf("vat=%d taxable=%d", inv.VatPaisa, inv.TaxablePaisa)
	}
	c := f.card(m.CustomerId)
	if c.Stamps != 1 || c.RewardsReady != 0 || inv.LoyaltySnapshot.GetStampsAfter() != 1 {
		t.Fatalf("after redeem: card=%v snapshot=%v", c, inv.LoyaltySnapshot)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'LOYALTY_REDEEMED'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("LOYALTY_REDEEMED audit rows = %d (%v)", n, err)
	}
	// One reward per bill: none left.
	_, err = f.sell(f.cashier, sale{qty: 3, customer: m.CustomerId, redeem: true})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_NO_REWARD)
}

// An on-account bill's stamp waits until the bill is fully repaid.
func TestOnAccountStampPendingUntilRepaid(t *testing.T) {
	f := newFixture(t)
	cust, err := f.credit.CreateCustomer(f.cashier, &restaurantv1.CreateCustomerRequest{Customer: &restaurantv1.CustomerInput{Name: "Shyam KC", Phone: "9861111111"}})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	if _, err := f.credit.UpdateCustomer(f.owner, &restaurantv1.UpdateCustomerRequest{Id: cust.Id, Customer: &restaurantv1.CustomerInput{Name: "Shyam KC", Phone: "9861111111"}, CreditLimitPaisa: 500000}); err != nil {
		t.Fatalf("limit: %v", err)
	}
	found, err := f.svc.FindMember(f.cashier, &restaurantv1.FindMemberRequest{Phone: "986-1111111"})
	if err != nil || found.Member != nil || found.CustomerName != "Shyam KC" {
		t.Fatalf("find before join = %v (%v)", found, err)
	}
	m := f.join("9861111111", "")
	if m.CustomerId != cust.Id {
		t.Fatalf("join made a second record: %s vs %s", m.CustomerId, cust.Id)
	}

	inv := f.mustSell(f.cashier, sale{qty: 2, customer: m.CustomerId, pay: []*restaurantv1.PaymentInput{
		{Method: restaurantv1.PaymentMethod_PAYMENT_METHOD_ON_ACCOUNT, AmountPaisa: 2 * price, CustomerId: cust.Id},
	}})
	if !inv.LoyaltySnapshot.GetPending() || inv.LoyaltySnapshot.GetStampsAfter() != 0 {
		t.Fatalf("snapshot = %v, want pending, 0 stamps", inv.LoyaltySnapshot)
	}
	if c := f.card(m.CustomerId); c.Stamps != 0 || c.PendingStamps != 1 {
		t.Fatalf("card = %v, want 0 stamps + 1 pending", c)
	}
	repay := func(amount int64) *restaurantv1.Repayment {
		r, err := f.credit.Repay(f.cashier, &restaurantv1.RepayRequest{RequestId: uuid.NewString(), CustomerId: cust.Id, AmountPaisa: amount,
			Method: restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH})
		if err != nil {
			t.Fatalf("repay: %v", err)
		}
		return r
	}
	repay(price)
	if c := f.card(m.CustomerId); c.Stamps != 0 || c.PendingStamps != 1 {
		t.Fatalf("after half repaid: card = %v, want still pending", c)
	}
	last := repay(price)
	if c := f.card(m.CustomerId); c.Stamps != 1 || c.PendingStamps != 0 {
		t.Fatalf("after fully repaid: card = %v, want 1 stamp", c)
	}
	// Voiding a repayment makes the bill unpaid again: the stamp waits again.
	if _, err := f.credit.VoidRepayment(f.owner, &restaurantv1.VoidRepaymentRequest{Id: last.Id, Reason: "bounced"}); err != nil {
		t.Fatalf("void: %v", err)
	}
	if c := f.card(m.CustomerId); c.Stamps != 0 || c.PendingStamps != 1 {
		t.Fatalf("after void: card = %v, want pending again", c)
	}
}

// Cancelling a bill reverses its stamp and gives back the reward used on it.
func TestCancelReversesStampAndRestoresReward(t *testing.T) {
	f := newFixture(t)
	m := f.join("9851234567", "Ram Sharma")
	if _, err := f.svc.AdjustStamps(f.owner, &restaurantv1.AdjustStampsRequest{CustomerId: m.CustomerId, Stamps: 10, Reason: "paper card"}); err != nil {
		t.Fatalf("adjust: %v", err)
	}
	inv := f.mustSell(f.cashier, sale{qty: 3, customer: m.CustomerId, redeem: true, total: 60000}) // 900 - 300 = 600: earns
	if c := f.card(m.CustomerId); c.Stamps != 1 {
		t.Fatalf("after redeem + earn: stamps = %d, want 1", c.Stamps)
	}
	if _, err := f.billing.CancelInvoice(f.cashier, &restaurantv1.CancelInvoiceRequest{CommandId: uuid.NewString(), InvoiceId: inv.Id, Reason: "wrong table",
		Approval: &restaurantv1.ManagerApproval{ManagerUserId: f.managerID.String(), Pin: "2222"}}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	c := f.card(m.CustomerId)
	if c.Stamps != 10 || c.RewardsReady != 1 || c.StampedToday {
		t.Fatalf("after cancel: card = %v, want 10 stamps, 1 reward, not stamped today", c)
	}
	// A re-bill the same day can earn again.
	again := f.mustSell(f.cashier, sale{qty: 2, customer: m.CustomerId})
	if again.LoyaltySnapshot.GetStampsAfter() != 11 {
		t.Fatalf("re-bill stamps after = %d, want 11", again.LoyaltySnapshot.GetStampsAfter())
	}
}

// A cashier can't put their own number on their own bill; another staff member's
// number is allowed but raises an alert.
func TestOwnNumberBlocked(t *testing.T) {
	f := newFixture(t)
	m := f.join("9841000003", "Hari Thapa") // the cashier's own phone
	_, err := f.sell(f.cashier, sale{qty: 2, customer: m.CustomerId})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_OWN_NUMBER)

	inv := f.mustSell(f.manager, sale{qty: 2, customer: m.CustomerId})
	if inv.LoyaltySnapshot.GetStampsAfter() != 1 {
		t.Fatalf("manager bill stamps after = %d, want 1", inv.LoyaltySnapshot.GetStampsAfter())
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM alerts WHERE rule = 'LOYALTY_STAFF_NUMBER'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("LOYALTY_STAFF_NUMBER alerts = %d (%v)", n, err)
	}
}
