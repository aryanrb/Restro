package alerts

import (
	"testing"
	"time"
)

// ktmAt is a Kathmandu wall-clock time.
func ktmAt(date, hhmm string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", date+" "+hhmm, ktm)
	if err != nil {
		panic(err)
	}
	return t
}

var (
	day     = &DayHours{Open: "10:00", Close: "22:00"}
	late    = &DayHours{Open: "10:00", Close: "01:00"}
	testWk  = []*DayHours{nil, day, day, day, day, late, late} // 2026-11-24 is a Tuesday
	noHours []*DayHours
)

func TestKtmClock(t *testing.T) {
	c := ktmClock(ktmAt("2026-11-24", "23:50"))
	if c.date != "2026-11-24" || c.minutes != 23*60+50 || c.weekday != 2 {
		t.Fatalf("got %+v", c)
	}
	c = ktmClock(time.Date(2026, 11, 24, 18, 20, 0, 0, time.UTC))
	if c.date != "2026-11-25" || c.minutes != 5 || c.weekday != 3 {
		t.Fatalf("got %+v", c)
	}
}

func TestBusinessDate(t *testing.T) {
	cases := []struct {
		at    time.Time
		hours []*DayHours
		want  string
	}{
		{ktmAt("2026-11-24", "12:00"), testWk, "2026-11-24"},
		{ktmAt("2026-11-25", "00:30"), testWk, "2026-11-24"},
		{ktmAt("2026-11-25", "09:59"), testWk, "2026-11-24"},
		{ktmAt("2026-11-25", "10:00"), testWk, "2026-11-25"},
		{ktmAt("2026-11-25", "00:30"), noHours, "2026-11-25"}, // calendar day without hours
		// Sunday 2026-11-29 is closed: the week's earliest opening (10:00) is the cut-off.
		{ktmAt("2026-11-29", "09:00"), testWk, "2026-11-28"},
		{ktmAt("2026-11-29", "11:00"), testWk, "2026-11-29"},
	}
	for _, c := range cases {
		if got := businessDate(c.at, c.hours); got != c.want {
			t.Errorf("businessDate(%v) = %s, want %s", c.at, got, c.want)
		}
	}
	if got := businessDayStart("2026-11-24", testWk); !got.Equal(ktmAt("2026-11-24", "10:00")) {
		t.Errorf("start = %v", got)
	}
	if got := businessDayStart("2026-11-24", noHours); !got.Equal(ktmAt("2026-11-24", "00:00")) {
		t.Errorf("start without hours = %v", got)
	}
}

func TestWithinOpeningHours(t *testing.T) {
	cases := []struct {
		at    time.Time
		hours []*DayHours
		want  bool
	}{
		{ktmAt("2026-11-24", "12:00"), testWk, true},
		{ktmAt("2026-11-24", "09:31"), testWk, true},
		{ktmAt("2026-11-24", "09:29"), testWk, false},
		{ktmAt("2026-11-24", "22:30"), testWk, true},
		{ktmAt("2026-11-24", "22:31"), testWk, false},
		{ktmAt("2026-11-25", "03:00"), testWk, false},
		// Friday 10:00 → Saturday 01:00
		{ktmAt("2026-11-28", "01:20"), testWk, true},
		{ktmAt("2026-11-28", "01:40"), testWk, false},
		{ktmAt("2026-11-29", "12:00"), testWk, false}, // closed day
		{ktmAt("2026-11-29", "03:00"), noHours, true}, // no hours: always inside
	}
	for _, c := range cases {
		if got := withinOpeningHours(c.at, c.hours, 30); got != c.want {
			t.Errorf("withinOpeningHours(%v) = %v, want %v", c.at, got, c.want)
		}
	}
}

func TestQuietHours(t *testing.T) {
	night := &QuietHours{From: "23:00", To: "07:00"}
	noon := &QuietHours{From: "13:00", To: "15:00"}
	cases := []struct {
		at    time.Time
		quiet *QuietHours
		want  bool
	}{
		{ktmAt("2026-11-24", "23:30"), night, true},
		{ktmAt("2026-11-24", "06:59"), night, true},
		{ktmAt("2026-11-24", "07:00"), night, false},
		{ktmAt("2026-11-24", "12:00"), night, false},
		{ktmAt("2026-11-24", "14:00"), noon, true},
		{ktmAt("2026-11-24", "16:00"), noon, false},
		{ktmAt("2026-11-24", "14:00"), nil, false},
	}
	for _, c := range cases {
		if got := inQuietHours(c.at, c.quiet); got != c.want {
			t.Errorf("inQuietHours(%v) = %v, want %v", c.at, got, c.want)
		}
	}
}

func TestBusinessDayClose(t *testing.T) {
	if got := businessDayClose("2026-11-27", testWk); got == nil || !got.Equal(ktmAt("2026-11-28", "01:00")) {
		t.Errorf("close past midnight = %v", got)
	}
	if got := businessDayClose("2026-11-29", testWk); got != nil {
		t.Errorf("closed day = %v", got)
	}
}
