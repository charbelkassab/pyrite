package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/charbelkassab/pyrite/internal/engine"
)

// selfTestOutput is the machine-readable form of a whole self-test run.
type selfTestOutput struct {
	Cases   []selfTestRow `json:"cases"`
	Passed  int           `json:"passed"`
	Failed  int           `json:"failed"`
	Verdict string        `json:"verdict"`
}

// selfTestRow is one case as the table shows it.
type selfTestRow struct {
	Case string `json:"case"`
	// Defect is what is wrong with the strategy.
	Defect string `json:"defect"`
	// Expected is the finding title the case declares, and Caught the one the
	// critique actually produced.
	Expected string           `json:"expected,omitempty"`
	Caught   string           `json:"caught,omitempty"`
	Severity engine.Severity  `json:"severity,omitempty"`
	Score    int              `json:"trust_score"`
	Pass     bool             `json:"pass"`
	Why      string           `json:"why,omitempty"`
	Findings []engine.Finding `json:"findings,omitempty"`
}

// cmdSelfTest runs the critique against strategies built to be caught.
//
// The critique is the part of this tool that is worth having, and it is the
// part nothing else tests adversarially: every strategy written for a test or
// an example is written to work. This runs it against ten that do not, each
// paired with the finding it must produce, and says which of them landed.
func cmdSelfTest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the outcomes as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	outs, err := engine.RunSelfTest(ctx)
	if err != nil {
		return err
	}

	out := selfTestOutput{Cases: make([]selfTestRow, 0, len(outs))}
	for _, o := range outs {
		row := selfTestRow{
			Case: o.Name, Defect: o.Case.Defect, Expected: o.Case.Expect,
			Score: o.TrustScore, Pass: o.Pass, Why: o.Why,
		}
		if o.Caught != nil {
			row.Caught, row.Severity = o.Caught.Title, o.Caught.Severity
		}
		if !o.Pass {
			row.Findings = o.Findings
		}
		out.Cases = append(out.Cases, row)
		if o.Pass {
			out.Passed++
		} else {
			out.Failed++
		}
	}
	out.Verdict = selfTestVerdict(out)

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	} else {
		printSelfTest(out)
	}

	// A critique that has stopped catching a defect is a broken tool, not a
	// finding about a strategy, so this is a plain failure exit.
	if out.Failed > 0 {
		return exitCode(1)
	}
	return nil
}

// printSelfTest renders the corpus in the same voice as a critique: the claim,
// then the evidence with its numbers in it.
func printSelfTest(out selfTestOutput) {
	fmt.Printf("\nSelf-test — %d strategies built to be wrong, and the finding that must catch each.\n",
		len(out.Cases))
	fmt.Printf("Synthetic and hand-built bars throughout: no network, no vendor, no key.\n\n")

	for _, c := range out.Cases {
		marker := "ok  "
		if !c.Pass {
			marker = "FAIL"
		}
		caught := "nothing caught it"
		switch {
		case c.Caught != "":
			caught = fmt.Sprintf("%s: %s", c.Severity, c.Caught)
		case c.Expected == "" && c.Pass:
			caught = "nothing critical said, which is the whole point"
		}
		fmt.Printf("  %s  %-23s %s\n", marker, c.Case, c.Defect)
		fmt.Printf("        %-56s %3d/100\n", caught, c.Score)
		if !c.Pass {
			fmt.Printf("        %s\n", wrapIndent(c.Why, 64, "        "))
			if len(c.Findings) == 0 {
				fmt.Printf("          the critique said nothing at all\n")
			}
			for _, f := range c.Findings {
				fmt.Printf("          %s: %s\n", f.Severity, f.Title)
			}
		}
	}

	fmt.Printf("\nVerdict\n")
	fmt.Printf("  %d of %d caught, %d missed.\n", out.Passed, len(out.Cases), out.Failed)
	fmt.Printf("  %s\n", wrapIndent(out.Verdict, 74, "  "))
}

// selfTestVerdict is the sentence to read if nothing else is read.
func selfTestVerdict(out selfTestOutput) string {
	if out.Failed > 0 {
		return fmt.Sprintf("%d of %d defects went unreported, or a sound run was called critical. "+
			"Until that is fixed, a clean critique on your own backtest is not evidence of "+
			"anything. Exit status 1.", out.Failed, len(out.Cases))
	}
	return "Every defect in the corpus was named, and the control came back with nothing " +
		"critical against it. That is the critique working on the defects it knows about, " +
		"which is not the same as your backtest being sound."
}
