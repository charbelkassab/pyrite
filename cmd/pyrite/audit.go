package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/charbelkassab/pyrite/internal/market"
)

// auditOutput is the machine-readable form of a whole audit run.
type auditOutput struct {
	Provider string          `json:"provider,omitempty"`
	CSVDir   string          `json:"csv_dir,omitempty"`
	From     market.Day      `json:"from,omitempty"`
	To       market.Day      `json:"to,omitempty"`
	Reports  []market.Report `json:"reports"`
	Critical int             `json:"critical"`
	Warnings int             `json:"warnings"`
	Notes    int             `json:"notes"`
	// Unavailable maps a symbol to why it could not be audited at all,
	// which is itself a data-quality answer.
	Unavailable map[string]string `json:"unavailable,omitempty"`
	Verdict     string            `json:"verdict"`
}

// cmdAudit inspects price data before anyone builds a conclusion on it.
//
// Everything the rest of this tool computes is downstream of the price
// series, so every criticism it makes of a strategy rests on the data being
// what it claims to be. This is that assumption made checkable.
func cmdAudit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	from := fs.String("from", "", "audit bars from this date, YYYY-MM-DD")
	to := fs.String("to", "", "audit bars up to this date, YYYY-MM-DD")
	universe := fs.String("universe", "", "audit a named universe, or sp500 for index membership")
	csvDir := fs.String("csv-dir", "", "audit every CSV in this directory instead of the configured provider")
	offline := fs.Bool("offline", false, "use synthetic data and disable network access")
	asJSON := fs.Bool("json", false, "print the reports as JSON")

	// Symbols are written before the flags, the way people type them.
	positional, flagArgs := splitPromptAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	symbols := market.DedupeSymbols(append(strings.Fields(positional), fs.Args()...))

	out := auditOutput{Unavailable: map[string]string{}, CSVDir: *csvDir}
	if *from != "" {
		d, err := market.ParseDay(*from)
		if err != nil {
			return err
		}
		out.From = d
	}
	if *to != "" {
		d, err := market.ParseDay(*to)
		if err != nil {
			return err
		}
		out.To = d
	} else {
		out.To = market.NewDay(time.Now())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	if *csvDir != "" {
		out.Reports, err = auditCSVDir(*csvDir, symbols, out.From, out.To, out.Unavailable)
	} else {
		out.Reports, err = auditProvider(ctx, fs, offline, symbols, *universe, &out)
	}
	if err != nil {
		return err
	}

	for _, r := range out.Reports {
		out.Critical += r.Count(market.SeverityCritical)
		out.Warnings += r.Count(market.SeverityWarning)
		out.Notes += r.Count(market.SeverityNote)
	}
	out.Verdict = auditVerdict(out)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	} else {
		printAudit(out)
	}

	// A pipeline needs to be able to stop on this, and it must not be
	// confused with the command itself failing.
	if out.Critical > 0 {
		return exitCode(2)
	}
	return nil
}

