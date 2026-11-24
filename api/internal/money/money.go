// Package money computes invoice totals in integer paisa. All tax maths is here
// and nowhere else; clients only display what this returns.
//
// Order of operations (design §8): subtotal − discount + service charge → VAT → total.
// VAT is rounded half-up once, at the invoice level. Prices are VAT-inclusive by
// default (VAT = gross × rate/(10000+rate), i.e. 13/113 at 13%).
//
// FLAGGED: the exact IRD rounding convention is unconfirmed (research 01 found no
// official rule). This implements design §8; confirm with an accountant before
// certified (TAX_INVOICE) use.
package money

// Input is the pre-tax picture. Subtotal is the sum of line amounts in paisa,
// where each line = (unit price + Σ modifier deltas) × quantity. Discount is the
// final discount amount in paisa (the service computes it from a % or amount).
type Input struct {
	Subtotal             int64
	Discount             int64
	ServiceChargeEnabled bool
	ServiceChargeRateBp  int32
	VatRegistered        bool
	PricesIncludeVat     bool
	VatRateBp            int32
	RoundTotalToRupee    bool
}

// Result is the computed breakdown. All fields are paisa.
type Result struct {
	Subtotal           int64
	Discount           int64
	ServiceCharge      int64
	Taxable            int64
	Vat                int64
	RoundingAdjustment int64
	Total              int64
}

// roundDiv divides a/b rounding half-up (inputs are non-negative here).
func roundDiv(a, b int64) int64 { return (a + b/2) / b }

func Compute(in Input) Result {
	r := Result{Subtotal: in.Subtotal, Discount: in.Discount}

	discounted := in.Subtotal - in.Discount
	if discounted < 0 {
		discounted = 0
	}
	if in.ServiceChargeEnabled && in.ServiceChargeRateBp > 0 {
		r.ServiceCharge = roundDiv(discounted*int64(in.ServiceChargeRateBp), 10000)
	}
	gross := discounted + r.ServiceCharge

	switch {
	case !in.VatRegistered || in.VatRateBp <= 0:
		// PAN bill: no VAT line.
		r.Taxable = gross
		r.Total = gross
	case in.PricesIncludeVat:
		// VAT extracted from the inclusive gross: vat = gross × rate/(10000+rate).
		r.Vat = roundDiv(gross*int64(in.VatRateBp), int64(10000+in.VatRateBp))
		r.Taxable = gross - r.Vat
		r.Total = gross
	default:
		// VAT added on top of a net gross.
		r.Taxable = gross
		r.Vat = roundDiv(gross*int64(in.VatRateBp), 10000)
		r.Total = gross + r.Vat
	}

	if in.RoundTotalToRupee {
		rounded := roundDiv(r.Total, 100) * 100
		r.RoundingAdjustment = rounded - r.Total
		r.Total = rounded
	}
	return r
}
