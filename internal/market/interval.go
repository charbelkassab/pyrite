package market

import (
	"fmt"
	"strings"
	"time"
)

// Interval is a bar size.
//
// It is a string rather than a duration because a month is not a fixed number
// of hours, and because it travels through JSON, cache paths and the strategy
// API where "5m" is what a person writes.
type Interval string

const (
	Interval1m  Interval = "1m"
	Interval5m  Interval = "5m"
	Interval15m Interval = "15m"
	Interval30m Interval = "30m"
	Interval1h  Interval = "1h"
	Interval1d  Interval = "1d"
	Interval1wk Interval = "1wk"
	Interval1mo Interval = "1mo"
)

// DefaultInterval is what a run uses when nothing says otherwise.
const DefaultInterval = Interval1d

// intervalSpec carries what the rest of the system needs to know about a size.
type intervalSpec struct {
	// duration is the nominal span of one bar. For a month it is an average,
	// which is only used for ordering and for warm-up estimates.
	duration time.Duration
	// fixedPerYear is the annualisation factor for a size that does not
	// depend on how long a session is or how many of them a year holds:
	// there are 52 weeks and 12 months in a year on every calendar. Zero
	// means the factor is derived from the calendar instead.
	fixedPerYear float64
	// intraday marks a size where one calendar day holds several bars.
	intraday bool
	// aliases are other spellings accepted from a user.
	aliases []string
}

var intervals = map[Interval]intervalSpec{
	Interval1m:  {duration: time.Minute, intraday: true, aliases: []string{"1min", "minute", "m1"}},
	Interval5m:  {duration: 5 * time.Minute, intraday: true, aliases: []string{"5min", "m5"}},
	Interval15m: {duration: 15 * time.Minute, intraday: true, aliases: []string{"15min", "m15"}},
	Interval30m: {duration: 30 * time.Minute, intraday: true, aliases: []string{"30min", "m30"}},
	Interval1h:  {duration: time.Hour, intraday: true, aliases: []string{"60m", "hour", "hourly", "h1"}},
	Interval1d:  {duration: 24 * time.Hour, aliases: []string{"d", "day", "daily", "1day"}},
	Interval1wk: {duration: 7 * 24 * time.Hour, fixedPerYear: 52, aliases: []string{"w", "1w", "week", "weekly"}},
	Interval1mo: {duration: 30 * 24 * time.Hour, fixedPerYear: 12, aliases: []string{"mo", "1month", "month", "monthly"}},
}

