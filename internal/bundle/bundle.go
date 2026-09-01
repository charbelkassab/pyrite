// Package bundle packages a backtest so that somebody else can re-run it.
//
// The engine already stamps every result with a manifest: the data vendor,
// per-symbol coverage, the code hash, the cost model and the seed. That
// records what was used but does not carry it, so "run it yourself with the
// same command" is not a reproduction. Vendors revise adjusted closes without
// telling anyone — this project has measured the same sweep return a
// probability of overfitting of 74%, 80% and 81% across three fetches of the
// same symbols and the same window — and the second person to run the command
// is testing a different dataset while believing otherwise. A bundle carries
// the bars, so the second run reads the first run's data rather than today's
// version of it, and either reproduces the numbers exactly or names the day
// they parted.
//
// # The file
//
// A bundle is a zip. A tar.gz has to be inflated from the front before
// anything inside can be identified, and a single JSON document has to be
// held whole before any of it can be checked. Zip has a central directory, so
// a bundle whose entries are named wrongly or declare an implausible size is
// refused after reading a few hundred bytes, and each entry inflates on its
// own, which is what makes a per-entry cap possible at all. Every one of those
// properties exists to refuse a bad file cheaply, and a bundle is a file from
// a stranger.
//
// # Trust
//
// A bundle carries executable JavaScript and is read as hostile input: entry
// names are checked against a fixed shape before anything is opened, the
// decompressed size is capped so a bomb is refused rather than allocated, and
// the strategy is read by the same checks the compiler applies before the
// sandbox sees it. Refusals say what was refused and why.
package bundle

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
)

// Format is the bundle layout version. A reader refuses a number it does not
// know rather than guessing at the contents.
const Format = 1

// Entry names. The set is closed: anything else in the archive is refused,
// because a bundle carrying something this reader does not understand is not
// the bundle this reader thinks it is.
const (
	manifestEntry = "manifest.json"
	specEntry     = "spec.json"
	codeEntry     = "strategy.js"
	resultEntry   = "result.json"
	barsPrefix    = "bars/"
	refPrefix     = "reference/"
	sharesEntry   = refPrefix + "shares_outstanding.csv"
)

// Limits on what a bundle may expand to.
//
// Generous for a real run — a forty-symbol decade of daily bars is about 25 MB
// of CSV — and small enough that a zip bomb is refused rather than allocated.
// The declared sizes in the central directory are checked first because that
// is free, and then enforced again while reading, because a declared size is
// written by the same author as the payload.
const (
	maxEntries       = 4096
	maxEntryBytes    = 64 << 20
	maxTotalBytes    = 256 << 20
	maxNameLen       = 200
	maxCodeBytes     = 1 << 20
	maxSeries        = 4096
	maxBarsPerSeries = 2_000_000
)

// SeriesEntry describes one bundled price series.
//
// File is where the rows live and Symbol is what they are: the two are kept
// apart on purpose, so that a symbol is never derived from a file name and a
// file is never located by a symbol.
type SeriesEntry struct {
	Symbol string     `json:"symbol"`
	Name   string     `json:"name,omitempty"`
	File   string     `json:"file"`
	Bars   int        `json:"bars"`
	First  market.Day `json:"first_bar"`
	Last   market.Day `json:"last_bar"`
}

// Manifest is what a bundle says about itself.
type Manifest struct {
	Format        int       `json:"format"`
	PyriteVersion string    `json:"pyrite_version"`
	GoVersion     string    `json:"go_version"`
	CreatedAt     time.Time `json:"created_at"`
	// ContentSHA256 covers every entry except this one, which is where it is
	// written. It is a checksum and not a signature: it catches a bundle that
	// has been edited or truncated in transit, and proves nothing about who
	// wrote it.
	ContentSHA256 string `json:"content_sha256"`

	Strategy string          `json:"strategy,omitempty"`
	Start    market.Day      `json:"start"`
	End      market.Day      `json:"end"`
	Interval market.Interval `json:"interval,omitempty"`
	Index    string          `json:"index,omitempty"`
	// DataProvider is the vendor the original run fetched from. Recorded for
	// the reader, not used on re-run: a bundle serves its own bars.
	DataProvider string        `json:"data_provider,omitempty"`
	Series       []SeriesEntry `json:"series"`
	Reference    []string      `json:"reference,omitempty"`
	// Reproducible is what the recorded run claimed for itself. A run that
	// called a model and missed cache was never reproducible, and a bundle
	// cannot make it so.
	Reproducible bool `json:"reproducible"`
	AICallCount  int  `json:"ai_call_count"`
}

