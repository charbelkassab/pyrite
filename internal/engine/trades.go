package engine

import (
	"math"
	"sort"

	"github.com/charbelkassab/pyrite/internal/market"
)

// Direction is which way a round trip was taken.
type Direction string

const (
	DirLong  Direction = "long"
	DirShort Direction = "short"
)

// Trade is one completed round trip: a quantity opened at one price and closed
// at another.
//
// This is a different unit from a Fill. A fill is an execution; a trade is the
// entry and the exit joined together, which is the only level at which "did
// this idea work" is a meaningful question. Fills are paired FIFO, so one entry
// scaled out across three exits produces three trades.
type Trade struct {
	Symbol    string     `json:"symbol"`
	Direction Direction  `json:"direction"`
	EntryDate market.Day `json:"entry_date"`
	ExitDate  market.Day `json:"exit_date,omitempty"`

	EntryPrice float64 `json:"entry_price"`
	ExitPrice  float64 `json:"exit_price"`
	Shares     float64 `json:"shares"`

	// GrossPnL is before costs; Costs is the commission and slippage of both
	// legs, allocated pro rata by share count; NetPnL is what actually landed
	// in the account.
	GrossPnL float64 `json:"gross_pnl"`
	Costs    float64 `json:"costs"`
	NetPnL   float64 `json:"net_pnl"`
	// ReturnPct is NetPnL over the capital committed at entry.
	ReturnPct float64 `json:"return_pct"`

	BarsHeld int `json:"bars_held"`
	DaysHeld int `json:"days_held"`

	// MAE and MFE are the maximum adverse and favourable excursions: the
	// worst and best the position ever looked while it was open, measured
	// intrabar against the entry price.
	//
	// These are the two numbers no equity curve can show you. A strategy whose
	// losers routinely show a large MFE was right and then gave it back — the
	// exit is the problem, not the entry. A strategy whose winners routinely
	// show a large MAE is being paid for surviving noise, and a tighter stop
	// would have destroyed it.
	MAE    float64 `json:"mae"`
	MFE    float64 `json:"mfe"`
	MAEPct float64 `json:"mae_pct"`
	MFEPct float64 `json:"mfe_pct"`

	EntryReason string `json:"entry_reason,omitempty"`
	ExitReason  string `json:"exit_reason,omitempty"`
	Tag         string `json:"tag,omitempty"`

	// Open marks a position still held at the end of the run. Its exit price
	// is the final mark, and it is excluded from win-rate style statistics
	// because its outcome is not yet known.
	Open bool `json:"open,omitempty"`
}

// lot is an open parcel of shares awaiting an offsetting fill. shares is
// signed — positive for a long parcel, negative for a short one — so the
// direction of the book is carried by the data rather than by a side flag
// that could drift out of step with it.
type lot struct {
	date         market.Day
	price        float64
	shares       float64
	costPerShare float64
	reason       string
	tag          string
}

// signedShares converts a fill's side and absolute quantity into a signed
// change in position.
func signedShares(f Fill) float64 {
	switch f.Side {
	case SideBuy, SideCover:
		return f.Shares
	default:
		return -f.Shares
	}
}

