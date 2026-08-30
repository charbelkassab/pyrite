package market

import (
	"strings"
	"testing"
)

// One --offline run used to write synthetic bars to market-cache/SPY.json,
// and every later real backtest read them back believing they were the
// market. Namespacing the cache by provider is what stops that, and the
// README recommends trying --offline first, so it would have been the common
// case rather than the rare one.
func TestProviderNamespacesAreDistinct(t *testing.T) {
	cases := map[string]string{
		"yahoo":       "yahoo",
		"synthetic":   "synthetic",
		"yahoo+stooq": "yahoo-stooq",
		"":            "unknown",
	}
	seen := map[string]string{}
	for in, want := range cases {
		got := SafeProviderDir(in)
		if got != want {
			t.Errorf("SafeProviderDir(%q) = %q, want %q", in, got, want)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%q and %q collide on %q", in, prev, got)
		}
		seen[got] = in
	}
	// The critical pair: synthetic and real data must never share a path.
	if SafeProviderDir("synthetic") == SafeProviderDir("yahoo") {
		t.Fatal("synthetic and real data would share a cache directory")
	}
}

func TestSafeProviderDirIsFilesystemSafe(t *testing.T) {
	for _, in := range []string{"a/b", "a\\b", "a b", "a+b", "A..B"} {
		got := SafeProviderDir(in)
		for _, bad := range []string{"/", "\\", " ", ".."} {
			if strings.Contains(got, bad) {
				t.Errorf("SafeProviderDir(%q) = %q, which contains %q", in, got, bad)
			}
		}
	}
}