// RecordedResult is the outcome the bundle was written from, and the thing a
// re-run is compared against.
//
// The equity curve and the metrics, not the whole engine.Result: the audit
// trail of positions, orders and log lines for every session is the largest
// part of a result and none of it is needed to answer whether two runs
// agree.
type RecordedResult struct {
	Metrics    engine.Metrics       `json:"metrics"`
	Curve      []engine.EquityPoint `json:"curve"`
	TradeStats engine.TradeStats    `json:"trade_stats"`
	// Manifest is the run's own reproducibility manifest, carried unchanged.
	Manifest       engine.Manifest `json:"manifest"`
	Warnings       []string        `json:"warnings,omitempty"`
	StrategyErrors int             `json:"strategy_errors"`
	Fills          int             `json:"fills"`
}

// checkName refuses an entry name that could escape the archive.
//
// This repository has already had a path-traversal bug from trusting a string
// a strategy supplied, so nothing downstream is derived from a name: files are
// located by the name the manifest gives, symbols come from the manifest
// rather than from a file name, and no entry is ever written to disk. This
// check runs anyway, on every name in the archive and on every name the
// manifest points at, because both lists are written by the same untrusted
// author and defence in one place is not defence.
func checkName(name string) error {
	shown := name
	if len(shown) > 80 {
		shown = shown[:80] + "..."
	}
	switch {
	case name == "":
		return errors.New("refusing a bundle entry with an empty name")
	case len(name) > maxNameLen:
		return fmt.Errorf("refusing bundle entry %q: the name is longer than %d bytes", shown, maxNameLen)
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("refusing bundle entry %q: the name contains a null byte", shown)
	case strings.ContainsRune(name, '\\'):
		return fmt.Errorf("refusing bundle entry %q: a backslash is a path separator on Windows, "+
			"so it may not appear in an entry name", shown)
	case strings.HasPrefix(name, "/"), path.IsAbs(name):
		return fmt.Errorf("refusing bundle entry %q: entry names must be relative to the bundle, "+
			"and this one is an absolute path", shown)
	case strings.HasSuffix(name, "/"):
		return fmt.Errorf("refusing bundle entry %q: a bundle holds files, not directories", shown)
	}
	for _, seg := range strings.Split(name, "/") {
		switch {
		case seg == "":
			return fmt.Errorf("refusing bundle entry %q: it has an empty path element", shown)
		case seg == "." || seg == "..":
			return fmt.Errorf("refusing bundle entry %q: %q walks outside the bundle", shown, seg)
		case strings.ContainsRune(seg, ':'):
			return fmt.Errorf("refusing bundle entry %q: a colon names a drive or a stream on Windows, "+
				"so it may not appear in an entry name", shown)
		}
	}
	return nil
}

// allowedName reports whether a name is one this reader knows how to read.
//
// A closed set rather than a deny list. An unexpected entry is not tolerated
// and skipped: it means the file is not the thing being read, and continuing
// would be reading half of somebody else's format.
func allowedName(name string) error {
	switch name {
	case manifestEntry, specEntry, codeEntry, resultEntry:
		return nil
	}
	if strings.HasPrefix(name, barsPrefix) && strings.HasSuffix(name, ".csv") {
		return nil
	}
	if strings.HasPrefix(name, refPrefix) && strings.HasSuffix(name, ".csv") {
		return nil
	}
	return fmt.Errorf("refusing bundle entry %q: it is not part of a pyrite bundle", name)
}

// membershipEntry is where an index's constituent table lives.
func membershipEntry(index string) string {
	return refPrefix + strings.ToLower(strings.TrimSpace(index)) + "_membership.csv"
}
