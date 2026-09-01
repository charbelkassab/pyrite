package market

import (
	"fmt"
	"sort"
	"strings"
)

// Calendar is the set of sessions an instrument actually trades on.
//
// It exists because annualisation is not a constant, and everything here grew
// up assuming it was. A year of US equities holds 252 sessions; a year of
// bitcoin holds 365, and a year of spot FX about 261. Scaling a crypto series
// by 252 leaves its annualised volatility understated by about a sixth and
// its Sharpe scaled by the wrong root, with nothing in the output to say so —
// which is the exact class of quietly wrong number this project exists to
// find, committed by the project itself.
type Calendar string

const (
	// CalendarUSEquity is the NYSE session calendar: weekdays, less the
	// exchange holiday list. The default, and what every figure in this
	// engine meant before there was anything else.
	CalendarUSEquity Calendar = "us_equity"
	// CalendarContinuous never closes. Crypto is the case that matters.
	CalendarContinuous Calendar = "continuous"
	// CalendarFX is the 24/5 interbank week: it runs straight through the US
	// holiday list and stops only at the weekend.
	CalendarFX Calendar = "fx"
)

// tradingSessionsPerYear is the conventional 252.
const tradingSessionsPerYear = 252.0

// continuousSessionsPerYear is every day, including the leap-year quarter.
// Rounding to 365 is the convention and it costs a tenth of a percent on a
// root, which is far below anything this tool would print.
const continuousSessionsPerYear = 365.0

// fxSessionsPerYear is the count of weekdays in a year: 365.25 * 5/7, which
// is 260.9. FX keeps a handful of thin half-days around Christmas and New
// Year rather than closing, so the weekday count is the honest figure.
const fxSessionsPerYear = 261.0

// SessionsPerYear is how many daily bars a year of this calendar holds.
func (c Calendar) SessionsPerYear() float64 {
	switch c {
	case CalendarContinuous:
		return continuousSessionsPerYear
	case CalendarFX:
		return fxSessionsPerYear
	default:
		return tradingSessionsPerYear
	}
}

// MinutesPerSession is how long one session lasts, which is what decides how
// many intraday bars fit inside it.
//
// A market that never closes packs nearly four times as many 5-minute bars
// into a day as the US regular session does, so an intraday Sharpe scaled by
// the equity figure is out by the square root of that.
func (c Calendar) MinutesPerSession() float64 {
	switch c {
	case CalendarContinuous, CalendarFX:
		return 24 * 60
	default:
		return usMinutesPerSession
	}
}

// usMinutesPerSession is 6.5 hours of US regular trading.
const usMinutesPerSession = 390.0

// Valid reports whether the calendar is one this package understands.
func (c Calendar) Valid() bool {
	switch c {
	case CalendarUSEquity, CalendarContinuous, CalendarFX:
		return true
	}
	return false
}

// Label names the calendar for a reader.
func (c Calendar) Label() string {
	switch c {
	case CalendarContinuous:
		return "continuous"
	case CalendarFX:
		return "FX 24/5"
	default:
		return "US equities"
	}
}

// Describe names the calendar with the figure that follows from it, for the
// places where the number is the point.
func (c Calendar) Describe() string {
	return fmt.Sprintf("%s, %.0f sessions a year", c.Label(), c.SessionsPerYear())
}

// String makes Calendar printable.
func (c Calendar) String() string {
	if c == "" {
		return string(CalendarUSEquity)
	}
	return string(c)
}

// Wider reports whether c holds more sessions in a year than other.
func (c Calendar) Wider(other Calendar) bool {
	return c.SessionsPerYear() > other.SessionsPerYear()
}

// ParseCalendar resolves a user-supplied calendar name.
func ParseCalendar(s string) (Calendar, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "":
		return "", nil
	case "us_equity", "us-equity", "equity", "us", "nyse", "252":
		return CalendarUSEquity, nil
	case "continuous", "crypto", "24/7", "247", "365":
		return CalendarContinuous, nil
	case "fx", "forex", "24/5", "245", "261":
		return CalendarFX, nil
	}
	return "", fmt.Errorf("unknown trading calendar %q (want us_equity, continuous or fx)", s)
}

// CalendarNames lists the calendars a user may name.
func CalendarNames() []string {
	return []string{"us_equity", "continuous", "fx"}
}

// minCalendarBars is how much history is needed before the bars themselves
// are better evidence than the ticker. Twenty daily bars span four weekends,
// which is enough for the weekend test to mean something.
const minCalendarBars = 20

