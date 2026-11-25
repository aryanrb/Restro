package inventory

import (
	"math/big"
	"time"

	"github.com/google/uuid"
)

// Quantities are int64 thousandths of the base unit (milli); unit cost is paisa
// per base unit ×10,000 (e4). quantity × cost therefore carries a 10^7 scale.
const costScale = 10_000_000

// Starting values from the spec (settings later).
const (
	// WasteApprovalPaisa: waste worth more than this in one entry needs a manager PIN.
	WasteApprovalPaisa = 100_000
	// Variance lines at or over Rs 500, or 10 % of expected, are highlighted.
	VarianceHighlightPaisa = 50_000
	varianceHighlightPct   = 10
)

// term is one quantity × unit cost product.
type term struct{ qtyMilli, costE4 int64 }

// valuePaisa sums quantity × cost terms exactly and rounds once to whole paisa,
// half up (toward +∞, like the web's Math.round).
func valuePaisa(terms ...term) int64 {
	sum := new(big.Int)
	p := new(big.Int)
	for _, t := range terms {
		sum.Add(sum, p.Mul(big.NewInt(t.qtyMilli), big.NewInt(t.costE4)))
	}
	// floor((2·sum + scale) / (2·scale))
	sum.Mul(sum, big.NewInt(2)).Add(sum, big.NewInt(costScale))
	q, m := new(big.Int), new(big.Int)
	q.DivMod(sum, big.NewInt(2*costScale), m) // Euclidean: floors for a positive divisor
	return q.Int64()
}

// MovingAverageE4 is the unit cost after receiving qty at cost (purchases): the
// weighted average of what is on hand (never below zero) and what came in,
// rounded half up to 4 decimals.
func MovingAverageE4(onHandMilli, oldCostE4, qtyMilli, costE4 int64) int64 {
	if onHandMilli < 0 {
		onHandMilli = 0
	}
	den := onHandMilli + qtyMilli
	if den <= 0 {
		return oldCostE4
	}
	num := new(big.Int).Mul(big.NewInt(onHandMilli), big.NewInt(oldCostE4))
	num.Add(num, new(big.Int).Mul(big.NewInt(qtyMilli), big.NewInt(costE4)))
	num.Mul(num, big.NewInt(2)).Add(num, big.NewInt(den))
	q, m := new(big.Int), new(big.Int)
	q.DivMod(num, big.NewInt(2*den), m)
	return q.Int64()
}

// highlighted: |value| >= Rs 500 or |variance| >= 10 % of |expected| (any variance
// counts when nothing was expected).
func highlighted(valuePaisa, varianceMilli, expectedMilli int64) bool {
	if abs(valuePaisa) >= VarianceHighlightPaisa {
		return true
	}
	if expectedMilli == 0 {
		return varianceMilli != 0
	}
	return abs(varianceMilli)*100 >= abs(expectedMilli)*varianceHighlightPct
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// now is the service clock, truncated to Postgres' microsecond precision so a time
// written and read back compares equal.
func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// Line is one ingredient: an inventory item and a quantity (milli) per 1 sold.
type Line struct {
	ItemID   uuid.UUID
	QtyMilli int64
}

// sumLines merges lines per ingredient, keeping first-seen order.
func sumLines(lines ...[]Line) []Line {
	var out []Line
	at := map[uuid.UUID]int{}
	for _, ls := range lines {
		for _, l := range ls {
			if i, ok := at[l.ItemID]; ok {
				out[i].QtyMilli += l.QtyMilli
				continue
			}
			at[l.ItemID] = len(out)
			out = append(out, l)
		}
	}
	return out
}

// dish is a menu item's recipes as the 86 check sees them: one recipe per variant
// (its own, or the item's), or just the item's when it has no variants.
type dish struct {
	ID       uuid.UUID
	Active   bool
	Is86d    bool
	Variants [][]Line
	Recipe   []Line
}

type shortage struct {
	MenuItemID      uuid.UUID
	InventoryItemID uuid.UUID
	LeftMilli       int64
}

// suggest86 lists dishes whose every recipe needs more of a tracked ingredient
// than is on hand (onHand holds tracked, active items only). The first short
// ingredient of the first recipe is reported.
func suggest86(dishes []dish, onHand map[uuid.UUID]int64) []shortage {
	var out []shortage
	for _, d := range dishes {
		if !d.Active || d.Is86d {
			continue
		}
		recipes := d.Variants
		if len(recipes) == 0 {
			recipes = [][]Line{d.Recipe}
		}
		var first *Line
		all := true
		for _, r := range recipes {
			var short *Line
			for i := range r {
				if left, ok := onHand[r[i].ItemID]; ok && left < r[i].QtyMilli {
					short = &r[i]
					break
				}
			}
			if short == nil {
				all = false
				break
			}
			if first == nil {
				first = short
			}
		}
		if all && first != nil {
			out = append(out, shortage{MenuItemID: d.ID, InventoryItemID: first.ItemID, LeftMilli: onHand[first.ItemID]})
		}
	}
	return out
}