// BuildTrades pairs fills into round trips, FIFO, per symbol.
//
// series is optional; when supplied, each trade is enriched with the maximum
// adverse and favourable excursion observed between entry and exit.
func BuildTrades(fills []Fill, series map[string]*market.Series) []Trade {
	if len(fills) == 0 {
		return nil
	}

	// Fills arrive in chronological order from the engine, but a caller may
	// hand us a filtered slice, so sort defensively. The sort must be stable:
	// two fills on the same day must keep their execution order, or FIFO
	// pairing silently reorders the book.
	ordered := make([]Fill, len(fills))
	copy(ordered, fills)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Date < ordered[j].Date })

	open := map[string][]lot{}
	var trades []Trade

	for _, f := range ordered {
		q := signedShares(f)
		if q == 0 || f.Price <= 0 {
			continue
		}
		costPerShare := 0.0
		if f.Shares > 0 {
			costPerShare = (f.Commission + f.Slippage) / f.Shares
		}

		lots := open[f.Symbol]
		remaining := q

		// A fill that agrees with the open direction adds a parcel; one that
		// opposes it consumes parcels oldest-first. A fill large enough to
		// carry the position through zero does both, which is why this is a
		// loop rather than a branch.
		for remaining != 0 {
			if len(lots) == 0 || sameSign(remaining, openSign(lots)) {
				lots = append(lots, lot{
					date:         f.Date,
					price:        f.Price,
					shares:       remaining,
					costPerShare: costPerShare,
					reason:       f.Reason,
					tag:          f.Tag,
				})
				break
			}

			take := math.Min(math.Abs(remaining), math.Abs(lots[0].shares))
			if take <= 0 {
				break
			}
			trades = append(trades, makeTrade(f.Symbol, lots[0], f, take, costPerShare, lots[0].shares > 0))

			if lots[0].shares > 0 {
				lots[0].shares -= take
			} else {
				lots[0].shares += take
			}
			if math.Abs(lots[0].shares) <= 1e-9 {
				lots = lots[1:]
			}
			if remaining > 0 {
				remaining -= take
			} else {
				remaining += take
			}
			if math.Abs(remaining) <= 1e-9 {
				remaining = 0
			}
		}

		if len(lots) == 0 {
			delete(open, f.Symbol)
		} else {
			open[f.Symbol] = lots
		}
	}

	// Anything still held is reported as an open trade, marked at the last
	// price we have. Dropping these would quietly flatter a strategy that
	// ends the run sitting on a loss it never took.
	for sym, lots := range open {
		var last float64
		var lastDay market.Day
		if s, ok := series[sym]; ok && s != nil {
			if bar, ok2 := s.Last(); ok2 {
				last = bar.AdjClose
				lastDay = bar.Date
			}
		}
		long := openSign(lots) > 0
		for _, l := range lots {
			qty := math.Abs(l.shares)
			t := Trade{
				Symbol:      sym,
				Direction:   directionOf(long),
				EntryDate:   l.date,
				EntryPrice:  l.price,
				ExitPrice:   last,
				Shares:      qty,
				EntryReason: l.reason,
				Tag:         l.tag,
				Open:        true,
			}
			if last > 0 {
				if long {
					t.GrossPnL = qty * (last - l.price)
				} else {
					t.GrossPnL = qty * (l.price - last)
				}
				t.Costs = qty * l.costPerShare
				t.NetPnL = t.GrossPnL - t.Costs
				if base := l.price * qty; base > 0 {
					t.ReturnPct = t.NetPnL / base
				}
				t.DaysHeld = daysBetween(l.date, lastDay)
			}
			trades = append(trades, t)
		}
	}

	if len(series) > 0 {
		for i := range trades {
			enrichExcursion(&trades[i], series[trades[i].Symbol])
		}
	}

	sort.SliceStable(trades, func(i, j int) bool {
		if trades[i].EntryDate != trades[j].EntryDate {
			return trades[i].EntryDate < trades[j].EntryDate
		}
		return trades[i].Symbol < trades[j].Symbol
	})
	return trades
}

// openSign reports whether the open lots for a symbol are long (+1) or short
// (-1). Lots are pushed only when they agree with the existing direction, so
// the oldest one settles it for the whole parcel list.
func openSign(lots []lot) float64 {
	if len(lots) == 0 {
		return 0
	}
	if lots[0].shares < 0 {
		return -1
	}
	return 1
}

func directionOf(long bool) Direction {
	if long {
		return DirLong
	}
	return DirShort
}

func makeTrade(symbol string, l lot, exit Fill, shares, exitCostPerShare float64, long bool) Trade {
	t := Trade{
		Symbol:      symbol,
		Direction:   directionOf(long),
		EntryDate:   l.date,
		ExitDate:    exit.Date,
		EntryPrice:  l.price,
		ExitPrice:   exit.Price,
		Shares:      shares,
		EntryReason: l.reason,
		ExitReason:  exit.Reason,
		Tag:         exit.Tag,
		DaysHeld:    daysBetween(l.date, exit.Date),
	}
	if t.Tag == "" {
		t.Tag = l.tag
	}
	if long {
		t.GrossPnL = shares * (exit.Price - l.price)
	} else {
		t.GrossPnL = shares * (l.price - exit.Price)
	}
	t.Costs = shares * (l.costPerShare + exitCostPerShare)
	t.NetPnL = t.GrossPnL - t.Costs
	if base := l.price * shares; base > 0 {
		t.ReturnPct = t.NetPnL / base
	}
	return t
}

