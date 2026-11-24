package money

import "testing"

func TestCompute(t *testing.T) {
	const vat13 = 1300
	cases := []struct {
		name string
		in   Input
		want Result
	}{
		{
			name: "vat-inclusive, exact",
			in:   Input{Subtotal: 11300, VatRegistered: true, PricesIncludeVat: true, VatRateBp: vat13},
			want: Result{Subtotal: 11300, Taxable: 10000, Vat: 1300, Total: 11300},
		},
		{
			name: "vat-inclusive, rounds half-up",
			in:   Input{Subtotal: 10000, VatRegistered: true, PricesIncludeVat: true, VatRateBp: vat13},
			want: Result{Subtotal: 10000, Taxable: 8850, Vat: 1150, Total: 10000}, // 10000*1300/11300 = 1150.44 -> 1150
		},
		{
			name: "service charge then vat (inclusive)",
			in:   Input{Subtotal: 10000, ServiceChargeEnabled: true, ServiceChargeRateBp: 1000, VatRegistered: true, PricesIncludeVat: true, VatRateBp: vat13},
			want: Result{Subtotal: 10000, ServiceCharge: 1000, Taxable: 9735, Vat: 1265, Total: 11000}, // sc 1000, gross 11000, vat 11000*13/113=1265.49->1265
		},
		{
			name: "discount then vat (inclusive)",
			in:   Input{Subtotal: 11300, Discount: 1300, VatRegistered: true, PricesIncludeVat: true, VatRateBp: vat13},
			want: Result{Subtotal: 11300, Discount: 1300, Taxable: 8850, Vat: 1150, Total: 10000},
		},
		{
			name: "non-vat registered: PAN bill, no vat",
			in:   Input{Subtotal: 10000, VatRegistered: false},
			want: Result{Subtotal: 10000, Taxable: 10000, Total: 10000},
		},
		{
			name: "vat added on top (not inclusive)",
			in:   Input{Subtotal: 10000, VatRegistered: true, PricesIncludeVat: false, VatRateBp: vat13},
			want: Result{Subtotal: 10000, Taxable: 10000, Vat: 1300, Total: 11300},
		},
		{
			name: "round total to rupee",
			in:   Input{Subtotal: 11265, VatRegistered: false, RoundTotalToRupee: true},
			want: Result{Subtotal: 11265, Taxable: 11265, RoundingAdjustment: 35, Total: 11300},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Compute(c.in); got != c.want {
				t.Fatalf("Compute() = %+v\nwant %+v", got, c.want)
			}
		})
	}
}
