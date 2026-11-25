package alerts

import "time"

// DayHours is one weekday's opening hours, "HH:MM" Kathmandu time; Close earlier
// than Open means past midnight. A nil *DayHours is a closed day.
type DayHours struct {
	Open  string `json:"open"`
	Close string `json:"close"`
}

// QuietHours: no pushes from From to To (may cross midnight).
type QuietHours struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Nepal has no daylight saving: always UTC+5:45.
var ktm = time.FixedZone("NPT", 5*3600+45*60)

const dayMinutes = 1440

type clock struct {
	date    string
	minutes int
	weekday int // 0 = Sunday
}

func ktmClock(at time.Time) clock {
	t := at.In(ktm)
	return clock{date: t.Format(time.DateOnly), minutes: t.Hour()*60 + t.Minute(), weekday: int(t.Weekday())}
}

// toMinutes parses "HH:MM" (validated on save); -1 when malformed.
func toMinutes(hhmm string) int {
	t, err := time.Parse("15:04", hhmm)
	if err != nil || len(hhmm) != 5 {
		return -1
	}
	return t.Hour()*60 + t.Minute()
}

func midnight(date string) time.Time {
	t, _ := time.ParseInLocation(time.DateOnly, date, ktm)
	return t
}

func addDays(date string, n int) string {
	return midnight(date).AddDate(0, 0, n).Format(time.DateOnly)
}

func weekdayOf(date string) int { return int(midnight(date).Weekday()) }

func hoursOn(hours []*DayHours, weekday int) *DayHours {
	if weekday < len(hours) {
		return hours[weekday]
	}
	return nil
}

// cutoffMinutes: when a business day starting on date begins — its opening time;
// a closed day uses the week's earliest opening; no hours: midnight.
func cutoffMinutes(date string, hours []*DayHours) int {
	if hours == nil {
		return 0
	}
	if h := hoursOn(hours, weekdayOf(date)); h != nil {
		return toMinutes(h.Open)
	}
	earliest := -1
	for _, h := range hours {
		if h != nil && (earliest < 0 || toMinutes(h.Open) < earliest) {
			earliest = toMinutes(h.Open)
		}
	}
	return max(earliest, 0)
}

// businessDate is the business day (opening time to the next opening time, so
// late-night bills count on the right day) an instant belongs to.
func businessDate(at time.Time, hours []*DayHours) string {
	c := ktmClock(at)
	if c.minutes < cutoffMinutes(c.date, hours) {
		return addDays(c.date, -1)
	}
	return c.date
}

// businessDayStart is the instant a business day starts.
func businessDayStart(date string, hours []*DayHours) time.Time {
	return midnight(date).Add(time.Duration(cutoffMinutes(date, hours)) * time.Minute)
}

// businessDayRange is [start, next day's start).
func businessDayRange(date string, hours []*DayHours) (time.Time, time.Time) {
	return businessDayStart(date, hours), businessDayStart(addDays(date, 1), hours)
}

// withinOpeningHours: inside some day's hours widened by grace either side. No
// hours set: always inside.
func withinOpeningHours(at time.Time, hours []*DayHours, grace int) bool {
	if hours == nil {
		return true
	}
	c := ktmClock(at)
	// Yesterday's hours can run past midnight; tomorrow's grace can start before it.
	for _, offset := range []int{-1, 0, 1} {
		h := hoursOn(hours, weekdayOf(addDays(c.date, offset)))
		if h == nil {
			continue
		}
		open, closeAt := toMinutes(h.Open), toMinutes(h.Close)
		if closeAt <= open {
			closeAt += dayMinutes
		}
		start := offset*dayMinutes + open - grace
		end := offset*dayMinutes + closeAt + grace
		if c.minutes >= start && c.minutes <= end {
			return true
		}
	}
	return false
}

func inQuietHours(at time.Time, q *QuietHours) bool {
	if q == nil {
		return false
	}
	m := ktmClock(at).minutes
	from, to := toMinutes(q.From), toMinutes(q.To)
	if from == to {
		return false
	}
	if from < to {
		return m >= from && m < to
	}
	return m >= from || m < to
}

// businessDayClose: when the business day's opening hours end; nil when closed
// that day or no hours are set.
func businessDayClose(date string, hours []*DayHours) *time.Time {
	h := hoursOn(hours, weekdayOf(date))
	if h == nil {
		return nil
	}
	open, closeAt := toMinutes(h.Open), toMinutes(h.Close)
	if closeAt <= open {
		closeAt += dayMinutes
	}
	t := midnight(date).Add(time.Duration(closeAt) * time.Minute)
	return &t
}
