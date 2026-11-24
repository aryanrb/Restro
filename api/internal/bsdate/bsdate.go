// Package bsdate converts between Gregorian (AD) and Bikram Sambat (BS) dates and
// resolves the Nepali fiscal year (which starts Shrawan 1 — BS month 4).
//
// The month-length table is panchang-verified data from rjach/sambat (checked
// against the Nepal Panchanga Nirnayak Bikas Samiti national panchang for
// 2082–2083, Surya Panchanga for 2081). Each year carries its own Baishakh-1 AD
// anchor, so there is no accumulation drift.
//
// IMPORTANT: the table currently ends at BS 2083 (AD ~mid-2027). FromAD returns an
// error beyond it; append the next year's VERIFIED panchang row before the pilot
// crosses into BS 2084. Do NOT guess future rows — invoice dates are a tax record.
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
	2076: {npt(2019, 4, 14), [12]int{31, 32, 31, 32, 31, 30, 30, 30, 29, 29, 30, 30}},
	2077: {npt(2020, 4, 13), [12]int{31, 32, 31, 32, 31, 30, 30, 30, 29, 30, 29, 31}},
	2078: {npt(2021, 4, 14), [12]int{31, 31, 31, 32, 31, 31, 30, 29, 30, 29, 30, 30}},
	2079: {npt(2022, 4, 14), [12]int{31, 31, 32, 31, 31, 31, 30, 29, 30, 29, 30, 30}},
	2080: {npt(2023, 4, 14), [12]int{31, 32, 31, 32, 31, 30, 30, 30, 29, 29, 30, 30}},
	2081: {npt(2024, 4, 13), [12]int{31, 32, 31, 32, 31, 30, 30, 30, 29, 30, 29, 31}},
	2082: {npt(2025, 4, 14), [12]int{31, 31, 32, 31, 31, 31, 30, 29, 30, 29, 30, 30}},
	2083: {npt(2026, 4, 14), [12]int{31, 31, 32, 31, 31, 31, 30, 29, 30, 29, 30, 30}},
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
	return Date{}, fmt.Errorf("bsdate: AD %s is outside the supported BS range (2076–2083); extend the verified calendar table", target.Format("2006-01-02"))
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
