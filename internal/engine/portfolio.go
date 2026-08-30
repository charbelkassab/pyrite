package engine

import (
	"fmt"
	"math"
	"sort"

	"github.com/charbelkassab/natural-quant/internal/market"
)

// Side describes the direction of a fill.
type Side string

const (
	SideBuy   Side = "buy"
	SideSell  Side = "sell"
	SideShort Side = "short"
	SideCover Side = "cover"
)

// OrderKind distinguishes how an order's size was expressed.
type OrderKind string

const (
	KindShares   OrderKind = "shares"
	KindNotional OrderKind = "notional"
	KindWeight   OrderKind = "weight"
)

// Order is a request submitted by a strategy on day D, to be executed on the
// next trading session according to the run's fill model.
type Order struct {
	Symbol string    `json:"symbol"`
	Kind   OrderKind `json:"kind"`
	// Shares is signed: positive buys, negative sells/shorts.
	Shares float64 `json:"shares,omitempty"`
	// Notional is a signed dollar amount.
	Notional float64 `json:"notional,omitempty"`
	// Weight is a signed target portfolio weight in [-1, 1] (or beyond when
	// leverage is permitted).
	Weight float64 `json:"weight,omitempty"`
	// TargetWeight marks the order as "move to this weight" rather than
	// "trade this much".
	IsTarget bool `json:"is_target,omitempty"`

	// Limit, when non-zero, rejects fills worse than this price.
	Limit float64 `json:"limit,omitempty"`
	// Reason is free text from the strategy, surfaced in the day detail view.
	Reason string `json:"reason,omitempty"`
	Tag    string `json:"tag,omitempty"`

	// NoFlip prevents the order from carrying the position through zero into
	// the opposite direction. Set for cover(), where overshooting the short
	// quantity should leave the book flat rather than silently long.
	NoFlip bool `json:"no_flip,omitempty"`

	// SubmittedOn is the day the strategy placed the order.
	SubmittedOn market.Day `json:"submitted_on"`
}

// Fill is an executed trade.
type Fill struct {
	Date   market.Day `json:"date"`
	Symbol string     `json:"symbol"`
	Side   Side       `json:"side"`
	Shares float64    `json:"shares"`
	Price  float64    `json:"price"`
	// Value is shares * price, always positive.
	Value      float64 `json:"value"`
	Commission float64 `json:"commission"`
	Slippage   float64 `json:"slippage"`
	// RealizedPnL is non-zero when the fill reduced or closed a position.
	RealizedPnL float64 `json:"realized_pnl"`
	Reason      string  `json:"reason,omitempty"`
	Tag         string  `json:"tag,omitempty"`
}

// Position is an open holding. Shares is negative for a short.
type Position struct {
	Symbol string  `json:"symbol"`
	Shares float64 `json:"shares"`
	// AvgPrice is the average entry price of the currently open quantity.
	AvgPrice float64 `json:"avg_price"`
	// OpenedOn is the day the current (non-flat) position was established.
	OpenedOn market.Day `json:"opened_on"`
	// PeakPrice tracks the best price seen since entry, for trailing stops.
	PeakPrice float64 `json:"peak_price"`
	// TroughPrice is the mirror of PeakPrice for shorts.
	TroughPrice float64 `json:"trough_price"`
}

// IsShort reports whether the position is short.
func (p *Position) IsShort() bool { return p.Shares < 0 }

// Costs configures the friction model.
type Costs struct {
	// CommissionPerShare is charged on every share traded.
	CommissionPerShare float64 `json:"commission_per_share"`
	// CommissionPct is charged as a fraction of notional (e.g. 0.0005).
	CommissionPct float64 `json:"commission_pct"`
	// CommissionMin is a per-order floor.
	CommissionMin float64 `json:"commission_min"`
	// SlippageBps moves the fill price against the trader, in basis points.
	SlippageBps float64 `json:"slippage_bps"`
	// ShortBorrowAnnualPct is the annual borrow cost charged on short value.
	ShortBorrowAnnualPct float64 `json:"short_borrow_annual_pct"`
	// CashAnnualPct is interest earned on idle cash.
	CashAnnualPct float64 `json:"cash_annual_pct"`
}