// enrichExcursion walks the bars a trade was open for and records how far the
// position ran against and in favour of the holder.
//
// Highs and lows are used rather than closes: a stop that would have been hit
// intraday is a fact about the trade even if the close recovered, and using
// closes here would under-report MAE by exactly the amount that matters.
func enrichExcursion(t *Trade, s *market.Series) {
	if s == nil || t.EntryPrice <= 0 {
		return
	}
	end := t.ExitDate
	if end == "" {
		if bar, ok := s.Last(); ok {
			end = bar.Date
		}
	}
	bars := s.Range(t.EntryDate, end)
	if len(bars) == 0 {
		return
	}
	t.BarsHeld = len(bars)

	worst, best := math.Inf(1), math.Inf(-1)
	for _, b := range bars {
		sf := b.SplitFactor()
		lo, hi := b.Low*sf, b.High*sf
		if lo <= 0 || hi <= 0 {
			continue
		}
		worst = math.Min(worst, lo)
		best = math.Max(best, hi)
	}
	if math.IsInf(worst, 0) || math.IsInf(best, 0) {
		return
	}

	if t.Direction == DirLong {
		t.MAEPct = math.Min(0, worst/t.EntryPrice-1)
		t.MFEPct = math.Max(0, best/t.EntryPrice-1)
	} else {
		t.MAEPct = math.Min(0, -(best/t.EntryPrice - 1))
		t.MFEPct = math.Max(0, -(worst/t.EntryPrice - 1))
	}
	notional := t.EntryPrice * t.Shares
	t.MAE = t.MAEPct * notional
	t.MFE = t.MFEPct * notional
}

func daysBetween(a, b market.Day) int {
	if a == "" || b == "" {
		return 0
	}
	d := b.Time().Sub(a.Time()).Hours() / 24
	if d < 0 {
		return 0
	}
	return int(math.Round(d))
}

// TradeStats summarises a set of round trips.
//
// Everything here is computed over closed trades only. An open position has no
// outcome yet, and folding a paper gain into a win rate is how a backtest
// congratulates itself for a trade it has not finished.
type TradeStats struct {
	Closed int `json:"closed"`
	Open   int `json:"open"`
	Wins   int `json:"wins"`
	Losses int `json:"losses"`

	WinRate float64 `json:"win_rate"`
	// Avg win and loss are reported both ways: as a fraction of capital
	// committed, which compares across position sizes, and in dollars, which
	// is what the account actually felt.
	AvgWinPct      float64 `json:"avg_win_pct"`
	AvgLossPct     float64 `json:"avg_loss_pct"`
	AvgWinDollars  float64 `json:"avg_win_dollars"`
	AvgLossDollars float64 `json:"avg_loss_dollars"`
	// PayoffRatio is average win over average loss, in dollars.
	PayoffRatio Ratio `json:"payoff_ratio"`
	// Expectancy is the dollars a randomly chosen trade is worth.
	Expectancy    float64 `json:"expectancy"`
	ExpectancyPct float64 `json:"expectancy_pct"`
	ProfitFactor  Ratio   `json:"profit_factor"`

	BestTradePct  float64 `json:"best_trade_pct"`
	WorstTradePct float64 `json:"worst_trade_pct"`
	LargestWin    float64 `json:"largest_win"`
	LargestLoss   float64 `json:"largest_loss"`

	AvgBarsHeld    float64 `json:"avg_bars_held"`
	MedianBarsHeld int     `json:"median_bars_held"`
	MaxBarsHeld    int     `json:"max_bars_held"`
	AvgDaysHeld    float64 `json:"avg_days_held"`

	MaxConsecWins   int `json:"max_consec_wins"`
	MaxConsecLosses int `json:"max_consec_losses"`

	// Excursion analysis. AvgMAEPct is how far the average trade went
	// underwater before it resolved; AvgMFEPct is how far in front it got.
	AvgMAEPct float64 `json:"avg_mae_pct"`
	AvgMFEPct float64 `json:"avg_mfe_pct"`
	// EdgeRatio is mean MFE over mean absolute MAE. Above 1 means trades
	// spend more of their life in profit than in loss, which is the raw
	// signal quality before the exit rule touches it.
	EdgeRatio Ratio `json:"edge_ratio"`
	// GiveBack is the average fraction of a losing trade's peak paper profit
	// that was surrendered, over the losers that had a peak worth the name.
	// Above 1 means the average loser was in profit and finished below its
	// entry. It is the specific, actionable version of "the entries are fine
	// and the exits are late".
	GiveBack float64 `json:"give_back"`
	// GiveBackTrades is how many losing trades the figure was measured over.
	GiveBackTrades int `json:"give_back_trades"`
	// WinnerMAEPct is the average worst excursion of trades that went on to
	// win — in effect, how tight a stop would have to be before it started
	// cutting the winners off.
	WinnerMAEPct float64 `json:"winner_mae_pct"`
}