// ParseInterval resolves a user-supplied bar size.
func ParseInterval(s string) (Interval, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return DefaultInterval, nil
	}
	if _, ok := intervals[Interval(v)]; ok {
		return Interval(v), nil
	}
	for name, spec := range intervals {
		for _, a := range spec.aliases {
			if a == v {
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("unknown bar size %q (want one of %s)", s, strings.Join(IntervalNames(), ", "))
}

// IntervalNames lists the supported sizes, coarsest last.
func IntervalNames() []string {
	return []string{"1m", "5m", "15m", "30m", "1h", "1d", "1wk", "1mo"}
}

// Valid reports whether the interval is one this package understands.
func (i Interval) Valid() bool {
	_, ok := intervals[i]
	return ok
}

// Duration is the nominal span of one bar.
func (i Interval) Duration() time.Duration {
	if s, ok := intervals[i]; ok {
		return s.duration
	}
	return 24 * time.Hour
}

// PeriodsPerYear is the annualisation factor for statistics computed on bars
// of this size, on the US equity calendar.
//
// Getting this wrong is not cosmetic: a Sharpe ratio computed on 1-minute bars
// and annualised as though they were daily is out by a factor of about twenty,
// and it is out in the flattering direction.
func (i Interval) PeriodsPerYear() float64 {
	return i.PeriodsPerYearOn(CalendarUSEquity)
}

// PeriodsPerYearOn is the annualisation factor for bars of this size on a
// given trading calendar.
//
// For intraday sizes it is sessions times bars per session, not calendar time:
// a strategy is not exposed overnight the way it is during the session, and
// scaling by wall-clock hours would overstate volatility badly. On a market
// that never closes the two coincide, because the session is the day.
func (i Interval) PeriodsPerYearOn(c Calendar) float64 {
	s, ok := intervals[i]
	if !ok {
		return c.SessionsPerYear()
	}
	if s.fixedPerYear > 0 {
		return s.fixedPerYear
	}
	if !s.intraday {
		return c.SessionsPerYear()
	}
	mins := float64(s.duration / time.Minute)
	if mins < 1 {
		mins = 1
	}
	return c.SessionsPerYear() * c.MinutesPerSession() / mins
}

// BarsPerSession is how many bars of this size fit in one session of a
// calendar. It is 1 for daily bars and less than 1 for coarser ones.
func (i Interval) BarsPerSession(c Calendar) float64 {
	return i.PeriodsPerYearOn(c) / c.SessionsPerYear()
}

// Intraday reports whether one calendar day holds several bars of this size.
func (i Interval) Intraday() bool {
	if s, ok := intervals[i]; ok {
		return s.intraday
	}
	return false
}

// Coarser reports whether i is a longer bar than other.
func (i Interval) Coarser(other Interval) bool {
	return i.Duration() > other.Duration()
}

// String makes Interval printable.
func (i Interval) String() string { return string(i) }

// Resample aggregates bars up to a coarser size.
//
// This is what makes "weekly signal, daily execution" or "daily trend filter,
// 5-minute entries" expressible: the run happens at the fine size, and a
// strategy asks for the coarse view when it needs one.
//
// It only ever aggregates. Producing finer bars than the input would mean
// inventing prices, and returning nil is the honest answer.
func Resample(s *Series, iv Interval) *Series {
	if s == nil || len(s.Bars) == 0 || !iv.Valid() {
		return s
	}

	out := make([]Bar, 0, len(s.Bars))
	var cur Bar
	var curKey string
	var have bool

	for _, b := range s.Bars {
		key := bucketKey(b.Date, iv)
		if !have {
			cur, curKey, have = b, key, true
			cur.Date = bucketStamp(b.Date, iv)
			continue
		}
		if key != curKey {
			out = append(out, cur)
			cur, curKey = b, key
			cur.Date = bucketStamp(b.Date, iv)
			continue
		}
		// Same bucket: the open is the first bar's, the close the last, and
		// the extremes the extremes.
		if b.High > cur.High {
			cur.High = b.High
		}
		if b.Low < cur.Low || cur.Low == 0 {
			cur.Low = b.Low
		}
		cur.Close = b.Close
		cur.AdjClose = b.AdjClose
		cur.Volume += b.Volume
	}
	if have {
		out = append(out, cur)
	}
	res := NewSeries(s.Symbol, out)
	res.Name = s.Name
	return res
}

// bucketKey identifies which coarse bar a fine bar belongs to.
func bucketKey(d Day, iv Interval) string {
	t := d.Time()
	switch iv {
	case Interval1mo:
		return fmt.Sprintf("%d-%02d", t.Year(), int(t.Month()))
	case Interval1wk:
		y, w := t.ISOWeek()
		return fmt.Sprintf("%d-W%02d", y, w)
	case Interval1d:
		return string(d.Date())
	default:
		// Truncate to the interval within the day, so a 1-hour bucket holds
		// every 5-minute bar of that hour.
		mins := t.Hour()*60 + t.Minute()
		size := int(iv.Duration() / time.Minute)
		if size < 1 {
			size = 1
		}
		return fmt.Sprintf("%s-%d", d.Date(), mins/size)
	}
}

// bucketStamp is the timestamp a coarse bar is labelled with: the start of
// its bucket, so ordering is preserved and a bar is never dated later than
// the data it summarises.
func bucketStamp(d Day, iv Interval) Day {
	t := d.Time()
	switch iv {
	case Interval1mo:
		return NewDay(time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC))
	case Interval1wk:
		back := (int(t.Weekday()) + 6) % 7 // ISO weeks start on Monday
		return NewDay(t.AddDate(0, 0, -back))
	case Interval1d:
		return d.Date()
	default:
		size := iv.Duration()
		if size < time.Minute {
			size = time.Minute
		}
		return NewStamp(t.Truncate(size))
	}
}
