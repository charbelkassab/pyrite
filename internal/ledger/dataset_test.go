package ledger

import (
	"testing"

	"github.com/charbelkassab/pyrite/internal/market"
)

func TestDatasetKeyIgnoresSymbolOrderAndCase(t *testing.T) {
	// The whole feature rests on this: a universe typed one way in January
	// and another way in February is one research problem, and a ledger that
	// cannot see that counts every sweep as the first.
	a := DatasetKey(Dataset{
		Symbols: []string{"spy", "QQQ", "iwm"},
		Start:   "2019-01-02", End: "2023-12-29", Interval: market.Interval1d,
	})
	b := DatasetKey(Dataset{
		Symbols: []string{" IWM ", "Spy", "qqq", "SPY"},
		Start:   "2019-01-02", End: "2023-12-29", Interval: market.Interval1d,
	})
	if a != b {
		t.Fatalf("same universe, different key:\n  %s\n  %s", a, b)
	}
	if want := "IWM,QQQ,SPY:2019-01-02:2023-12-29:1d"; a != want {
		t.Errorf("key = %q, want %q", a, want)
	}
}

func TestDatasetKeySeparatesDifferentProblems(t *testing.T) {
	base := Dataset{Symbols: []string{"SPY"}, Start: "2019-01-02", End: "2023-12-29", Interval: market.Interval1d}
	key := DatasetKey(base)

	for name, other := range map[string]Dataset{
		"a later start":      {Symbols: []string{"SPY"}, Start: "2020-01-02", End: "2023-12-29", Interval: market.Interval1d},
		"an earlier end":     {Symbols: []string{"SPY"}, Start: "2019-01-02", End: "2022-12-29", Interval: market.Interval1d},
		"an unset start":     {Symbols: []string{"SPY"}, End: "2023-12-29", Interval: market.Interval1d},
		"another bar size":   {Symbols: []string{"SPY"}, Start: "2019-01-02", End: "2023-12-29", Interval: market.Interval1h},
		"an extra symbol":    {Symbols: []string{"SPY", "TLT"}, Start: "2019-01-02", End: "2023-12-29", Interval: market.Interval1d},
		"an index universe":  {Index: "sp500", Start: "2019-01-02", End: "2023-12-29", Interval: market.Interval1d},
		"no symbols at all":  {Start: "2019-01-02", End: "2023-12-29", Interval: market.Interval1d},
		"an unset bar size":  {Symbols: []string{"SPY"}, Start: "2019-01-02", End: "2023-12-29"},
		"a different symbol": {Symbols: []string{"QQQ"}, Start: "2019-01-02", End: "2023-12-29", Interval: market.Interval1d},
	} {
		got := DatasetKey(other)
		// An unset bar size means daily, which is the same problem.
		if name == "an unset bar size" {
			if got != key {
				t.Errorf("%s should default to daily: %q", name, got)
			}
			continue
		}
		if got == key {
			t.Errorf("%s should be a different dataset, got %q", name, got)
		}
	}
}

func TestDatasetKeyIndexIsCaseInsensitive(t *testing.T) {
	a := DatasetKey(Dataset{Index: "SP500", Start: "2019-01-02", End: "2023-12-29"})
	b := DatasetKey(Dataset{Index: " sp500", Start: "2019-01-02", End: "2023-12-29"})
	if a != b {
		t.Fatalf("index keys differ: %q and %q", a, b)
	}
	// An index and a ticker spelled the same must not collide.
	if a == DatasetKey(Dataset{Symbols: []string{"SP500"}, Start: "2019-01-02", End: "2023-12-29"}) {
		t.Error("an index universe and a ticker of the same name share a key")
	}
}
