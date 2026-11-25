package inventory

import (
	"testing"

	"github.com/google/uuid"
)

func TestValuePaisaRoundsOnceHalfUp(t *testing.T) {
	cases := []struct {
		name  string
		terms []term
		want  int64
	}{
		{"400 g at 60 paisa/g", []term{{400_000, 600_000}}, 24_000},
		{"4 pcs at Rs 410", []term{{-4_000, 410_000_000}}, -164_000},
		{"half paisa rounds up", []term{{1, 5_000_000}}, 1},           // 0.5
		{"minus half rounds toward +inf", []term{{-1, 5_000_000}}, 0}, // -0.5
		{"just under minus half", []term{{-1, 5_000_001}}, -1},
		{"summed before rounding", []term{{1, 4_000_000}, {1, 4_000_000}}, 1}, // 0.4 + 0.4
		{"no terms", nil, 0},
	}
	for _, c := range cases {
		if got := valuePaisa(c.terms...); got != c.want {
			t.Errorf("%s: valuePaisa = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestMovingAverage(t *testing.T) {
	// 10 kg at 60 paisa/g on hand, buy 10 kg at 70 → 65.
	if got := MovingAverageE4(10_000_000, 600_000, 10_000_000, 700_000); got != 650_000 {
		t.Fatalf("avg = %d", got)
	}
	// negative on hand counts as zero: the purchase price wins.
	if got := MovingAverageE4(-5_000, 600_000, 1_000, 700_000); got != 700_000 {
		t.Fatalf("avg = %d", got)
	}
	// rounding to 4 decimals, half up: (1×1 + 2×2) / 3 = 1.6667 → 16667 e4 for costs 10000/20000
	if got := MovingAverageE4(1_000, 10_000, 2_000, 20_000); got != 16_667 {
		t.Fatalf("avg = %d", got)
	}
}

func TestHighlighted(t *testing.T) {
	cases := []struct {
		value, variance, expected int64
		want                      bool
	}{
		{-50_000, -1, 1_000_000, true}, // Rs 500
		{-49_999, -1, 1_000_000, false},
		{-100, -100_000, 1_000_000, true}, // 10 %
		{-100, -99_999, 1_000_000, false},
		{0, 1, 0, true}, // anything when nothing was expected
		{0, 0, 0, false},
	}
	for _, c := range cases {
		if got := highlighted(c.value, c.variance, c.expected); got != c.want {
			t.Errorf("highlighted(%d,%d,%d) = %v", c.value, c.variance, c.expected, got)
		}
	}
}

func TestSumLinesMergesPerIngredient(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	got := sumLines([]Line{{a, 120}, {b, 80}}, []Line{{a, 10}})
	if len(got) != 2 || got[0] != (Line{a, 130}) || got[1] != (Line{b, 80}) {
		t.Fatalf("sum = %v", got)
	}
}

func TestSuggest86(t *testing.T) {
	chicken, flour := uuid.New(), uuid.New()
	momo := dish{ID: uuid.New(), Active: true, Recipe: []Line{{flour, 80}, {chicken, 120}}}
	dal := dish{ID: uuid.New(), Active: true, Variants: [][]Line{{{chicken, 150}}, {{flour, 10}}}}
	off := dish{ID: uuid.New(), Active: true, Is86d: true, Recipe: []Line{{chicken, 120}}}
	none := dish{ID: uuid.New(), Active: true}
	onHand := map[uuid.UUID]int64{chicken: 80} // flour untracked: not in the map
	got := suggest86([]dish{momo, dal, off, none}, onHand)
	if len(got) != 1 || got[0].MenuItemID != momo.ID || got[0].InventoryItemID != chicken || got[0].LeftMilli != 80 {
		t.Fatalf("suggestions = %v (dal's veg variant can still be made; 86'd and recipe-less dishes skipped)", got)
	}
}
