package market

import (
	"sync"
	"time"
)

// CalendarFrom is the earliest date this US market calendar is trusted from.
//
// The modern NYSE holiday list settled at the start of the 1980s. Before that
// the exchange also shut for Columbus Day, Veterans Day and presidential
// election days, and the rules moved several times. Anything that reasons
// about a missing or an impossible session ignores dates earlier than this
// rather than guess: a calendar that is confidently wrong produces a wall of
// findings about days the market really was shut, and a user who sees that
// once stops reading the output.
const CalendarFrom Day = "1981-01-01"

// IsWeekend reports whether d falls on a Saturday or a Sunday.
func IsWeekend(d Day) bool {
	switch d.Date().Time().Weekday() {
	case time.Saturday, time.Sunday:
		return true
	}
	return false
}

// IsMarketHoliday reports whether the US equity market was shut on d for a
// holiday or an unscheduled closure.
//
// A weekend is not a holiday. The two are kept apart because a bar on each
// means something different: a Saturday print is a broken vendor or a
// timezone bug, while a Thanksgiving print is a vendor padding its calendar.
func IsMarketHoliday(d Day) bool {
	date := d.Date()
	if IsWeekend(date) {
		return false
	}
	return holidays(date.Time().Year())[date]
}

// IsTradingDay reports whether the US equity market held a regular session.
func IsTradingDay(d Day) bool {
	date := d.Date()
	return !IsWeekend(date) && !holidays(date.Time().Year())[date]
}

// TradingDays lists the sessions in [from, to] inclusive.
func TradingDays(from, to Day) []Day {
	var out []Day
	for d := from.Date(); d <= to.Date(); d = d.Add(1) {
		if IsTradingDay(d) {
			out = append(out, d)
		}
	}
	return out
}

var (
	holidayMu    sync.Mutex
	holidayCache = map[int]map[Day]bool{}
)

// holidays returns the closed weekdays in a year, building the set once.
//
// The returned map is never written to after construction, so handing the
// same one to concurrent callers is safe; only the cache itself is locked.
func holidays(year int) map[Day]bool {
	holidayMu.Lock()
	defer holidayMu.Unlock()
	if h, ok := holidayCache[year]; ok {
		return h
	}
	h := buildHolidays(year)
	holidayCache[year] = h
	return h
}

func buildHolidays(year int) map[Day]bool {
	h := map[Day]bool{}
	// observe applies the exchange's weekend rule: a holiday on a Sunday is
	// taken the following Monday, one on a Saturday the preceding Friday. New
	// Year's Day is the exception — when 1 January is a Saturday the exchange
	// trades the last day of the old year as normal, so nothing is observed.
	observe := func(t time.Time) {
		switch t.Weekday() {
		case time.Sunday:
			t = t.AddDate(0, 0, 1)
		case time.Saturday:
			if t.Month() == time.January && t.Day() == 1 {
				return
			}
			t = t.AddDate(0, 0, -1)
		}
		h[NewDay(t)] = true
	}
	fixed := func(m time.Month, d int) time.Time {
		return time.Date(year, m, d, 0, 0, 0, 0, time.UTC)
	}

	observe(fixed(time.January, 1))
	// Martin Luther King Jr Day became an exchange holiday in 1998.
	if year >= 1998 {
		observe(nthWeekday(year, time.January, time.Monday, 3))
	}
	observe(nthWeekday(year, time.February, time.Monday, 3)) // Washington's Birthday
	// Good Friday is always a Friday, so it takes no observance rule.
	h[NewDay(goodFriday(year))] = true
	observe(lastWeekday(year, time.May, time.Monday)) // Memorial Day
	// Juneteenth became a federal holiday, and an exchange one, in 2022.
	if year >= 2022 {
		observe(fixed(time.June, 19))
	}
	observe(fixed(time.July, 4))
	observe(nthWeekday(year, time.September, time.Monday, 1))  // Labor Day
	observe(nthWeekday(year, time.November, time.Thursday, 4)) // Thanksgiving
	observe(fixed(time.December, 25))

	for _, d := range unscheduledClosures[year] {
		h[d] = true
	}
	return h
}

// unscheduledClosures are the days since 1981 the exchange shut outside its
// published holiday list. Without these a correct series looks like it has a
// gap, which is exactly the false positive that makes an auditor useless.
var unscheduledClosures = map[int][]Day{
	1985: {"1985-09-27"},                                           // Hurricane Gloria
	1994: {"1994-04-27"},                                           // Nixon's funeral
	2001: {"2001-09-11", "2001-09-12", "2001-09-13", "2001-09-14"}, // after the attacks
	2004: {"2004-06-11"},                                           // Reagan's funeral
	2007: {"2007-01-02"},                                           // Ford's funeral
	2012: {"2012-10-29", "2012-10-30"},                             // Hurricane Sandy
	2018: {"2018-12-05"},                                           // Bush's funeral
	2025: {"2025-01-09"},                                           // Carter's funeral
}

// nthWeekday returns the nth given weekday of a month, counting from one.
func nthWeekday(year int, m time.Month, wd time.Weekday, n int) time.Time {
	t := time.Date(year, m, 1, 0, 0, 0, 0, time.UTC)
	shift := (int(wd) - int(t.Weekday()) + 7) % 7
	return t.AddDate(0, 0, shift+7*(n-1))
}

// lastWeekday returns the final given weekday of a month.
func lastWeekday(year int, m time.Month, wd time.Weekday) time.Time {
	t := time.Date(year, m+1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	shift := (int(t.Weekday()) - int(wd) + 7) % 7
	return t.AddDate(0, 0, -shift)
}

// goodFriday returns the Friday before Easter Sunday, by the Gregorian
// computus. Easter is the one exchange holiday with neither a fixed date nor
// an nth-weekday rule, so it has to be computed rather than tabulated.
func goodFriday(year int) time.Time {
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	hh := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - hh - k) % 7
	m := (a + 11*hh + 22*l) / 451
	month := (hh + l - 7*m + 114) / 31
	day := ((hh + l - 7*m + 114) % 31) + 1
	easter := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	return easter.AddDate(0, 0, -2)
}