// CalendarOf infers which calendar produced a price series.
//
// One question is settled by the bars and one is not. Whether the weekend
// trades is visible in the dates and nothing else explains a Saturday print,
// so the bars overrule the ticker on it. Whether a weekday-only series is an
// exchange listing or spot FX is not visible: FX runs through the US holiday
// list, but so does a vendor that pads its calendar by carrying the previous
// close forward, and the two are indistinguishable from dates alone. Guessing
// there costs more than it saves — an FX session is 24 hours against the
// exchange's six and a half, so a wrong answer multiplies an intraday
// annualisation factor by nearly four — so FX is taken from the ticker or
// from an explicit setting, and never inferred.
func CalendarOf(s *Series) Calendar {
	if s == nil {
		return CalendarUSEquity
	}
	byName := CalendarForSymbol(s.Symbol)
	if len(s.Bars) < minCalendarBars {
		return byName
	}
	if continuousWeekend(len(s.Bars), weekendBars(s.Bars)) {
		return CalendarContinuous
	}
	if byName == CalendarContinuous {
		// The ticker says crypto and the dates say otherwise. The dates win,
		// because a series with no weekend bars cannot be annualised by 365
		// however it is spelled — there are not 365 of them in a year.
		return CalendarUSEquity
	}
	return byName
}

// CalendarForSymbol guesses a calendar from the ticker alone, for when there
// are no bars to judge — a universe resolved before any data is loaded, or a
// series too short to have met a weekend.
func CalendarForSymbol(symbol string) Calendar {
	s := NormalizeSymbol(symbol)
	switch {
	// Crypto pairs are quoted against a currency: BTC-USD, ETH-EUR. A share
	// class suffix is a single letter (BRK-B), so the two do not collide.
	case strings.HasSuffix(s, "-USD"), strings.HasSuffix(s, "-USDT"),
		strings.HasSuffix(s, "-EUR"), strings.HasSuffix(s, "-GBP"):
		return CalendarContinuous
	// Yahoo's FX convention, e.g. EURUSD=X.
	case strings.HasSuffix(s, "=X"):
		return CalendarFX
	}
	return CalendarUSEquity
}

// CalendarForSymbols picks the calendar for a whole universe, and reports
// every distinct calendar it saw.
func CalendarForSymbols(symbols []string) (Calendar, []Calendar) {
	return widest(symbols, CalendarForSymbol)
}

// CalendarForSeries picks the calendar for a loaded universe, from the bars.
func CalendarForSeries(series map[string]*Series) (Calendar, []Calendar) {
	syms := make([]string, 0, len(series))
	for sym := range series {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	return widest(syms, func(sym string) Calendar { return CalendarOf(series[sym]) })
}

// widest resolves a universe to one calendar: the one with the most sessions
// in it.
//
// A run produces one equity point per session in the union of its symbols'
// calendars — TradingCalendar is a union, not an intersection — so a book
// holding SPY and BTC-USD is marked 365 times a year, not 252. The
// annualisation factor describes how often the curve was sampled, not what is
// in it, so the widest calendar present is the arithmetically correct one. A
// dominant calendar chosen by headcount, or a blend weighted by anything,
// would be answering a different question than the one Scale asks.
//
// It is still worth saying out loud, which is what the second return value is
// for: a mixed book marks its equities at a stale price twice a week, and no
// choice of divisor repairs that.
func widest(symbols []string, of func(string) Calendar) (Calendar, []Calendar) {
	best := CalendarUSEquity
	var seen []Calendar
	has := map[Calendar]bool{}
	for _, sym := range symbols {
		c := of(sym)
		if !c.Valid() {
			c = CalendarUSEquity
		}
		if !has[c] {
			has[c] = true
			seen = append(seen, c)
		}
		if c.Wider(best) {
			best = c
		}
	}
	if len(seen) == 0 {
		seen = append(seen, best)
	}
	// A stable order, so two identical runs describe their universe the same
	// way: narrowest first.
	for i := 1; i < len(seen); i++ {
		for j := i; j > 0 && seen[j].SessionsPerYear() < seen[j-1].SessionsPerYear(); j-- {
			seen[j], seen[j-1] = seen[j-1], seen[j]
		}
	}
	return best, seen
}

// weekendBars counts the bars falling on a Saturday or a Sunday.
func weekendBars(bars []Bar) int {
	var n int
	for _, b := range bars {
		if IsWeekend(b.Date) {
			n++
		}
	}
	return n
}

// continuousWeekend reports whether enough bars fall at weekends that the US
// session calendar cannot be what produced them.
//
// This is the single definition of that test. The auditor stands its missing
// session and closed day checks down on it, and the annualisation factor is
// chosen by it; two thresholds that could drift apart would mean a series
// audited as crypto and annualised as equity.
func continuousWeekend(bars, weekend int) bool {
	return bars > 0 && float64(weekend) > continuousWeekendShare*float64(bars)
}
