package market

import (
	"strings"
	"testing"
)

func testMembership(t *testing.T) *Membership {
	t.Helper()
	m, err := LoadMembership("sp500", "")
	if err != nil {
		t.Fatalf("load membership: %v", err)
	}
	return m
}

func TestBundledMembershipHasRealHistory(t *testing.T) {
	m := testMembership(t)
	if len(m.Symbols()) < 700 {
		t.Fatalf("the table should hold current and former members, got %d", len(m.Symbols()))
	}
	// Roughly 500 members on any date the record covers.
	for _, d := range []Day{"2015-06-30", "2020-12-31", "2023-06-30"} {
		n := len(m.MembersOn(d))
		if n < 450 || n > 520 {
			t.Errorf("membership on %s is %d, which cannot be an S&P 500", d, n)
		}
	}
}

// The three 2023 bank failures are the clearest possible test of whether this
// table is real: a survivorship-biased universe contains none of them.
func TestMembershipHoldsCompaniesThatFailed(t *testing.T) {
	m := testMembership(t)
	cases := []struct {
		symbol       string
		inIndexOn    Day
		outOfIndexOn Day
	}{
		{"SIVB", "2022-06-30", "2023-06-30"}, // Silicon Valley Bank
		{"SBNY", "2022-06-30", "2023-06-30"}, // Signature Bank
		{"FRC", "2022-06-30", "2023-06-30"},  // First Republic
		{"TWTR", "2021-06-30", "2023-01-31"}, // taken private
	}
	for _, c := range cases {
		if !m.WasMember(c.symbol, c.inIndexOn) {
			t.Errorf("%s should be a member on %s", c.symbol, c.inIndexOn)
		}
		if m.WasMember(c.symbol, c.outOfIndexOn) {
			t.Errorf("%s should have left the index by %s", c.symbol, c.outOfIndexOn)
		}
	}
}

func TestMembershipKnowsWhenCompaniesJoined(t *testing.T) {
	m := testMembership(t)
	cases := []struct {
		symbol string
		before Day // not yet a member
		after  Day // a member
	}{
		{"TSLA", "2020-06-30", "2021-06-30"}, // joined December 2020
		{"META", "2013-06-30", "2014-06-30"}, // joined December 2013 as FB
		{"NVDA", "2001-06-30", "2002-06-30"}, // joined November 2001
	}
	for _, c := range cases {
		if m.WasMember(c.symbol, c.before) {
			t.Errorf("%s was not in the index on %s", c.symbol, c.before)
		}
		if !m.WasMember(c.symbol, c.after) {
			t.Errorf("%s should be a member by %s", c.symbol, c.after)
		}
	}
}

func TestEverMembersIsWiderThanAnySnapshot(t *testing.T) {
	m := testMembership(t)
	union := m.EverMembers("2015-01-01", "2024-01-01")
	snapshot := m.MembersOn("2024-01-01")
	if len(union) <= len(snapshot) {
		t.Fatalf("the union over a decade must exceed one snapshot: %d vs %d",
			len(union), len(snapshot))
	}
	// The union must contain names that are no longer members — the whole
	// reason to load it.
	var gone int
	for _, sym := range union {
		if !m.WasMember(sym, "2024-01-01") {
			gone++
		}
	}
	if gone < 50 {
		t.Errorf("only %d of the union had left by 2024; the table looks survivorship-biased", gone)
	}
}

func TestBuildMembershipUndoesChangesInReverse(t *testing.T) {
	current := map[string]Day{"AAA": "2020-01-01", "BBB": ""}
	changes := []IndexChange{
		{Date: "2020-01-01", Added: "AAA", Removed: "CCC"},
	}
	tenures := BuildMembership(current, changes)

	byName := map[string][]Tenure{}
	for _, ten := range tenures {
		byName[ten.Symbol] = append(byName[ten.Symbol], ten)
	}
	// AAA replaced CCC on 2020-01-01: AAA starts then, CCC ends the day before.
	if got := byName["AAA"]; len(got) != 1 || got[0].From != "2020-01-01" || got[0].To != "" {
		t.Errorf("AAA tenure wrong: %+v", got)
	}
	if got := byName["CCC"]; len(got) != 1 || got[0].To != "2019-12-31" {
		t.Errorf("CCC should have left the day before it was replaced: %+v", got)
	}
}

