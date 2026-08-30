package market

import (
	"sort"
	"strings"
)

// Universe is a named, static list of tickers a strategy can trade.
//
// These lists are survivorship-biased: they contain today's members, not
// point-in-time index membership. A backtest that picks from "megacap" in
// 2012 is therefore choosing from companies we now know went on to succeed.
// The UI surfaces this warning; docs/limitations.md explains it in full.
type Universe struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Symbols     []string `json:"symbols"`
}

// Universes are the built-in symbol lists, keyed by short name.
var Universes = map[string]*Universe{
	"megacap": {
		Key:         "megacap",
		Label:       "US mega caps",
		Description: "The largest US listed companies. The default universe for market-cap ranking strategies.",
		Symbols: []string{
			"AAPL", "MSFT", "NVDA", "GOOGL", "AMZN", "META", "BRK-B", "TSLA",
			"AVGO", "LLY", "JPM", "V", "XOM", "WMT", "UNH", "MA", "JNJ", "PG",
			"ORCL", "HD", "CVX", "MRK", "ABBV", "KO", "PEP", "COST", "ADBE",
			"CSCO", "CRM", "NFLX", "AMD", "INTC", "IBM", "GE", "PFE", "T",
			"VZ", "DIS", "BAC", "WFC", "C", "TSM", "PLTR",
		},
	},
	"tech": {
		Key:         "tech",
		Label:       "US technology",
		Description: "Large US technology and semiconductor names.",
		Symbols: []string{
			"AAPL", "MSFT", "NVDA", "GOOGL", "AMZN", "META", "AVGO", "ORCL",
			"CRM", "ADBE", "AMD", "INTC", "CSCO", "IBM", "NFLX", "QCOM", "TXN",
			"AMAT", "MU", "NOW", "PLTR", "TSM",
		},
	},
	"dow": {
		Key:         "dow",
		Label:       "Dow 30",
		Description: "Current members of the Dow Jones Industrial Average.",
		Symbols: []string{
			"AAPL", "AMGN", "AXP", "BA", "CAT", "CRM", "CSCO", "CVX", "DIS",
			"GS", "HD", "HON", "IBM", "JNJ", "JPM", "KO", "MCD", "MMM", "MRK",
			"MSFT", "NKE", "NVDA", "PG", "SHW", "TRV", "UNH", "V", "VZ", "WMT",
			"DOW",
		},
	},
	"faang": {
		Key:         "faang",
		Label:       "Big tech (FAANG+)",
		Description: "The handful of names that drove a decade of index returns.",
		Symbols:     []string{"META", "AAPL", "AMZN", "NFLX", "GOOGL", "MSFT", "NVDA", "TSLA"},
	},
	"sectors": {
		Key:         "sectors",
		Label:       "S&P 500 sector ETFs",
		Description: "The eleven SPDR sector ETFs, for rotation strategies.",
		Symbols: []string{
			"XLK", "XLF", "XLV", "XLY", "XLP", "XLE", "XLI", "XLB", "XLU",
			"XLRE", "XLC",
		},
	},
	"indices": {
		Key:         "indices",
		Label:       "Major indices",
		Description: "Benchmark indices for comparison.",
		Symbols:     []string{"^GSPC", "^IXIC", "^DJI", "^RUT", "^VIX"},
	},
	"etf-core": {
		Key:         "etf-core",
		Label:       "Core ETFs",
		Description: "Broad market, bond, gold and international ETFs for asset allocation.",
		Symbols: []string{
			"SPY", "QQQ", "IWM", "DIA", "VTI", "VEA", "VWO", "AGG", "TLT",
			"IEF", "SHY", "GLD", "SLV", "VNQ", "HYG", "LQD",
		},
	},
	"crypto": {
		Key:         "crypto",
		Label:       "Crypto",
		Description: "Major cryptocurrencies quoted in USD. Note these trade 7 days a week.",
		Symbols:     []string{"BTC-USD", "ETH-USD", "SOL-USD", "XRP-USD", "DOGE-USD"},
	},
	"us-large": {
		Key:   "us-large",
		Label: "US large caps (broad)",
		Description: "A broad list of large and mid cap US companies across every sector. " +
			"Use this when a strategy should choose from the wider market rather than " +
			"only the very largest names.",
		Symbols: usLarge,
	},
}

