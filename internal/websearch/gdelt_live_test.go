package websearch

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/charbelkassab/pyrite/internal/market"
)

// TestGDELTLive checks the one property that matters against the real index:
// nothing comes back that was published after the simulated day.
//
// The unit tests beside this one prove the request is *built* correctly, which
// is a different claim from the service honouring it. Only a live call
// establishes the second, and getting a backtest's news window wrong is silent
// — plausible headlines from the future read exactly like plausible headlines
// from the past.
//
// It is opt-in because it depends on a third party being reachable, and a test
// that fails when someone's wifi drops teaches people to ignore red builds.
//
//	PYRITE_LIVE_TESTS=1 go test ./internal/websearch/ -run TestGDELTLive -v
func TestGDELTLive(t *testing.T) {
	if os.Getenv("PYRITE_LIVE_TESTS") != "1" {
		t.Skip("set PYRITE_LIVE_TESTS=1 to query the live article index")
	}

	g := NewGDELT()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Each case is a story with a known date, so an empty answer is
	// informative rather than ambiguous.
	cases := []struct {
		day   market.Day
		query string
	}{
		{"2023-03-10", "Silicon Valley Bank"},
		{"2022-11-11", "FTX bankruptcy"},
		{"2024-02-22", "Nvidia earnings"},
	}

	covered, unreachable := 0, 0
	for _, tc := range cases {
		// GDELT rate-limits bursts, and a throttled reply is empty rather
		// than an error — indistinguishable from "nothing was published".
		// Spacing the requests out is what makes an empty answer mean
		// something.
		time.Sleep(6 * time.Second)

		res, err := g.News(ctx, tc.day, tc.query, 5)
		if err != nil {
			// A refused connection is not a wrong answer. Failing here
			// would mean the suite goes red when a third party is having
			// a bad day, which trains people to ignore it.
			unreachable++
			t.Logf("%s %q: unreachable: %v", tc.day, tc.query, err)
			continue
		}
		if len(res) == 0 {
			t.Logf("%s %q: nothing indexed (or throttled)", tc.day, tc.query)
			continue
		}
		covered++

		cutoff := string(tc.day.EndOfDay())
		for _, r := range res {
			if r.Published == "" {
				continue
			}
			// Published is normalised to the same layout as the cutoff, so
			// a string comparison orders them correctly.
			if r.Published > cutoff {
				t.Errorf("%s %q: article published %s is after the simulated day (%q)",
					tc.day, tc.query, r.Published, r.Title)
			}
		}
		t.Logf("%s %q: %d articles, newest %s", tc.day, tc.query, len(res), res[0].Published)
	}

	if covered == 0 {
		t.Skipf("no window returned anything (%d/%d unreachable); "+
			"nothing was verified, but nothing was contradicted either",
			unreachable, len(cases))
	}
}
