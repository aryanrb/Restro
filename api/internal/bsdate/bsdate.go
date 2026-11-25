// Package bsdate converts between Gregorian (AD) and Bikram Sambat (BS) dates and
// resolves the Nepali fiscal year (which starts Shrawan 1 — BS month 4).
//
// Month-length table, BS 2070–2090. Rows 2076–2083 were originally taken from
// rjach/sambat (checked against the Nepal Panchanga Nirnayak Bikas Samiti national
// panchang for 2082–2083, Surya Panchanga for 2081). All rows 2070–2090 match the
// npm package nepali-date-converter@3.4.2 (dist/lib/date-config.js, the converter
// the web frontend uses; epoch BS 2000-01-01 = AD 1943-04-14), from which 2070–2075
// and 2084–2090 were taken, so backend and frontend agree on every date. Each year
// carries its own Baishakh-1 AD anchor; TestTableContiguous checks each anchor equals
// the previous anchor plus the previous year's length.
//
// Rows beyond the currently published national panchang (2084+) are the package's
// projected values; re-verify them against the official panchang as each year is
// published. The package ends at BS 2090 (AD 2034-04-13) and FromAD errors beyond it.
package bsdate

import (
	"fmt"
	"sort"
	"time"
)

// Nepal is UTC+5:45, no DST — a fixed zone avoids any tzdata dependency.
var nepal = time.FixedZone("NPT", 5*3600+45*60)

type yearInfo struct {
	baishakh1 time.Time // midnight NPT of Baishakh 1
	months    [12]int   // days in months 1..12 (Baishakh..Chaitra)
}

func npt(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, nepal) }

var table = map[int]yearInfo{
	2070: {npt(2013, 4, 14), [12]int{31, 31, 31, 32, 31, 31, 29, 30, 30, 29, 30, 30}},
	2071: {npt(2014, 4, 14), [12]int{31, 31, 32, 31, 31, 31, 30, 29, 30, 29, 30, 30}},
	2072: {npt(2015, 4, 14), [12]int{31, 32, 31, 32, 31, 30, 30, 29, 30, 29, 30, 30}},
	2073: {npt(2016, 4, 13), [12]int{31, 32, 31, 32, 31, 30, 30, 30, 29, 29, 30, 31}},
	2074: {npt(2017, 4, 14), [12]int{31, 31, 31, 32, 31, 31, 30, 29, 30, 29, 30, 30}},
	2075: {npt(2018, 4, 14), [12]int{31, 31, 32, 31, 31, 31, 30, 29, 30, 29, 30, 30}},
	2076: {npt(2019, 4, 14), [12]int{31, 32, 31, 32, 31, 30, 30, 30, 29, 29, 30, 30}},
	2077: {npt(2020, 4, 13), [12]int{31, 32, 31, 32, 31, 30, 30, 30, 29, 30, 29, 31}},
	2078: {npt(2021, 4, 14), [12]int{31, 31, 31, 32, 31, 31, 30, 29, 30, 29, 30, 30}},
	2079: {npt(2022, 4, 14), [12]int{31, 31, 32, 31, 31, 31, 30, 29, 30, 29, 30, 30}},
	2080: {npt(2023, 4, 14), [12]int{31, 32, 31, 32, 31, 30, 30, 30, 29, 29, 30, 30}},
	2081: {npt(2024, 4, 13), [12]int{31, 32, 31, 32, 31, 30, 30, 30, 29, 30, 29, 31}},
	2082: {npt(2025, 4, 14), [12]int{31, 31, 32, 31, 31, 31, 30, 29, 30, 29, 30, 30}},
	2083: {npt(2026, 4, 14), [12]int{31, 31, 32, 31, 31, 31, 30, 29, 30, 29, 30, 30}},
	2084: {npt(2027, 4, 14), [12]int{31, 31, 32, 31, 31, 30, 30, 30, 29, 30, 30, 30}},
	2085: {npt(2028, 4, 13), [12]int{31, 32, 31, 32, 30, 31, 30, 30, 29, 30, 30, 30}},
	2086: {npt(2029, 4, 14), [12]int{30, 32, 31, 32, 31, 30, 30, 30, 29, 30, 30, 30}},
	2087: {npt(2030, 4, 14), [12]int{31, 31, 32, 31, 31, 31, 30, 30, 29, 30, 30, 30}},
	2088: {npt(2031, 4, 15), [12]int{30, 31, 32, 32, 30, 31, 30, 30, 29, 30, 30, 30}},
	2089: {npt(2032, 4, 14), [12]int{30, 32, 31, 32, 31, 30, 30, 30, 29, 30, 30, 30}},
	2090: {npt(2033, 4, 14), [12]int{30, 32, 31, 32, 31, 30, 30, 30, 29, 30, 30, 30}},
}

var years []int

func init() {
	for y := range table {
		years = append(years, y)
	}
	sort.Ints(years)
}

// Date is a Bikram Sambat calendar date.
type Date struct {
	Year  int
	Month int // 1..12 (Baishakh..Chaitra)
	Day   int
}

// String formats as YYYY.MM.DD (CBMS/print format).
func (d Date) String() string { return fmt.Sprintf("%04d.%02d.%02d", d.Year, d.Month, d.Day) }

// FiscalYear returns the Nepali fiscal year as "YYYY.0YY" (e.g. "2083.084").
// The fiscal year starts Shrawan 1 (month 4); months 1–3 belong to the prior FY.
func (d Date) FiscalYear() string {
	start := d.Year
	if d.Month < 4 {
		start = d.Year - 1
	}
	return fmt.Sprintf("%d.%03d", start, (start+1)%1000)
}

const oneDay = 24 * time.Hour

// FromAD converts an AD instant to the BS calendar date in Nepal local time.
func FromAD(t time.Time) (Date, error) {
	tn := t.In(nepal)
	target := npt(tn.Year(), int(tn.Month()), tn.Day())
	for _, y := range years {
		info := table[y]
		total := 0
		for _, m := range info.months {
			total += m
		}
		end := info.baishakh1.AddDate(0, 0, total)
		if target.Before(info.baishakh1) || !target.Before(end) {
			continue
		}
		offset := int(target.Sub(info.baishakh1) / oneDay)
		for mi, md := range info.months {
			if offset < md {
				return Date{Year: y, Month: mi + 1, Day: offset + 1}, nil
			}
			offset -= md
		}
	}
	return Date{}, fmt.Errorf("bsdate: AD %s is outside the supported BS range (2070–2090); extend the verified calendar table", target.Format("2006-01-02"))
}

// ToAD returns midnight (Nepal time) of the BS date.
func (d Date) ToAD() (time.Time, error) {
	info, ok := table[d.Year]
	if !ok {
		return time.Time{}, fmt.Errorf("bsdate: BS year %d is outside the supported range", d.Year)
	}
	if d.Month < 1 || d.Month > 12 || d.Day < 1 || d.Day > info.months[d.Month-1] {
		return time.Time{}, fmt.Errorf("bsdate: invalid BS date %s", d)
	}
	offset := d.Day - 1
	for i := 0; i < d.Month-1; i++ {
		offset += info.months[i]
	}
	return info.baishakh1.AddDate(0, 0, offset), nil
}