// auditProvider audits symbols served by the configured data provider.
func auditProvider(ctx context.Context, fs *flag.FlagSet, offline *bool,
	symbols []string, universe string, out *auditOutput) ([]market.Report, error) {

	a, err := newApp(fs, offline)
	if err != nil {
		return nil, err
	}
	out.Provider = a.Store.ProviderName()

	if universe != "" {
		if index := market.IndexUniverse(universe); index != "" {
			m, err := a.Store.Membership(index)
			if err != nil {
				return nil, fmt.Errorf("load %s membership: %w", index, err)
			}
			// Every name that ever held membership in the window, including
			// the ones later dropped: those are the files most likely to be
			// broken, and the ones a survivorship-biased list never audits.
			symbols = market.DedupeSymbols(append(symbols, m.EverMembers(auditFrom(out.From), out.To)...))
		} else {
			symbols = market.DedupeSymbols(append(symbols, market.ResolveUniverse(universe)...))
		}
	}
	if len(symbols) == 0 {
		return nil, fmt.Errorf("nothing to audit: name some symbols, or pass --universe or --csv-dir.\n" +
			"  pyrite audit AAPL MSFT SPY\n" +
			"  pyrite audit --universe sp500\n" +
			"  pyrite audit --csv-dir ./vendor-export")
	}

	fmt.Fprintf(os.Stderr, "loading %d symbols from %s...\n", len(symbols), a.Store.ProviderName())
	series, errs := a.Store.GetMany(ctx, symbols, auditFrom(out.From), out.To.EndOfDay())
	if len(series) == 0 {
		var first string
		for _, err := range errs {
			first = err.Error()
			break
		}
		return nil, fmt.Errorf("no data for any of the %d symbols requested (%s)", len(symbols), first)
	}
	for sym, err := range errs {
		out.Unavailable[sym] = truncate(strings.ReplaceAll(err.Error(), "\n", " "), 120)
	}

	reports := make([]market.Report, 0, len(series))
	for _, sym := range symbols {
		ser, ok := series[sym]
		if !ok || ser == nil {
			continue
		}
		reports = append(reports, market.Audit(windowed(sym, ser.Range(out.From, out.To.EndOfDay()))))
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].Symbol < reports[j].Symbol })
	return reports, nil
}

// auditCSVDir audits the files in a directory as they were written.
//
// This path deliberately does not go through the loader. The loader sorts and
// de-duplicates as it builds a series, so two contradictory rows for one
// session are resolved and forgotten before anything can look at them — and
// a user pointing at their own vendor export is exactly who needs to know.
func auditCSVDir(dir string, only []string, from, to market.Day, unavailable map[string]string) ([]market.Report, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read CSV directory %s: %w", dir, err)
	}
	want := map[string]bool{}
	for _, s := range only {
		want[s] = true
	}

	var reports []market.Report
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".csv") {
			continue
		}
		sym := market.NormalizeSymbol(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
		if len(want) > 0 && !want[sym] {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := os.Open(path)
		if err != nil {
			unavailable[sym] = fmt.Sprintf("could not open %s: %v", path, err)
			continue
		}
		bars, err := market.ReadBarsCSV(f)
		f.Close()
		if err != nil {
			unavailable[sym] = fmt.Sprintf("could not parse %s: %v", path, err)
			continue
		}
		if len(bars) == 0 {
			unavailable[sym] = fmt.Sprintf("%s holds no usable rows", path)
			continue
		}
		reports = append(reports, market.Audit(windowed(sym, clipBars(bars, from, to.EndOfDay()))))
	}
	if len(reports) == 0 && len(unavailable) == 0 {
		return nil, fmt.Errorf("no CSV files in %s to audit", dir)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].Symbol < reports[j].Symbol })
	return reports, nil
}

// auditFrom bounds a full-history request so the provider gets a real range.
func auditFrom(from market.Day) market.Day {
	if from == "" {
		return "1970-01-02"
	}
	return from
}

// windowed wraps bars as a series without rebuilding the index, which the
// auditor does not use and which would de-duplicate what it is looking for.
func windowed(symbol string, bars []market.Bar) *market.Series {
	return &market.Series{Symbol: symbol, Bars: bars}
}

// clipBars restricts already-sorted bars to [from, to].
func clipBars(bars []market.Bar, from, to market.Day) []market.Bar {
	out := bars
	if from != "" {
		i := 0
		for i < len(out) && out[i].Date < from {
			i++
		}
		out = out[i:]
	}
	if to != "" {
		j := len(out)
		for j > 0 && out[j-1].Date > to {
			j--
		}
		out = out[:j]
	}
	return out
}

