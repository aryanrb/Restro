package billing_test

import (
	"testing"

	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

func TestPreviewBillItemSubset(t *testing.T) {
	f := newBillFixture(t)
	ocid, items := seedOrder(t, f.owner, f.pool, 3) // 3 × 15000

	p, err := f.svc.PreviewBill(f.owner, &restaurantv1.PreviewBillRequest{
		OrderClientId: ocid.String(), ItemIds: ids(items[:2]),
		Discount: &restaurantv1.DiscountInput{Type: restaurantv1.DiscountType_DISCOUNT_TYPE_AMOUNT, Value: 3000},
	})
	if err != nil {
		t.Fatalf("preview subset: %v", err)
	}
	if p.SubtotalPaisa != 30000 || p.DiscountPaisa != 3000 || p.TotalPaisa != 27000 || p.EarnsStamp != nil {
		t.Fatalf("subtotal=%d discount=%d total=%d earns=%v", p.SubtotalPaisa, p.DiscountPaisa, p.TotalPaisa, p.EarnsStamp)
	}
	_, err = f.svc.PreviewBill(f.owner, &restaurantv1.PreviewBillRequest{OrderClientId: ocid.String(), ItemIds: []string{uuid.NewString()}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "item_ids")
}

func TestPreviewCounterSalePricesLinesWritingNothing(t *testing.T) {
	f := newBillFixture(t)
	before := f.count(`SELECT count(*) FROM orders`)

	p, err := f.svc.PreviewCounterSale(f.owner, &restaurantv1.PreviewCounterSaleRequest{
		Lines:    []*restaurantv1.NewItem{f.line(1), f.line(2)},
		Discount: &restaurantv1.DiscountInput{Type: restaurantv1.DiscountType_DISCOUNT_TYPE_AMOUNT, Value: 5000},
	})
	if err != nil {
		t.Fatalf("preview counter: %v", err)
	}
	if p.SubtotalPaisa != 75000 || p.DiscountPaisa != 5000 || p.TotalPaisa != 70000 {
		t.Fatalf("subtotal=%d discount=%d total=%d", p.SubtotalPaisa, p.DiscountPaisa, p.TotalPaisa)
	}
	if n := f.count(`SELECT count(*) FROM orders`); n != before {
		t.Fatalf("orders %d -> %d: a preview must not write", before, n)
	}
	bad := f.line(1)
	bad.MenuItemId = uuid.NewString()
	_, err = f.svc.PreviewCounterSale(f.owner, &restaurantv1.PreviewCounterSaleRequest{Lines: []*restaurantv1.NewItem{bad}})
	wantErr(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID, "lines")
}
