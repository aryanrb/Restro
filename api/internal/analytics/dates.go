package analytics

import (
	"fmt"
	"time"

	"github.com/oneaccord/restro/api/internal/bsdate"
	"github.com/oneaccord/restro/api/internal/connecterr"
)

// Calendar helpers. A business day is a Kathmandu calendar date, carried as a
// time.Time at UTC midnight (so day arithmetic never meets a zone); on the wire it
// is "YYYY-MM-DD". BS months are 0-based in period keys (0 = Baisakh, 7 = Mangsir),
// as in the web's analytics/periods.ts; bsdate months are 1-based.

// MaxRangeDays: 3 fiscal years per call at most (RANGE_TOO_LARGE).
const MaxRangeDays = 3 * 366

var nepal = time.FixedZone("NPT", 5*3600+45*60)

const dayLayout = "2006-01-02"

func parseDay(s string) (time.Time, bool) {
	if len(s) != len(dayLayout) {
		return time.Time{}, false
	}
	t, err := time.Parse(dayLayout, s)
	return t, err == nil
}

func dayStr(d time.Time) string { return d.Format(dayLayout) }

func addDays(d time.Time, n int) time.Time { return d.AddDate(0, 0, n) }

// ktmDay is the Kathmandu business day of an instant.
func ktmDay(t time.Time) time.Time {
	n := t.In(nepal)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

// startNPT is midnight Kathmandu at the start of a business day.
func startNPT(d time.Time) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, nepal)
}

func ktmHour(t time.Time) int { return t.In(nepal).Hour() }

// weekday: 0 = Sunday … 6 = Saturday.
func weekday(d time.Time) int { return int(d.Weekday()) }

// countDays in an inclusive range.
func countDays(from, to time.Time) int { return int(to.Sub(from).Hours()/24) + 1 }

// daysIn lists every day from..to inclusive.
func daysIn(from, to time.Time) []time.Time {
	var out []time.Time
	for d := from; !d.After(to); d = addDays(d, 1) {
		out = append(out, d)
	}
	return out
}

func minDay(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxDay(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// dayRange is an inclusive range of business days.
type dayRange struct{ from, to time.Time }

func (r dayRange) days() int { return countDays(r.from, r.to) }

func (r dayRange) overlaps(from, to time.Time) bool { return !from.After(r.to) && !to.Before(r.from) }

// parseRange validates a wire range: well-formed dates, from <= to (INVALID,
// field "range"), and at most MaxRangeDays days (RANGE_TOO_LARGE).
func parseRange(from, to string) (dayRange, error) {
	f, ok1 := parseDay(from)
	t, ok2 := parseDay(to)
	if !ok1 || !ok2 || f.After(t) {
		return dayRange{}, connecterr.Invalid("range", "range must be from <= to, as YYYY-MM-DD")
	}
	r := dayRange{f, t}
	if r.days() > MaxRangeDays {
		return dayRange{}, RangeTooLarge()
	}
	return r, nil
}

// adToBs is the BS date of a business day.
func adToBs(d time.Time) (bsdate.Date, error) {
	bs, err := bsdate.FromAD(startNPT(d))
	if err != nil {
		return bsdate.Date{}, connecterr.Invalid("range", "date outside the supported BS calendar")
	}
	return bs, nil
}

// bsToAd: month is 1-based (bsdate).
func bsToAd(year, month, day int) (time.Time, error) {
	t, err := bsdate.Date{Year: year, Month: month, Day: day}.ToAD()
	if err != nil {
		return time.Time{}, connecterr.Invalid("range", "date outside the supported BS calendar")
	}
	return ktmDay(t), nil
}

// bsMonthDays: month is 1-based.
func bsMonthDays(year, month int) (int, error) {
	first, err := bsToAd(year, month, 1)
	if err != nil {
		return 0, err
	}
	ny, nm := year, month+1
	if nm == 13 {
		ny, nm = year+1, 1
	}
	next, err := bsToAd(ny, nm, 1)
	if err != nil {
		return 0, err
	}
	return countDays(first, next) - 1, nil
}

// fiscalYearOf is the fiscal year's starting BS year (Shrawan–Ashadh).
func fiscalYearOf(d time.Time) (int, error) {
	bs, err := adToBs(d)
	if err != nil {
		return 0, err
	}
	if bs.Month >= 4 {
		return bs.Year, nil
	}
	return bs.Year - 1, nil
}

// weekStart is the Sunday of d's Sunday–Saturday week.
func weekStart(d time.Time) time.Time { return addDays(d, -weekday(d)) }

// previousRange is the range of equal length just before.
func previousRange(r dayRange) dayRange {
	n := r.days()
	return dayRange{addDays(r.from, -n), addDays(r.from, -1)}
}

func bsMinusYear(d time.Time) (time.Time, error) {
	bs, err := adToBs(d)
	if err != nil {
		return time.Time{}, err
	}
	n, err := bsMonthDays(bs.Year-1, bs.Month)
	if err != nil {
		return time.Time{}, err
	}
	return bsToAd(bs.Year-1, bs.Month, min(bs.Day, n))
}

// lastYearRange is the same BS period a year earlier (Mangsir 2083 -> Mangsir
// 2082); a range ending on a month end still ends on that month's end.
func lastYearRange(r dayRange) (dayRange, error) {
	from, err := bsMinusYear(r.from)
	if err != nil {
		return dayRange{}, err
	}
	to, err := adToBs(r.to)
	if err != nil {
		return dayRange{}, err
	}
	n, err := bsMonthDays(to.Year, to.Month)
	if err != nil {
		return dayRange{}, err
	}
	var end time.Time
	if to.Day == n {
		last, err := bsMonthDays(to.Year-1, to.Month)
		if err != nil {
			return dayRange{}, err
		}
		end, err = bsToAd(to.Year-1, to.Month, last)
		if err != nil {
			return dayRange{}, err
		}
	} else if end, err = bsMinusYear(r.to); err != nil {
		return dayRange{}, err
	}
	return dayRange{from, end}, nil
}

// period is one slice of a range at a grain, clipped to the range.
type period struct {
	key      string
	from, to time.Time
}

type grain int

const (
	grainDay grain = iota
	grainWeek
	grainMonthBS
	grainFY
)

// periodsOf splits a range into periods: DAY key "2026-11-24", WEEK its Sunday,
// MONTH_BS "2083-07" (0-based month), FY "2083".
func periodsOf(r dayRange, g grain) ([]period, error) {
	var out []period
	for _, d := range daysIn(r.from, r.to) {
		var key string
		switch g {
		case grainWeek:
			key = dayStr(weekStart(d))
		case grainMonthBS:
			bs, err := adToBs(d)
			if err != nil {
				return nil, err
			}
			key = fmt.Sprintf("%d-%02d", bs.Year, bs.Month-1)
		case grainFY:
			fy, err := fiscalYearOf(d)
			if err != nil {
				return nil, err
			}
			key = fmt.Sprint(fy)
		default:
			key = dayStr(d)
		}
		if n := len(out); n > 0 && out[n-1].key == key {
			out[n-1].to = d
		} else {
			out = append(out, period{key, d, d})
		}
	}
	return out, nil
}
