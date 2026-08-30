package websearch

import "testing"

func TestExtractTickerFindsTheSymbolInFreeText(t *testing.T) {
	// Regression: a strategy asking for "Apple AAPL stock latest headlines"
	// once resolved to "APPLE", which is not a ticker, so the headline feed
	// returned nothing and the caller fell back to a generic web search that
	// produced a landing page instead of news.
	cases := map[string]string{
		"Apple AAPL stock latest headlines": "AAPL",
		"AAPL":                              "AAPL",
		"latest news about NVDA today":      "NVDA",
		"BRK-B earnings":                    "BRK-B",
		"what is happening with BTC-USD":    "BTC-USD",
		"bitcoin":                           "BTC-USD",
		"gold":                              "GOLD", // Barrick Gold, not the metal
		"^VIX spike":                        "^VIX",
	}
	for query, want := range cases {
		if got := extractTicker(query); got != want {
			t.Errorf("extractTicker(%q) = %q, want %q", query, got, want)
		}
	}
}

func TestExtractTickerDeclinesWhenThereIsNoSymbol(t *testing.T) {
	// A prose query has no ticker; returning a bogus one would query a
	// nonexistent feed rather than falling back to a general web search.
	for _, q := range []string{
		"what is the mood of the market today",
		"federal reserve interest rate decision",
	} {
		if got := extractTicker(q); got != "" {
			t.Errorf("extractTicker(%q) = %q, want empty", q, got)
		}
	}
}

func TestCleanHTMLStripsMarkupAndEntities(t *testing.T) {
	in := `<a href="x">Apple &amp; Co.</a>   <b>rises</b>` + "\n" + `10%`
	want := "Apple & Co. rises 10%"
	if got := cleanHTML(in); got != want {
		t.Errorf("cleanHTML: got %q, want %q", got, want)
	}
}

func TestHostOfTrimsWWW(t *testing.T) {
	if got := hostOf("https://www.reuters.com/markets/x"); got != "reuters.com" {
		t.Errorf("hostOf: got %q", got)
	}
	if got := hostOf("::not a url::"); got != "" {
		t.Errorf("an unparseable URL should yield no host, got %q", got)
	}
}