// printAudit renders the reports in the same voice as a critique: the claim,
// then the evidence with its numbers in it.
func printAudit(out auditOutput) {
	source := out.Provider
	if out.CSVDir != "" {
		source = out.CSVDir
	}
	window := "all available history"
	if out.From != "" {
		window = fmt.Sprintf("%s to %s", out.From, out.To)
	}
	fmt.Printf("\nData audit — %s, %s\n", source, window)

	for _, r := range out.Reports {
		fmt.Printf("\n%-8s %d bars", r.Symbol, r.Bars)
		if r.Bars > 0 {
			fmt.Printf("   %s to %s", r.First, r.Last)
		}
		if r.Sessions > 0 {
			fmt.Printf("   %d sessions expected", r.Sessions)
		}
		fmt.Println()
		if len(r.Findings) == 0 {
			fmt.Printf("  clean %s\n", wrapIndent(r.Verdict, 68, "        "))
			continue
		}
		for _, f := range r.Findings {
			marker := "note "
			switch f.Severity {
			case market.SeverityCritical:
				marker = "STOP "
			case market.SeverityWarning:
				marker = "warn "
			}
			fmt.Printf("  %s%s\n", marker, f.Title)
			fmt.Printf("        %s\n", wrapIndent(f.Detail, 68, "        "))
			if line := datesLine(f); line != "" {
				fmt.Printf("        %s\n", wrapIndent(line, 68, "        "))
			}
		}
	}

	if len(out.Unavailable) > 0 {
		fmt.Printf("\nNot audited\n")
		syms := make([]string, 0, len(out.Unavailable))
		for sym := range out.Unavailable {
			syms = append(syms, sym)
		}
		sort.Strings(syms)
		for i, sym := range syms {
			if i >= 10 {
				fmt.Printf("  ... and %d more\n", len(syms)-10)
				break
			}
			fmt.Printf("  %-8s %s\n", sym, truncate(out.Unavailable[sym], 60))
		}
	}

	noun := "symbols"
	if len(out.Reports) == 1 {
		noun = "symbol"
	}
	fmt.Printf("\nVerdict\n")
	fmt.Printf("  %d %s, %d critical, %d warnings, %d notes.\n",
		len(out.Reports), noun, out.Critical, out.Warnings, out.Notes)
	fmt.Printf("  %s\n", wrapIndent(out.Verdict, 74, "  "))
}

// datesLine names the specific bars behind a finding, because "17 sessions"
// is a claim and a list of dates is something a reader can go and check.
func datesLine(f market.Finding) string {
	if len(f.Dates) == 0 {
		return ""
	}
	parts := make([]string, 0, len(f.Dates))
	for _, d := range f.Dates {
		parts = append(parts, string(d))
	}
	line := "dates: " + strings.Join(parts, ", ")
	if f.Count > len(f.Dates) {
		line += fmt.Sprintf(" (+%d more)", f.Count-len(f.Dates))
	}
	return line
}

// auditVerdict is the sentence to read if nothing else is read.
func auditVerdict(out auditOutput) string {
	if len(out.Reports) == 0 {
		return "nothing was audited, so nothing is known about this data."
	}
	var bad int
	for _, r := range out.Reports {
		if r.HasCritical() {
			bad++
		}
	}
	switch {
	case bad > 0:
		carry := "carry"
		if bad == 1 {
			carry = "carries"
		}
		return fmt.Sprintf("%d of %d symbols %s a defect that a backtest would trade against "+
			"rather than around. Fix the data before reading any result computed from it. "+
			"Exit status 2.", bad, len(out.Reports), carry)
	case out.Warnings > 0:
		return fmt.Sprintf("Nothing disqualifying, but %d warnings stand. Each one moves a "+
			"statistic in a knowable direction, so read the result knowing which.", out.Warnings)
	case out.Notes > 0:
		return fmt.Sprintf("No defect found, and %d things worth a look. An audit finds what it "+
			"checks for, which is not the same as the data being right.", out.Notes)
	}
	return "No defect found by any check here. That is the absence of evidence against " +
		"this data, not evidence for it."
}