// usLarge is a broad cross-sector list of liquid US large and mid caps.
//
// It is not an index and makes no claim to be point-in-time: like every list
// here it contains companies that matter today, which is a survivorship bias
// documented in docs/limitations.md. It exists so a strategy can select from
// the wider market instead of the few dozen mega caps.
//
// Note that market-cap ranking only considers symbols present in the bundled
// share-count table, so ranking within this universe silently restricts itself
// to the names that have share data. That is deliberate: guessing a share
// count would corrupt the ranking rather than extend it.
var usLarge = []string{
	// Technology and semiconductors
	"AAPL", "MSFT", "NVDA", "AVGO", "ORCL", "CRM", "ADBE", "AMD", "INTC", "CSCO",
	"IBM", "QCOM", "TXN", "AMAT", "MU", "LRCX", "KLAC", "ADI", "NXPI", "MRVL",
	"SNPS", "CDNS", "NOW", "INTU", "PANW", "FTNT", "CRWD", "DDOG", "SNOW", "TEAM",
	"WDAY", "ADSK", "ANSS", "ROP", "APH", "GLW", "HPQ", "DELL", "HPE", "WDC",
	"STX", "ON", "MCHP", "SWKS", "TER", "ZS", "NET", "MDB", "PLTR", "TSM",
	// Communication and media
	"GOOGL", "META", "NFLX", "DIS", "CMCSA", "T", "VZ", "TMUS", "CHTR", "EA",
	"TTWO", "WBD", "OMC", "IPG", "LYV", "SPOT", "RBLX", "PINS", "SNAP", "UBER",
	// Consumer
	"AMZN", "TSLA", "HD", "MCD", "NKE", "SBUX", "LOW", "TJX", "BKNG", "ABNB",
	"MAR", "HLT", "CMG", "YUM", "DRI", "ROST", "ORLY", "AZO", "LULU", "DPZ",
	"GM", "F", "APTV", "WMT", "COST", "TGT", "DG", "DLTR", "KR", "SYY",
	"PG", "KO", "PEP", "PM", "MO", "MDLZ", "CL", "KMB", "GIS", "K",
	"HSY", "STZ", "KHC", "EL", "CHD", "GPS", "CAG", "CPB",
	// Health care
	"LLY", "UNH", "JNJ", "ABBV", "MRK", "PFE", "TMO", "ABT", "DHR", "AMGN",
	"BMY", "GILD", "VRTX", "REGN", "ISRG", "SYK", "BSX", "MDT", "BDX", "ZTS",
	"CI", "ELV", "CVS", "HCA", "MCK", "COR", "IQV", "A", "EW", "IDXX",
	"MRNA", "BIIB", "DXCM", "WST", "RMD", "HOLX", "BAX", "ZBH",
	// Financials
	"BRK-B", "JPM", "V", "MA", "BAC", "WFC", "GS", "MS", "C", "SCHW",
	"BLK", "SPGI", "AXP", "PGR", "CB", "MMC", "AON", "ICE", "CME", "COF",
	"USB", "PNC", "TFC", "BK", "TRV", "ALL", "AIG", "MET", "PRU", "AFL",
	"DFS", "SYF", "FIS", "FISV", "GPN", "PYPL", "COIN", "HOOD", "NDAQ", "MSCI",
	// Industrials and transport
	"GE", "CAT", "HON", "RTX", "BA", "LMT", "NOC", "GD", "DE", "UNP",
	"UPS", "FDX", "CSX", "NSC", "EMR", "ETN", "ITW", "PH", "CMI", "PCAR",
	"MMM", "JCI", "CARR", "OTIS", "TT", "AME", "ROK", "DOV", "FAST", "GWW",
	"URI", "WM", "RSG", "LUV", "DAL", "UAL", "AAL",
	// Energy and materials
	"XOM", "CVX", "COP", "EOG", "SLB", "PSX", "VLO", "MPC", "OXY", "WMB",
	"KMI", "OKE", "HAL", "BKR", "DVN", "FANG", "HES", "TRGP",
	"LIN", "APD", "SHW", "ECL", "FCX", "NEM", "NUE", "DOW", "DD", "PPG",
	"VMC", "MLM", "IFF", "ALB", "CF", "MOS",
	// Utilities and real estate
	"NEE", "DUK", "SO", "D", "AEP", "SRE", "EXC", "XEL", "ED", "PEG",
	"WEC", "ES", "EIX", "PCG", "AWK", "DTE", "PPL", "FE",
	"PLD", "AMT", "EQIX", "CCI", "PSA", "SPG", "O", "WELL", "DLR", "VICI",
	"AVB", "EQR", "INVH", "ARE", "SBAC",
}

// UniverseKeys returns the built-in universe keys, sorted.
func UniverseKeys() []string {
	out := make([]string, 0, len(Universes))
	for k := range Universes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IndexUniverses are universes resolved from point-in-time membership rather
// than a fixed list. They cannot be expanded without knowing the date, so
// ResolveUniverse deliberately returns nothing for them and the engine
// resolves them per session instead.
var IndexUniverses = map[string]string{
	"sp500":   "sp500",
	"s&p500":  "sp500",
	"spx":     "sp500",
	"sp-500":  "sp500",
	"s&p 500": "sp500",
}

// IndexUniverse maps a name to its membership table, or "" if the name is not
// a point-in-time index.
func IndexUniverse(name string) string {
	return IndexUniverses[strings.ToLower(strings.TrimSpace(name))]
}

// ResolveUniverse maps a name to a symbol list. It accepts a built-in
// universe key, a comma-separated list of tickers, or a single ticker.
func ResolveUniverse(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if u, ok := Universes[strings.ToLower(name)]; ok {
		return append([]string(nil), u.Symbols...)
	}
	// A point-in-time index has no static answer; the caller must resolve it
	// against a date.
	if IndexUniverse(name) != "" {
		return nil
	}
	parts := strings.Split(name, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := NormalizeSymbol(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// DedupeSymbols normalises and removes duplicates, preserving order.
func DedupeSymbols(syms []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		s = NormalizeSymbol(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