func TestBuildMembershipPrefersTheStatedDateAdded(t *testing.T) {
	// A ticker change means the change log never records the new symbol being
	// added, so only the current table knows when it joined. Preferring the
	// change log's reach here is what put Meta's start date in 1976.
	current := map[string]Day{"META": "2013-12-23"}
	changes := []IndexChange{{Date: "1976-07-01", Added: "XXX", Removed: "YYY"}}
	tenures := BuildMembership(current, changes)
	for _, ten := range tenures {
		if ten.Symbol == "META" && ten.From != "2013-12-23" {
			t.Errorf("META should start at its stated date added, got %v", ten.From)
		}
	}
}

func TestMembershipCSVRoundTrips(t *testing.T) {
	tenures := []Tenure{
		{Symbol: "AAA", From: "2010-01-01", To: "2015-06-30"},
		{Symbol: "AAA", From: "2018-01-01"},
		{Symbol: "BBB", From: "2000-01-01"},
	}
	var sb strings.Builder
	if err := WriteMembershipCSV(&sb, "test", tenures, 3); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := &Membership{Index: "test", tenures: map[string][]Tenure{}}
	if err := m.parse(strings.NewReader(sb.String())); err != nil {
		t.Fatalf("the generated CSV does not parse: %v\n%s", err, sb.String())
	}
	m.finalise()

	// A symbol that left and rejoined has a gap, and the gap must be real.
	if !m.WasMember("AAA", "2012-01-01") {
		t.Error("AAA should be a member during its first tenure")
	}
	if m.WasMember("AAA", "2016-01-01") {
		t.Error("AAA should not be a member in the gap between tenures")
	}
	if !m.WasMember("AAA", "2020-01-01") {
		t.Error("AAA should be a member again after rejoining")
	}
}

func TestIndexUniverseRecognisesAliases(t *testing.T) {
	for _, name := range []string{"sp500", "SP500", "S&P500", "spx", " sp500 "} {
		if IndexUniverse(name) != "sp500" {
			t.Errorf("%q should resolve to sp500, got %q", name, IndexUniverse(name))
		}
	}
	if IndexUniverse("megacap") != "" {
		t.Error("a static universe is not a point-in-time index")
	}
	// And ResolveUniverse must refuse to flatten one.
	if got := ResolveUniverse("sp500"); got != nil {
		t.Errorf("sp500 has no static expansion, got %v", got)
	}
}

func TestCellTickerHandlesWikiMarkup(t *testing.T) {
	cases := map[string]string{
		"|| {{NyseSymbol|MMM}}":     "MMM",
		"{{NasdaqSymbol|AAPL}}":     "AAPL",
		"BRK.B":                     "BRK.B",
		"| RDDT":                    "RDDT",
		"|| [[Meta Platforms|FB]] ": "FB",
		"":                          "",
	}
	for in, want := range cases {
		if got := cellTicker(in); got != want {
			t.Errorf("cellTicker(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWikiDateHandlesTableFormats(t *testing.T) {
	cases := map[string]Day{
		"August 18, 2026":                     "2026-08-18",
		"| August 5, 2026":                    "2026-08-05",
		"2026-08-18":                          "2026-08-18",
		"August 18, 2026<ref>something</ref>": "2026-08-18",
	}
	for in, want := range cases {
		got, err := wikiDate(in)
		if err != nil {
			t.Errorf("wikiDate(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("wikiDate(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := wikiDate("not a date"); err == nil {
		t.Error("garbage should be rejected")
	}
}
