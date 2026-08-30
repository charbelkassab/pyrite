package examples

import (
	"strings"
	"testing"
)

func TestEveryExampleIsBundled(t *testing.T) {
	names := Names()
	if len(names) < 5 {
		t.Fatalf("expected the bundled strategies to be embedded, got %v", names)
	}
	for _, want := range []string{"golden-cross", "sixty-forty", "momentum-rotation"} {
		var found bool
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is missing from %v", want, names)
		}
	}
}

func TestEachExampleCarriesWhatItNeedsToRun(t *testing.T) {
	// A bundled example must run with no flags, which means the file itself
	// has to say what universe and warm-up it needs.
	for _, ex := range All() {
		if ex.Title == "" {
			t.Errorf("%s has no title in its leading comment", ex.Name)
		}
		if len(ex.Universe) == 0 {
			t.Errorf("%s declares no universe directive", ex.Name)
		}
		if ex.Warmup <= 0 {
			t.Errorf("%s declares no warmup directive", ex.Name)
		}
		if !strings.Contains(ex.Code, "function onDay") {
			t.Errorf("%s does not define onDay", ex.Name)
		}
	}
}

func TestTitleIsTheWholeFirstParagraph(t *testing.T) {
	// A one-sentence description that wraps in the source must not be
	// truncated at the wrap.
	ex := parse("t", `// A description that happens to
// wrap onto a second line.
//
// More detail here.
//
// universe: SPY
// warmup: 20

function onDay(ctx) {}
`)
	if ex.Title != "A description that happens to wrap onto a second line." {
		t.Errorf("title: %q", ex.Title)
	}
	if ex.Summary != "More detail here." {
		t.Errorf("summary: %q", ex.Summary)
	}
	if len(ex.Universe) != 1 || ex.Universe[0] != "SPY" {
		t.Errorf("universe: %v", ex.Universe)
	}
	if ex.Warmup != 20 {
		t.Errorf("warmup: %d", ex.Warmup)
	}
}

func TestDirectivesParse(t *testing.T) {
	ex := parse("t", `// Title.
//
// universe: KO, PEP
// benchmarks: SPY,QQQ
// warmup: 160
// allow_short: true
// needs_model: yes

function onDay(ctx) {}
`)
	if len(ex.Universe) != 2 || ex.Universe[1] != "PEP" {
		t.Errorf("universe: %v", ex.Universe)
	}
	if len(ex.Benchmarks) != 2 {
		t.Errorf("benchmarks: %v", ex.Benchmarks)
	}
	if ex.Warmup != 160 || !ex.AllowShort || !ex.NeedsModel {
		t.Errorf("flags wrong: %+v", ex)
	}
}

// Every example should be sweepable, because a bundled strategy is the first
// thing anyone runs and the search is the reason to use this tool over a
// spreadsheet.
func TestExamplesDeclareParameters(t *testing.T) {
	exempt := map[string]string{
		"biggest-company": "the rule has no free parameter: it holds the largest company",
		"news-sentiment":  "its behaviour is the model's answer, not a number",
	}
	for _, ex := range All() {
		if _, ok := exempt[ex.Name]; ok {
			continue
		}
		if !strings.Contains(ex.Code, "ctx.param(") {
			t.Errorf("%s declares no parameters, so `sweep --example %s` finds nothing",
				ex.Name, ex.Name)
		}
	}
}

func TestGetReportsAnUnknownName(t *testing.T) {
	_, err := Get("no-such-example")
	if err == nil {
		t.Fatal("expected an error")
	}
	// The error should list what is available rather than just refusing.
	if !strings.Contains(err.Error(), "golden-cross") {
		t.Errorf("the error should list the alternatives: %v", err)
	}
}

func TestGetToleratesAFileExtension(t *testing.T) {
	if _, err := Get("golden-cross.js"); err != nil {
		t.Errorf("a name with .js should resolve: %v", err)
	}
}

func TestLabelIsShortEnoughForALegend(t *testing.T) {
	// Title is documentation — a full sentence. Using it as a chart legend or
	// a metrics column header produces something nobody can read.
	for _, ex := range All() {
		if ex.Label == "" {
			t.Errorf("%s has no label", ex.Name)
		}
		if len(ex.Label) > 24 {
			t.Errorf("%s label %q is too long for a legend", ex.Name, ex.Label)
		}
		if ex.Label == ex.Title {
			t.Errorf("%s label and title should differ; the title is a sentence", ex.Name)
		}
	}
}

func TestLabelFor(t *testing.T) {
	cases := map[string]string{
		"golden-cross":      "Golden cross",
		"momentum-rotation": "Momentum rotation",
		"sixty-forty":       "Sixty forty",
		"single":            "Single",
		"":                  "",
	}
	for in, want := range cases {
		if got := labelFor(in); got != want {
			t.Errorf("labelFor(%q) = %q, want %q", in, got, want)
		}
	}
}
