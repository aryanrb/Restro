package billing_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/bsdate"
)

// currentFY is today's fiscal year as invoices display it: "2083/84".
func currentFY(t *testing.T) string {
	t.Helper()
	d, err := bsdate.FromAD(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fy := d.FiscalYear() // "2083.084"
	return strings.Split(fy, ".")[0] + "/" + fy[len(fy)-2:]
}

// withSettings sets the seller / billing-mode settings and restores the defaults after the test.
func (f *billFixture) withSettings(sql string, args ...any) {
	f.t.Helper()
	f.exec(sql, args...)
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `UPDATE settings SET name_en = '', name_ne = '', address = '', pan_number = '',
			logo_url = '', billing_mode = 'ESTIMATE' WHERE id = 1`)
	})
}

func (f *billFixture) payAll(ctx context.Context, ocid uuid.UUID, total int64) *restaurantv1.Invoice {
	f.t.Helper()
	inv, err := f.svc.IssueBill(ctx, &restaurantv1.IssueBillRequest{CommandId: uuid.NewString(), OrderClientId: ocid.String(), Payments: []*restaurantv1.PaymentInput{cash(total)}})
	if err != nil {
		f.t.Fatalf("issue: %v", err)
	}
	return inv
}

func TestInvoiceSellerSnapshotFrozenAfterSettingsChange(t *testing.T) {
	f := newBillFixture(t)
	f.openShift(f.owner)
	f.withSettings(`UPDATE settings SET name_en = 'Himalayan Momo', name_ne = 'हिमालयन मम', address = 'Thamel, Kathmandu',
		pan_number = '601234567', logo_url = 'https://x/logo.png', vat_rate_bp = 1300, vat_registered = true WHERE id = 1`)
	ocid, _ := seedOrder(t, f.owner, f.pool, 1)
	issued := f.payAll(f.owner, ocid, 15000)

	want := &restaurantv1.InvoiceSeller{
		Name: &restaurantv1.LocalText{En: "Himalayan Momo", Ne: "हिमालयन मम"}, Address: "Thamel, Kathmandu",
		PanNumber: "601234567", VatRateBp: 1300, LogoUrl: "https://x/logo.png", VatRegistered: true,
	}
	checkSeller := func(where string, got *restaurantv1.InvoiceSeller) {
		t.Helper()
		if got.GetName().GetEn() != want.Name.En || got.GetName().GetNe() != want.Name.Ne || got.GetAddress() != want.Address ||
			got.GetPanNumber() != want.PanNumber || got.GetVatRateBp() != want.VatRateBp || got.GetLogoUrl() != want.LogoUrl ||
			got.GetVatRegistered() != want.VatRegistered {
			t.Fatalf("%s seller = %+v, want %+v", where, got, want)
		}
	}
	checkSeller("issued", issued.GetSeller())

	// The restaurant renames itself, moves and changes VAT: the old bill must not change.
	f.exec(`UPDATE settings SET name_en = 'New Name', name_ne = 'नयाँ', address = 'Patan', pan_number = '999999999',
		logo_url = '', vat_rate_bp = 1500, vat_registered = false WHERE id = 1`)
	got, err := f.svc.GetInvoice(f.owner, &restaurantv1.GetInvoiceRequest{Id: issued.Id})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	checkSeller("reprint", got.GetSeller())
	list, err := f.svc.ListInvoices(f.owner, &restaurantv1.ListInvoicesRequest{Filter: &restaurantv1.ListInvoicesRequest_OrderClientId{OrderClientId: ocid.String()}})
	if err != nil || len(list.Invoices) != 1 {
		t.Fatalf("list: %v", err)
	}
	checkSeller("list", list.Invoices[0].GetSeller())

	// And the DB refuses to rewrite the snapshot.
	if _, err := f.pool.Exec(context.Background(), `UPDATE invoices SET seller_name_en = 'x' WHERE id = $1`, issued.Id); err == nil {
		t.Fatal("seller snapshot was updatable")
	}
}

