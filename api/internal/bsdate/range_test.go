package bsdate

import (
	"testing"
	"time"
)

func mustParseAD(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.ParseInLocation("2006-01-02", s, nepal)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

// The table must cover well past the old BS 2083 cliff (AD ~2027-04-13).
func TestFromADFutureYears(t *testing.T) {
	cases := []struct {
		ad   string
		want Date
	}{
		{"2027-04-13", Date{2083, 12, 30}}, // last day of 2083
		{"2027-04-14", Date{2084, 1, 1}},   // first day after the old cliff
		{"2027-11-25", Date{2084, 8, 10}},
		{"2033-04-14", Date{2090, 1, 1}},
		{"2033-10-01", Date{2090, 6, 15}},
		{"2034-04-13", Date{2090, 12, 30}}, // last supported day
	}
	for _, c := range cases {
		got, err := FromAD(mustParseAD(t, c.ad))
		if err != nil {
			t.Fatalf("FromAD(%s): %v", c.ad, err)
		}
		if got != c.want {
			t.Fatalf("FromAD(%s) = %s, want %s", c.ad, got, c.want)
		}
	}
}

func TestNewYearBoundaries(t *testing.T) {
	// Baisakh 1 AD dates (nepali-date-converter@3.4.2).
	cases := map[int]string{
		2070: "2013-04-14", 2073: "2016-04-13", 2077: "2020-04-13", 2081: "2024-04-13",
		2083: "2026-04-14", 2084: "2027-04-14", 2085: "2028-04-13", 2088: "2031-04-15",
		2090: "2033-04-14",
	}
	for y, s := range cases {
		day := mustParseAD(t, s)
		got, err := FromAD(day)
		if err != nil || got != (Date{y, 1, 1}) {
			t.Fatalf("FromAD(%s) = %s, %v; want %d.01.01", s, got, err, y)
		}
		if y > 2070 {
			prev, err := FromAD(day.AddDate(0, 0, -1))
			if err != nil || prev.Year != y-1 || prev.Month != 12 || prev.Day != table[y-1].months[11] {
				t.Fatalf("day before %d.01.01 = %s, %v; want last day of %d Chaitra", y, prev, err, y-1)
			}
		}
		back, err := (Date{y, 1, 1}).ToAD()
		if err != nil || !back.Equal(day) {
			t.Fatalf("ToAD(%d.01.01) = %s, %v; want %s", y, back, err, s)
		}
	}
}

func TestFiscalYearRolloverShrawan1(t *testing.T) {
	cases := []struct {
		lastAsar, shrawan1 string
		y                  int
	}{
		{"2026-07-16", "2026-07-17", 2083},
		{"2027-07-16", "2027-07-17", 2084},
		{"2032-07-15", "2032-07-16", 2089},
	}
	for _, c := range cases {
		a, err := FromAD(mustParseAD(t, c.lastAsar))
		if err != nil {
			t.Fatal(err)
		}
		s, err := FromAD(mustParseAD(t, c.shrawan1))
		if err != nil {
			t.Fatal(err)
		}
		if a.Year != c.y || a.Month != 3 || s != (Date{c.y, 4, 1}) {
			t.Fatalf("%s -> %s, %s -> %s; want end of %d Asar then %d.04.01", c.lastAsar, a, c.shrawan1, s, c.y, c.y)
		}
		wantPrev := Date{c.y - 1, 4, 1}.FiscalYear()
		wantNew := Date{c.y, 4, 1}.FiscalYear()
		if a.FiscalYear() != wantPrev || s.FiscalYear() != wantNew || a.FiscalYear() == s.FiscalYear() {
			t.Fatalf("FY rollover %d: Asar=%s Shrawan=%s", c.y, a.FiscalYear(), s.FiscalYear())
		}
	}
	if got := (Date{2084, 4, 1}).FiscalYear(); got != "2084.085" {
		t.Fatalf("FY(2084.04.01) = %s, want 2084.085", got)
	}
	if got := (Date{2084, 3, 32}).FiscalYear(); got != "2083.084" {
		t.Fatalf("FY(2084.03.32) = %s, want 2083.084", got)
	}
}

// Each year's Baisakh-1 anchor must equal the previous anchor + previous year length,
// so a typo in either a month length or an anchor is caught.
func TestTableContiguous(t *testing.T) {
	for i := 1; i < len(years); i++ {
		p, y := years[i-1], years[i]
		if y != p+1 {
			t.Fatalf("gap in table between %d and %d", p, y)
		}
		total := 0
		for _, m := range table[p].months {
			total += m
		}
		if want := table[p].baishakh1.AddDate(0, 0, total); !table[y].baishakh1.Equal(want) {
			t.Fatalf("%d anchor %s, want %s (from %d + %d days)", y, table[y].baishakh1.Format("2006-01-02"), want.Format("2006-01-02"), p, total)
		}
	}
}

func TestCrossCheckNepaliDateConverter(t *testing.T) {
	for _, f := range npmFixtures {
		day := mustParseAD(t, f.ad)
		got, err := FromAD(day)
		if err != nil {
			t.Fatalf("FromAD(%s): %v", f.ad, err)
		}
		if got != f.bs {
			t.Fatalf("FromAD(%s) = %s, npm says %s", f.ad, got, f.bs)
		}
		back, err := f.bs.ToAD()
		if err != nil || !back.Equal(day) {
			t.Fatalf("ToAD(%s) = %s, %v; npm says %s", f.bs, back.Format("2006-01-02"), err, f.ad)
		}
	}
}

func TestOutOfRangeEdges(t *testing.T) {
	if _, err := FromAD(mustParseAD(t, "2013-04-13")); err == nil {
		t.Fatal("want error before BS 2070")
	}
	if _, err := FromAD(mustParseAD(t, "2034-04-14")); err == nil {
		t.Fatal("want error after BS 2090")
	}
}
