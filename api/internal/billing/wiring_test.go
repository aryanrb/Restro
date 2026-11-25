package billing_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
)

func (f *billFixture) cancelReq(invID string) *restaurantv1.CancelInvoiceRequest {
	return &restaurantv1.CancelInvoiceRequest{
		CommandId: uuid.NewString(), InvoiceId: invID, Reason: "wrong table",
		Approval: &restaurantv1.ManagerApproval{ManagerUserId: f.ownerID.String(), Pin: "1111"},
	}
}

// Cancelling a bill inside the shift that took its payment needs no adjustment.
func TestCancelInvoiceInOwnOpenShiftNoAdjustment(t *testing.T) {
	f := newBillFixture(t)
	cashier := f.as(auth.RoleCashier)
	f.openShift(cashier)
	ocid, _ := seedOrder(t, f.owner, f.pool, 1)
	inv := f.payAll(cashier, ocid, 15000)

	if _, err := f.svc.CancelInvoice(cashier, f.cancelReq(inv.Id)); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM shift_adjustments WHERE invoice_id = $1`, inv.Id); n != 0 {
		t.Fatalf("adjustments = %d, want 0", n)
	}
}

// A bill whose shift has closed: cancelling it with no open shift is refused and
// changes nothing; with a shift open the adjustment lands there, in the same tx.
func TestCancelInvoiceFromClosedShiftRecordsAdjustment(t *testing.T) {
	f := newBillFixture(t)
	cashier := f.as(auth.RoleCashier)
	first := f.openShift(cashier)
	ocid, _ := seedOrder(t, f.owner, f.pool, 1)
	inv := f.payAll(cashier, ocid, 15000)
	f.exec(`UPDATE shifts SET status = 'CLOSED', closed_at = now() - interval '1 second', counted_cash_paisa = 15000 WHERE id = $1`, first)

	// No open shift: refused, nothing written.
	_, err := f.svc.CancelInvoice(cashier, f.cancelReq(inv.Id))
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_SHIFT_NOT_OPEN, "")
	if got := mustGet(t, f, inv.Id); got.Status != restaurantv1.InvoiceStatus_INVOICE_STATUS_ISSUED {
		t.Fatalf("status after refused cancel = %v", got.Status)
	}
	if n := f.count(`SELECT count(*) FROM audit_log WHERE action = 'invoice.cancel'`); n != 0 {
		t.Fatalf("cancel audit rows = %d, want 0", n)
	}
	if n := f.count(`SELECT count(*) FROM order_items WHERE invoice_id = $1`, inv.Id); n != 1 {
		t.Fatalf("items still billed = %d, want 1", n)
	}
	if n := f.count(`SELECT count(*) FROM cbms_outbox WHERE invoice_id = $1 AND kind = 'RETURN'`, inv.Id); n != 0 {
		t.Fatalf("return outbox rows = %d, want 0", n)
	}

	// With a shift open, the cancel goes through and is counted back there.
	second := f.openShift(cashier)
	if _, err := f.svc.CancelInvoice(cashier, f.cancelReq(inv.Id)); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	var shiftID, origID uuid.UUID
	var amount int64
	if err := f.pool.QueryRow(context.Background(), `SELECT shift_id, original_shift_id, amount_paisa FROM shift_adjustments WHERE invoice_id = $1`, inv.Id).
		Scan(&shiftID, &origID, &amount); err != nil {
		t.Fatalf("adjustment: %v", err)
	}
	if shiftID != second || origID != first || amount != 15000 {
		t.Fatalf("adjustment shift=%s orig=%s amount=%d, want %s/%s/15000", shiftID, origID, amount, second, first)
	}
}

// The counter-sale label is stored on the order too, so it agrees with the bill.
func TestCounterSaleLabelOnOrder(t *testing.T) {
	f := newBillFixture(t)
	cashier := f.as(auth.RoleCashier)
	f.openShift(cashier)
	inv, err := f.svc.CounterSale(cashier, &restaurantv1.CounterSaleRequest{CommandId: uuid.NewString(), Label: "Sita", Lines: []*restaurantv1.NewItem{f.line(1)}, Payments: []*restaurantv1.PaymentInput{cash(25000)}})
	if err != nil {
		t.Fatalf("counter sale: %v", err)
	}
	var label string
	if err := f.pool.QueryRow(context.Background(), `SELECT label FROM orders WHERE client_id = $1`, inv.OrderClientId).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if label != "Sita" || inv.Place != "Sita" {
		t.Fatalf("order label = %q, place = %q, want Sita", label, inv.Place)
	}
}

// Audit rows written during a request from an enrolled device carry its label.
func TestAuditRowCarriesDeviceLabel(t *testing.T) {
	f := newBillFixture(t)
	_, devToken, err := f.authSvc.EnrollDevice(f.owner, auth.ActorFrom(f.owner), "Counter 1")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// Re-resolve the owner's session from the enrolled device.
	req := connect.NewRequest(&restaurantv1.IssueBillRequest{})
	req.Header().Set("Authorization", "Bearer "+auth.ActorFrom(f.owner).Token)
	req.Header().Set("Cookie", "rm_device="+devToken)
	var onDevice context.Context
	_, _ = f.authSvc.Interceptor()(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		onDevice = c
		return nil, nil
	})(context.Background(), req)
	if auth.ActorFrom(onDevice).Device == nil {
		t.Fatal("device not resolved")
	}
	f.openShift(onDevice)
	ocid, _ := seedOrder(t, f.owner, f.pool, 1)
	inv := f.payAll(onDevice, ocid, 15000)

	var label string
	if err := f.pool.QueryRow(context.Background(), `SELECT COALESCE(device_label, '') FROM audit_log WHERE action = 'invoice.issue' AND entity_id = $1`, inv.Id).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if label != "Counter 1" {
		t.Fatalf("device_label = %q, want Counter 1", label)
	}
	// Without a device the label stays empty.
	ocid2, _ := seedOrder(t, f.owner, f.pool, 1)
	other := f.as(auth.RoleCashier)
	f.openShift(other)
	inv2 := f.payAll(other, ocid2, 15000)
	if n := f.count(`SELECT count(*) FROM audit_log WHERE action = 'invoice.issue' AND entity_id = $1 AND device_label IS NULL`, inv2.Id); n != 1 {
		t.Fatalf("un-enrolled audit rows without label = %d, want 1", n)
	}
}