func TestInvoiceDisplayNumberBothModes(t *testing.T) {
	f := newBillFixture(t)
	f.openShift(f.owner)
	fy := currentFY(t)

	f.withSettings(`UPDATE settings SET billing_mode = 'ESTIMATE' WHERE id = 1`)
	o1, _ := seedOrder(t, f.owner, f.pool, 1)
	o2, _ := seedOrder(t, f.owner, f.pool, 1)
	e1 := f.payAll(f.owner, o1, 15000)
	e2 := f.payAll(f.owner, o2, 15000)
	if e1.DisplayNumber != "E-"+fy+"-1" || e2.DisplayNumber != "E-"+fy+"-2" {
		t.Fatalf("estimate display numbers = %q, %q, want E-%s-1, E-%s-2", e1.DisplayNumber, e2.DisplayNumber, fy, fy)
	}

	f.exec(`UPDATE settings SET billing_mode = 'TAX_INVOICE' WHERE id = 1`)
	o3, _ := seedOrder(t, f.owner, f.pool, 1)
	tax := f.payAll(f.owner, o3, 15000)
	if tax.DisplayNumber != fy+"-1" || tax.Mode != restaurantv1.BillingMode_BILLING_MODE_TAX_INVOICE {
		t.Fatalf("tax display number = %q (mode %v), want %s-1", tax.DisplayNumber, tax.Mode, fy)
	}
	// The stored fiscal year keeps its canonical form.
	if !strings.Contains(tax.FiscalYear, ".") {
		t.Fatalf("fiscal_year = %q, want the stored YYYY.0YY form", tax.FiscalYear)
	}
	got, err := f.svc.GetInvoice(f.owner, &restaurantv1.GetInvoiceRequest{Id: e2.Id})
	if err != nil || got.DisplayNumber != e2.DisplayNumber {
		t.Fatalf("get display number = %q (%v), want %q", got.GetDisplayNumber(), err, e2.DisplayNumber)
	}
}