// DefaultCosts is a deliberately non-zero default. A backtest with zero
// friction flatters high-turnover strategies badly enough to be misleading,
// so natural-quant charges realistic retail costs unless told otherwise.
func DefaultCosts() Costs {
	return Costs{
		CommissionPct:        0.0,
		SlippageBps:          5, // 0.05% — a plausible retail round-trip cost
		ShortBorrowAnnualPct: 0.03,
		CashAnnualPct:        0.0,
	}
}

// Portfolio tracks cash, positions and realised results.
type Portfolio struct {
	Cash      float64
	Positions map[string]*Position
	Costs     Costs
	// AllowFractional permits fractional share quantities. Enabled by
	// default because dollar-denominated strategies ("buy $100 of X") are a
	// primary use case and rounding them to whole shares distorts small
	// accounts badly.
	AllowFractional bool
	// AllowShort permits negative positions.
	AllowShort bool
	// MaxLeverage caps gross exposure as a multiple of equity.
	MaxLeverage float64

	realized   float64
	commission float64
	slippage   float64
	borrowCost float64
}

// NewPortfolio creates a portfolio with the given starting cash.
func NewPortfolio(cash float64, costs Costs) *Portfolio {
	return &Portfolio{
		Cash:            cash,
		Positions:       map[string]*Position{},
		Costs:           costs,
		AllowFractional: true,
		AllowShort:      true,
		MaxLeverage:     1.0,
	}
}

// Position returns the open position for a symbol, or nil.
func (p *Portfolio) Position(symbol string) *Position {
	if pos, ok := p.Positions[symbol]; ok && pos.Shares != 0 {
		return pos
	}
	return nil
}

// MarketValue is the signed value of all open positions at the given prices.
func (p *Portfolio) MarketValue(prices map[string]float64) float64 {
	var total float64
	for sym, pos := range p.Positions {
		if pos.Shares == 0 {
			continue
		}
		if px, ok := prices[sym]; ok {
			total += pos.Shares * px
		}
	}
	return total
}

// Equity is cash plus the market value of open positions.
func (p *Portfolio) Equity(prices map[string]float64) float64 {
	return p.Cash + p.MarketValue(prices)
}

// GrossExposure is the sum of absolute position values.
func (p *Portfolio) GrossExposure(prices map[string]float64) float64 {
	var total float64
	for sym, pos := range p.Positions {
		if px, ok := prices[sym]; ok {
			total += math.Abs(pos.Shares * px)
		}
	}
	return total
}

