package bsdate

import (
	"testing"
	"time"
)

func ad(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 12, 0, 0, 0, time.UTC) }

func TestFromAD(t *testing.T) {
	cases := []struct {
		ad   time.Time
		want Date
	}{
		{ad(2023, 4, 14), Date{2080, 1, 1}},   // Nepali New Year 2080
		{ad(2024, 4, 13), Date{2081, 1, 1}},   // 2081
		{ad(2026, 4, 14), Date{2083, 1, 1}},   // 2083
		{ad(2026, 11, 23), Date{2083, 8, 7}},  // research anchor: BS 2083 Mangsir 7
	}
	for _, c := range cases {
		got, err := FromAD(c.ad)
		if err != nil {
			t.Fatalf("FromAD(%s): %v", c.ad.Format("2006-01-02"), err)
		}
		if got != c.want {
			t.Fatalf("FromAD(%s) = %s, want %s", c.ad.Format("2006-01-02"), got, c.want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for _, c := range []Date{{2080, 1, 1}, {2083, 8, 7}, {2082, 12, 30}, {2081, 6, 15}} {
		adt, err := c.ToAD()
		if err != nil {
			t.Fatalf("ToAD(%s): %v", c, err)
		}
		back, err := FromAD(adt)
		if err != nil {
			t.Fatalf("FromAD: %v", err)
		}
		if back != c {
			t.Fatalf("round trip %s -> %s -> %s", c, adt.Format("2006-01-02"), back)
		}
	}
}

func TestFiscalYear(t *testing.T) {
	// Shrawan (month 4) onward is the FY starting that BS year.
	if got := (Date{2083, 8, 7}).FiscalYear(); got != "2083.084" {
		t.Fatalf("FY(2083-08) = %s, want 2083.084", got)
	}
	// Baishakh/Jestha/Asar (months 1-3) belong to the prior FY.
	if got := (Date{2083, 2, 10}).FiscalYear(); got != "2082.083" {
		t.Fatalf("FY(2083-02) = %s, want 2082.083", got)
	}
}

func TestOutOfRange(t *testing.T) {
	if _, err := FromAD(ad(2040, 1, 1)); err == nil {
		t.Fatal("want error for AD date beyond the verified BS table")
	}
}