// ComputeTradeStats aggregates round trips.
func ComputeTradeStats(trades []Trade) TradeStats {
	s := TradeStats{
		PayoffRatio:  Ratio(math.NaN()),
		ProfitFactor: Ratio(math.NaN()),
		EdgeRatio:    Ratio(math.NaN()),
	}

	var grossWin, grossLoss, netTotal, pctTotal float64
	var winPct, lossPct float64
	var barsTotal, daysTotal float64
	var maeTotal, mfeTotal float64
	var winnerMAE float64
	var giveBackNum float64
	var giveBackN int
	var bars []int
	var consecW, consecL int

	for _, t := range trades {
		if t.Open {
			s.Open++
			continue
		}
		s.Closed++
		netTotal += t.NetPnL
		pctTotal += t.ReturnPct
		barsTotal += float64(t.BarsHeld)
		daysTotal += float64(t.DaysHeld)
		maeTotal += t.MAEPct
		mfeTotal += t.MFEPct
		bars = append(bars, t.BarsHeld)

		if t.NetPnL > 0 {
			s.Wins++
			grossWin += t.NetPnL
			winPct += t.ReturnPct
			winnerMAE += t.MAEPct
			consecW++
			consecL = 0
			if consecW > s.MaxConsecWins {
				s.MaxConsecWins = consecW
			}
			if t.NetPnL > s.LargestWin {
				s.LargestWin = t.NetPnL
			}
		} else {
			s.Losses++
			grossLoss += -t.NetPnL
			lossPct += t.ReturnPct
			consecL++
			consecW = 0
			if consecL > s.MaxConsecLosses {
				s.MaxConsecLosses = consecL
			}
			if t.NetPnL < s.LargestLoss {
				s.LargestLoss = t.NetPnL
			}
			// A loser that had been up is a give-back, measured against the
			// peak it reached rather than the entry.
			//
			// The 1% floor is not cosmetic. A trade that never rose more than
			// a few basis points has no profit to have given back, and
			// dividing its loss by that sliver produces figures in the
			// hundreds of percent that say nothing about the exit rule.
			// Clamping at 2 keeps a single catastrophic trade from setting
			// the average on its own.
			if t.MFEPct >= 0.01 {
				giveBackNum += math.Min(2, (t.MFEPct-t.ReturnPct)/t.MFEPct)
				giveBackN++
			}
		}
		if s.Closed == 1 || t.ReturnPct > s.BestTradePct {
			s.BestTradePct = t.ReturnPct
		}
		if s.Closed == 1 || t.ReturnPct < s.WorstTradePct {
			s.WorstTradePct = t.ReturnPct
		}
		if t.BarsHeld > s.MaxBarsHeld {
			s.MaxBarsHeld = t.BarsHeld
		}
	}

	if s.Closed == 0 {
		return s
	}
	n := float64(s.Closed)
	s.WinRate = float64(s.Wins) / n
	s.Expectancy = netTotal / n
	s.ExpectancyPct = pctTotal / n
	s.AvgBarsHeld = barsTotal / n
	s.AvgDaysHeld = daysTotal / n
	s.AvgMAEPct = maeTotal / n
	s.AvgMFEPct = mfeTotal / n

	if s.Wins > 0 {
		s.AvgWinDollars = grossWin / float64(s.Wins)
		s.AvgWinPct = winPct / float64(s.Wins)
		s.WinnerMAEPct = winnerMAE / float64(s.Wins)
	}
	if s.Losses > 0 {
		s.AvgLossDollars = grossLoss / float64(s.Losses)
		s.AvgLossPct = lossPct / float64(s.Losses)
	}
	if s.AvgLossDollars > 0 {
		s.PayoffRatio = Ratio(s.AvgWinDollars / s.AvgLossDollars)
	}
	if grossLoss > 0 {
		s.ProfitFactor = Ratio(grossWin / grossLoss)
	} else if grossWin > 0 {
		s.ProfitFactor = Ratio(math.Inf(1))
	}
	if a := math.Abs(s.AvgMAEPct); a > 0 {
		s.EdgeRatio = Ratio(s.AvgMFEPct / a)
	}
	if giveBackN > 0 {
		s.GiveBack = giveBackNum / float64(giveBackN)
		s.GiveBackTrades = giveBackN
	}
	if len(bars) > 0 {
		sort.Ints(bars)
		s.MedianBarsHeld = bars[len(bars)/2]
	}
	return s
}

// BySymbol groups round trips by symbol, newest grouping first by net P&L.
func BySymbol(trades []Trade) map[string][]Trade {
	out := map[string][]Trade{}
	for _, t := range trades {
		out[t.Symbol] = append(out[t.Symbol], t)
	}
	return out
}