// Execute applies a signed share quantity at a reference price, returning the
// resulting fill. A nil fill means nothing traded (rejected or zero size).
func (p *Portfolio) Execute(day market.Day, symbol string, shares, refPrice float64, reason, tag string) (*Fill, error) {
	if shares == 0 || refPrice <= 0 || math.IsNaN(shares) || math.IsInf(shares, 0) {
		return nil, nil
	}
	if !p.AllowFractional {
		shares = math.Trunc(shares)
		if shares == 0 {
			return nil, nil
		}
	}

	pos := p.Positions[symbol]
	if pos == nil {
		pos = &Position{Symbol: symbol}
		p.Positions[symbol] = pos
	}

	if !p.AllowShort && pos.Shares+shares < -1e-9 {
		// Clamp a sell to flat rather than rejecting: a strategy saying
		// "sell everything" should not fail because it over-specified.
		shares = -pos.Shares
		if shares == 0 {
			return nil, nil
		}
	}

	// Slippage always moves against the trader.
	slipRate := p.Costs.SlippageBps / 10000.0
	fillPrice := refPrice
	if shares > 0 {
		fillPrice = refPrice * (1 + slipRate)
	} else {
		fillPrice = refPrice * (1 - slipRate)
	}
	if fillPrice <= 0 {
		return nil, fmt.Errorf("non-positive fill price for %s", symbol)
	}

	value := math.Abs(shares) * fillPrice
	commission := math.Abs(shares)*p.Costs.CommissionPerShare + value*p.Costs.CommissionPct
	if commission < p.Costs.CommissionMin {
		commission = p.Costs.CommissionMin
	}
	slipCost := math.Abs(shares) * math.Abs(fillPrice-refPrice)

	// Determine the realised P&L on any quantity that reduces the position.
	realized := 0.0
	oldShares := pos.Shares
	newShares := oldShares + shares

	switch {
	case oldShares == 0 || sameSign(oldShares, shares):
		// Opening or adding: blend the average price.
		totalCost := math.Abs(oldShares)*pos.AvgPrice + math.Abs(shares)*fillPrice
		if math.Abs(newShares) > 0 {
			pos.AvgPrice = totalCost / math.Abs(newShares)
		}
		if oldShares == 0 {
			pos.OpenedOn = day
			pos.PeakPrice = fillPrice
			pos.TroughPrice = fillPrice
		}
	default:
		// Reducing, closing, or flipping.
		closing := math.Min(math.Abs(shares), math.Abs(oldShares))
		if oldShares > 0 {
			realized = closing * (fillPrice - pos.AvgPrice)
		} else {
			realized = closing * (pos.AvgPrice - fillPrice)
		}
		if sameSign(newShares, shares) && newShares != 0 {
			// Flipped through zero: the remainder opens a new position.
			pos.AvgPrice = fillPrice
			pos.OpenedOn = day
			pos.PeakPrice = fillPrice
			pos.TroughPrice = fillPrice
		}
	}

	pos.Shares = newShares
	if math.Abs(pos.Shares) < 1e-9 {
		pos.Shares = 0
		pos.AvgPrice = 0
		pos.OpenedOn = ""
	}

	// Cash: buying spends, selling receives; commission always costs.
	p.Cash -= shares * fillPrice
	p.Cash -= commission
	p.realized += realized
	p.commission += commission
	p.slippage += slipCost

	side := SideBuy
	switch {
	case shares > 0 && oldShares < 0:
		side = SideCover
	case shares > 0:
		side = SideBuy
	case shares < 0 && newShares < 0 && oldShares <= 0:
		side = SideShort
	default:
		side = SideSell
	}

	return &Fill{
		Date:        day,
		Symbol:      symbol,
		Side:        side,
		Shares:      math.Abs(shares),
		Price:       fillPrice,
		Value:       value,
		Commission:  commission,
		Slippage:    slipCost,
		RealizedPnL: realized,
		Reason:      reason,
		Tag:         tag,
	}, nil
}

// AccrueFinancing charges short borrow and credits cash interest for one day.
func (p *Portfolio) AccrueFinancing(prices map[string]float64, tradingDaysPerYear float64) {
	if tradingDaysPerYear <= 0 {
		tradingDaysPerYear = 252
	}
	if p.Costs.ShortBorrowAnnualPct > 0 {
		var shortValue float64
		for sym, pos := range p.Positions {
			if pos.Shares >= 0 {
				continue
			}
			if px, ok := prices[sym]; ok {
				shortValue += math.Abs(pos.Shares) * px
			}
		}
		cost := shortValue * p.Costs.ShortBorrowAnnualPct / tradingDaysPerYear
		p.Cash -= cost
		p.borrowCost += cost
	}
	if p.Costs.CashAnnualPct > 0 && p.Cash > 0 {
		p.Cash += p.Cash * p.Costs.CashAnnualPct / tradingDaysPerYear
	}
}

// UpdateTrailing refreshes peak/trough marks used by trailing stops.
func (p *Portfolio) UpdateTrailing(prices map[string]float64) {
	for sym, pos := range p.Positions {
		if pos.Shares == 0 {
			continue
		}
		px, ok := prices[sym]
		if !ok {
			continue
		}
		if pos.PeakPrice == 0 || px > pos.PeakPrice {
			pos.PeakPrice = px
		}
		if pos.TroughPrice == 0 || px < pos.TroughPrice {
			pos.TroughPrice = px
		}
	}
}

// OpenSymbols lists symbols with a non-zero position, sorted for determinism.
func (p *Portfolio) OpenSymbols() []string {
	out := make([]string, 0, len(p.Positions))
	for sym, pos := range p.Positions {
		if pos.Shares != 0 {
			out = append(out, sym)
		}
	}
	sort.Strings(out)
	return out
}

// Totals reports cumulative cost accounting.
func (p *Portfolio) Totals() (realized, commission, slippage, borrow float64) {
	return p.realized, p.commission, p.slippage, p.borrowCost
}

func sameSign(a, b float64) bool {
	return (a > 0 && b > 0) || (a < 0 && b < 0)
}
