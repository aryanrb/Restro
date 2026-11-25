package analytics

import (
	"errors"
	"testing"
	"time"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/connecterr"
)

func day(t *testing.T, s string) time.Time {
	t.Helper()
	d, ok := parseDay(s)
	if !ok {
		t.Fatalf("bad day %q", s)
	}
	return d
}

func rng(t *testing.T, from, to string) dayRange {
	t.Helper()
	return dayRange{day(t, from), day(t, to)}
}

func rangeStr(r dayRange) string { return dayStr(r.from) + ".." + dayStr(r.to) }

func bsMonthRange(t *testing.T, year, month0 int) dayRange {
	t.Helper()
	from, err := bsToAd(year, month0+1, 1)
	if err != nil {
		t.Fatal(err)
	}
	n, err := bsMonthDays(year, month0+1)
	if err != nil {
		t.Fatal(err)
	}
	return dayRange{from, addDays(from, n-1)}
}

func fiscalYearRange(t *testing.T, start int) dayRange {
	t.Helper()
	from, err := bsToAd(start, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	next, err := bsToAd(start+1, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	return dayRange{from, addDays(next, -1)}
}

// Mirrors web/src/lib/analytics/periods.test.ts.
func TestADToBSForKathmanduDates(t *testing.T) {
	bs, err := adToBs(day(t, "2026-11-24"))
	if err != nil || bs.Year != 2083 || bs.Month != 8 || bs.Day != 8 {
		t.Fatalf("2026-11-24 = %v %v, want 8 Mangsir 2083", bs, err)
	}
	if d, _ := bsToAd(2083, 8, 1); dayStr(d) != "2026-11-17" {
		t.Fatalf("1 Mangsir 2083 = %s", dayStr(d))
	}
	if d, _ := bsToAd(2083, 1, 1); dayStr(d) != "2026-04-14" {
		t.Fatalf("BS new year = %s", dayStr(d))
	}
}

func TestBSMonthAndFiscalYearRanges(t *testing.T) {
	mangsir := bsMonthRange(t, 2083, 7)
	poush1, _ := bsToAd(2083, 9, 1)
	if dayStr(mangsir.from) != "2026-11-17" || !addDays(mangsir.to, 1).Equal(poush1) {
		t.Fatalf("mangsir = %s", rangeStr(mangsir))
	}
	if got := rangeStr(fiscalYearRange(t, 2083)); got != "2026-07-17..2027-07-16" {
		t.Fatalf("FY 2083 = %s", got)
	}
	for in, want := range map[string]int{"2026-07-16": 2082, "2026-07-17": 2083} {
		if got, _ := fiscalYearOf(day(t, in)); got != want {
			t.Fatalf("fiscalYearOf(%s) = %d, want %d", in, got, want)
		}
	}
}

func TestWeeksRunSundayToSaturday(t *testing.T) {
	d := day(t, "2026-11-24")
	if weekday(d) != 2 || dayStr(weekStart(d)) != "2026-11-22" {
		t.Fatalf("weekday %d, week start %s", weekday(d), dayStr(weekStart(d)))
	}
}

func TestComparisonRanges(t *testing.T) {
	if got := rangeStr(previousRange(rng(t, "2026-11-17", "2026-11-23"))); got != "2026-11-10..2026-11-16" {
		t.Fatalf("previous = %s", got)
	}
	ly, err := lastYearRange(bsMonthRange(t, 2083, 7))
	if err != nil || rangeStr(ly) != rangeStr(bsMonthRange(t, 2082, 7)) {
		t.Fatalf("last year of Mangsir 2083 = %s %v", rangeStr(ly), err)
	}
	ly, err = lastYearRange(fiscalYearRange(t, 2083))
	if err != nil || rangeStr(ly) != rangeStr(fiscalYearRange(t, 2082)) {
		t.Fatalf("last year of FY 2083 = %s %v", rangeStr(ly), err)
	}
}

func TestRangeLimits(t *testing.T) {
	if got := countDays(day(t, "2026-01-01"), day(t, "2026-01-31")); got != 31 {
		t.Fatalf("countDays = %d", got)
	}
	if got := len(daysIn(day(t, "2026-02-27"), day(t, "2026-03-02"))); got != 4 {
		t.Fatalf("daysIn = %d", got)
	}
	three := dayRange{fiscalYearRange(t, 2081).from, fiscalYearRange(t, 2083).to}
	if _, err := parseRange(dayStr(three.from), dayStr(three.to)); err != nil {
		t.Fatalf("3 fiscal years rejected: %v", err)
	}
	var ce *connecterr.Error
	if _, err := parseRange("2020-01-01", "2026-01-01"); !errors.As(err, &ce) || ce.Reason != restaurantv1.ErrorReason_ERROR_REASON_RANGE_TOO_LARGE {
		t.Fatalf("6 years: %v", err)
	}
	for _, bad := range [][2]string{{"2026-01-02", "2026-01-01"}, {"2026-1-1", "2026-01-02"}, {"", ""}} {
		if _, err := parseRange(bad[0], bad[1]); !errors.As(err, &ce) || ce.Reason != restaurantv1.ErrorReason_ERROR_REASON_INVALID {
			t.Fatalf("%v: %v", bad, err)
		}
	}
}

func TestPeriodsOfGrains(t *testing.T) {
	r := rng(t, "2026-11-15", "2026-11-24") // Sun 15 Nov … Tue 24 Nov; Mangsir starts 17 Nov
	weeks, _ := periodsOf(r, grainWeek)
	if len(weeks) != 2 || weeks[1].key != "2026-11-22" || dayStr(weeks[0].from) != "2026-11-15" {
		t.Fatalf("weeks = %+v", weeks)
	}
	months, _ := periodsOf(r, grainMonthBS)
	if len(months) != 2 || months[0].key != "2083-06" || months[1].key != "2083-07" || dayStr(months[1].from) != "2026-11-17" {
		t.Fatalf("months = %+v", months)
	}
	fy, _ := periodsOf(rng(t, "2026-07-16", "2026-07-17"), grainFY)
	if len(fy) != 2 || fy[0].key != "2082" || fy[1].key != "2083" {
		t.Fatalf("fy = %+v", fy)
	}
}
