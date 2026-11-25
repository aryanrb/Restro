package purchases

import "math/big"

// Pure bill arithmetic, ported from the web's lib/purchases/compute.ts. Money is
// paisa; quantities are thousandths (milli); stock-unit cost is paisa ×10,000.
// Every rounding is half up (Math.round) on non-negative values.

// roundDiv returns round(a × b / c) half up, exactly (c > 0, a·b >= 0).
func roundDiv(a, b, c int64) int64 {
	num := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	num.Mul(num, big.NewInt(2)).Add(num, big.NewInt(c))
	q := new(big.Int).Quo(num, big.NewInt(2*c))
	return q.Int64()
}

// lineAmount: quantity (purchase units, milli) × unit cost (paisa per purchase unit).
func lineAmount(qtyMilli, unitCost int64) int64 { return roundDiv(qtyMilli, unitCost, 1000) }

// stockMilli: purchase units (milli) × factor (stock-unit milli per purchase unit), in stock-unit milli.
func stockMilli(qtyMilli, factorMilli int64) int64 { return roundDiv(qtyMilli, factorMilli, 1000) }

// spread splits total over weights in proportion; the parts always add up to
// total (cumulative rounding).
func spread(total int64, weights []int64) []int64 {
	var sum int64
	for _, w := range weights {
		sum += w
	}
	out := make([]int64, len(weights))
	if sum == 0 {
		return out
	}
	var cum, given int64
	for i, w := range weights {
		cum += w
		upTo := roundDiv(total, cum, sum)
		out[i] = upTo - given
		given = upTo
	}
	return out
}

type lineMoney struct {
	amount     int64
	vatApplies bool
}

type billMath struct {
	gross, taxable, exempt int64
	discounts, nets        []int64
}

// billTotals spreads the bill discount over the lines by amount; taxable and
// exempt are after the discount.
func billTotals(lines []lineMoney, discount int64) billMath {
	weights := make([]int64, len(lines))
	var m billMath
	for i, l := range lines {
		weights[i] = l.amount
		m.gross += l.amount
	}
	m.discounts = spread(discount, weights)
	m.nets = make([]int64, len(lines))
	for i, l := range lines {
		m.nets[i] = l.amount - m.discounts[i]
		if l.vatApplies {
			m.taxable += m.nets[i]
		} else {
			m.exempt += m.nets[i]
		}
	}
	return m
}

// vatShares spreads the VAT typed from the bill over the taxable lines by net amount.
func vatShares(nets []int64, vatApplies []bool, vat int64) []int64 {
	weights := make([]int64, len(nets))
	for i := range nets {
		if vatApplies[i] {
			weights[i] = nets[i]
		}
	}
	return spread(vat, weights)
}

// costPerStockUnitE4: paisa per stock unit ×10,000. Claimable VAT stays out of
// cost; otherwise it is part of it.
func costPerStockUnitE4(net, vat, qtyMilli, factorMilli int64, includeVat bool) int64 {
	units := stockMilli(qtyMilli, factorMilli)
	if units <= 0 {
		return 0
	}
	value := net
	if includeVat {
		value += vat
	}
	return roundDiv(value, 10_000_000, units)
}

type lineValue struct{ qtyMilli, net, vat int64 }

// returnValue is the value of returning qty of a line; returning the last of it
// takes exactly what is left.
func returnValue(line lineValue, qtyMilli int64, already lineValue) (net, vat int64) {
	if already.qtyMilli+qtyMilli >= line.qtyMilli {
		return line.net - already.net, line.vat - already.vat
	}
	return roundDiv(line.net, qtyMilli, line.qtyMilli), roundDiv(line.vat, qtyMilli, line.qtyMilli)
}

// ceilUnits: whole purchase units (rounded up) to cover needMilli stock-unit milli.
func ceilUnits(needMilli, factorMilli int64) int64 {
	r3 := roundDiv(needMilli, 1000, factorMilli) // purchase units, milli (round3)
	return (r3 + 999) / 1000
}