func TestInvoiceIssueAndCancelFields(t *testing.T) {
	f := newBillFixture(t)
	cashier := f.as(auth.RoleCashier)
	shift := f.openShift(cashier)
	ocid, _ := seedOrder(t, f.owner, f.pool, 2)

	inv, err := f.svc.IssueBill(cashier, &restaurantv1.IssueBillRequest{
		CommandId: uuid.NewString(), OrderClientId: ocid.String(),
		Discount: &restaurantv1.DiscountInput{Type: restaurantv1.DiscountType_DISCOUNT_TYPE_AMOUNT, Value: 1000, Reason: "regular guest"},
		Payments: []*restaurantv1.PaymentInput{cash(29000)},
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	cashierID := auth.ActorFrom(cashier).User.ID.String()
	if inv.DiscountReason != "regular guest" || inv.IssuedByUserId != cashierID || inv.IssuedByName != auth.RoleCashier {
		t.Fatalf("discount_reason=%q issued_by=%q/%q", inv.DiscountReason, inv.IssuedByUserId, inv.IssuedByName)
	}
	if inv.OrderClientId != ocid.String() || inv.Place != "Takeaway" {
		t.Fatalf("order_client_id=%q place=%q", inv.OrderClientId, inv.Place)
	}
	if len(inv.Payments) != 1 || inv.Payments[0].ShiftId != shift.String() {
		t.Fatalf("payment shift = %v, want %s", inv.Payments, shift)
	}
	if inv.CancelReason != "" || inv.CancelledAtUnix != 0 || inv.CancelledByUserId != "" || inv.CancelApprovedByUserId != "" {
		t.Fatalf("issued invoice carries cancel fields: %+v", inv)
	}

	before := time.Now().Add(-time.Minute).Unix()
	cancelled, err := f.svc.CancelInvoice(cashier, &restaurantv1.CancelInvoiceRequest{
		CommandId: uuid.NewString(), InvoiceId: inv.Id, Reason: "wrong table",
		Approval: &restaurantv1.ManagerApproval{ManagerUserId: f.ownerID.String(), Pin: "1111"},
	})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	for where, c := range map[string]*restaurantv1.Invoice{"cancel": cancelled, "get": mustGet(t, f, inv.Id)} {
		if c.Status != restaurantv1.InvoiceStatus_INVOICE_STATUS_CANCELLED || c.CancelReason != "wrong table" ||
			c.CancelledByUserId != cashierID || c.CancelApprovedByUserId != f.ownerID.String() || c.CancelledAtUnix < before {
			t.Fatalf("%s: status=%v reason=%q by=%q approver=%q at=%d", where, c.Status, c.CancelReason, c.CancelledByUserId, c.CancelApprovedByUserId, c.CancelledAtUnix)
		}
		// Issue-time fields survive the cancel.
		if c.DiscountReason != "regular guest" || c.IssuedByName != auth.RoleCashier || c.DisplayNumber != inv.DisplayNumber {
			t.Fatalf("%s: issue fields changed: %+v", where, c)
		}
	}
}

func mustGet(t *testing.T, f *billFixture, id string) *restaurantv1.Invoice {
	t.Helper()
	inv, err := f.svc.GetInvoice(f.owner, &restaurantv1.GetInvoiceRequest{Id: id})
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return inv
}

func TestInvoicePrintLogOriginalAndReprint(t *testing.T) {
	f := newBillFixture(t)
	f.openShift(f.owner)
	ocid, _ := seedOrder(t, f.owner, f.pool, 1)
	inv := f.payAll(f.owner, ocid, 15000)
	if len(inv.Prints) != 0 {
		t.Fatalf("fresh invoice has %d prints", len(inv.Prints))
	}

	first, err := f.svc.MarkPrinted(f.owner, &restaurantv1.MarkPrintedRequest{InvoiceId: inv.Id})
	if err != nil || !first.IsOriginal || first.PrintCount != 1 {
		t.Fatalf("original print: %+v %v", first, err)
	}
	cashier := f.as(auth.RoleCashier)
	second, err := f.svc.MarkPrinted(cashier, &restaurantv1.MarkPrintedRequest{InvoiceId: inv.Id})
	if err != nil || second.IsOriginal || second.CopyNumber != 1 || second.PrintCount != 2 {
		t.Fatalf("reprint: %+v %v", second, err)
	}

	got := mustGet(t, f, inv.Id)
	if got.PrintCount != 2 || len(got.Prints) != 2 {
		t.Fatalf("print_count=%d prints=%d, want 2/2", got.PrintCount, len(got.Prints))
	}
	p0, p1 := got.Prints[0], got.Prints[1]
	if p0.ByUserId != f.ownerID.String() || p0.ByName != "Ram" || p0.AtUnix == 0 {
		t.Fatalf("original print = %+v, want by owner Ram", p0)
	}
	if p1.ByUserId != auth.ActorFrom(cashier).User.ID.String() || p1.ByName != auth.RoleCashier || p1.AtUnix < p0.AtUnix {
		t.Fatalf("reprint = %+v", p1)
	}
	if n := f.count(`SELECT count(*) FROM invoice_prints WHERE invoice_id = $1`, inv.Id); n != 2 {
		t.Fatalf("invoice_prints rows = %d, want 2", n)
	}
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM invoice_prints WHERE invoice_id = $1`, inv.Id); err == nil {
		t.Fatal("print log was deletable")
	}
	// Unknown invoice: nothing logged.
	if _, err := f.svc.MarkPrinted(f.owner, &restaurantv1.MarkPrintedRequest{InvoiceId: uuid.NewString()}); err == nil {
		t.Fatal("printed an unknown invoice")
	}
	if n := f.count(`SELECT count(*) FROM invoice_prints`); n != 2 {
		t.Fatalf("invoice_prints rows = %d after unknown print, want 2", n)
	}
}

func TestInvoiceLinesCarryClientIDsAndBilingualVariant(t *testing.T) {
	f := newBillFixture(t)
	f.openShift(f.owner)
	ocid, items := seedOrder(t, f.owner, f.pool, 2)
	f.exec(`UPDATE order_items SET has_variant = true, variant_name_en = 'Large', variant_name_ne = 'ठूलो' WHERE id = $1`, items[0])
	inv := f.payAll(f.owner, ocid, 30000)

	clientIDs := map[string]string{}
	rows, err := f.pool.Query(context.Background(), `SELECT id::text, client_id::text FROM order_items WHERE id = ANY($1)`, items)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, cid string
		if err := rows.Scan(&id, &cid); err != nil {
			t.Fatal(err)
		}
		clientIDs[id] = cid
	}
	rows.Close()

	if len(inv.Lines) != 2 {
		t.Fatalf("lines = %d", len(inv.Lines))
	}
	seenVariant := false
	for _, l := range inv.Lines {
		if l.OrderItemId == "" || l.OrderItemClientId == "" || clientIDs[l.OrderItemId] != l.OrderItemClientId {
			t.Fatalf("line item ids = %q / %q, want a billed order line and its client id", l.OrderItemId, l.OrderItemClientId)
		}
		if l.OrderItemId == items[0].String() {
			seenVariant = true
			if l.GetVariant().GetEn() != "Large" || l.GetVariant().GetNe() != "ठूलो" || l.VariantName != "Large" {
				t.Fatalf("variant = %+v / %q", l.GetVariant(), l.VariantName)
			}
		} else if l.Variant != nil {
			t.Fatalf("line without a variant has %+v", l.Variant)
		}
	}
	if !seenVariant {
		t.Fatal("variant line not found")
	}
}

func TestInvoicePlace(t *testing.T) {
	f := newBillFixture(t)
	cashier := f.as(auth.RoleCashier)
	f.openShift(cashier)

	// Dine-in: the table's name.
	var tableID uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `WITH a AS (INSERT INTO areas (name_en) VALUES ('Hall') RETURNING id)
		INSERT INTO dining_tables (area_id, name) SELECT id, 'T-7' FROM a RETURNING id`).Scan(&tableID); err != nil {
		t.Fatalf("table: %v", err)
	}
	ocid, _ := seedOrder(t, f.owner, f.pool, 1)
	f.exec(`UPDATE orders SET type = 'DINE_IN', table_id = $2 WHERE client_id = $1`, ocid, tableID)
	if inv := f.payAll(cashier, ocid, 15000); inv.Place != "T-7" {
		t.Fatalf("dine-in place = %q, want T-7", inv.Place)
	}
	// Renaming the table later doesn't touch the issued bill.
	f.exec(`UPDATE dining_tables SET name = 'Garden 1' WHERE id = $1`, tableID)
	list, err := f.svc.ListInvoices(f.owner, &restaurantv1.ListInvoicesRequest{Filter: &restaurantv1.ListInvoicesRequest_OrderClientId{OrderClientId: ocid.String()}})
	if err != nil || list.Invoices[0].Place != "T-7" {
		t.Fatalf("place after rename = %v (%v)", list.GetInvoices(), err)
	}

	// Counter sale: the label, or "Takeaway" when blank.
	labelled, err := f.svc.CounterSale(cashier, &restaurantv1.CounterSaleRequest{CommandId: uuid.NewString(), Label: "Sita", Lines: []*restaurantv1.NewItem{f.line(1)}, Payments: []*restaurantv1.PaymentInput{cash(25000)}})
	if err != nil || labelled.Place != "Sita" || labelled.OrderClientId == "" {
		t.Fatalf("counter sale place = %q ocid=%q (%v)", labelled.GetPlace(), labelled.GetOrderClientId(), err)
	}
	plain, err := f.svc.CounterSale(cashier, &restaurantv1.CounterSaleRequest{CommandId: uuid.NewString(), Lines: []*restaurantv1.NewItem{f.line(1)}, Payments: []*restaurantv1.PaymentInput{cash(25000)}})
	if err != nil || plain.Place != "Takeaway" {
		t.Fatalf("blank-label counter sale place = %q (%v)", plain.GetPlace(), err)
	}
}
