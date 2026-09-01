package bundle

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
)

// staticProvider serves pre-built bars. No network, no keys, no clock.
type staticProvider struct{ series map[string]*market.Series }

func (p *staticProvider) Name() string { return "hand-built" }

func (p *staticProvider) Fetch(_ context.Context, symbol string, _, _ market.Day) (*market.Series, error) {
	if s, ok := p.series[symbol]; ok {
		return s, nil
	}
	return nil, market.ErrNotFound
}

func (p *staticProvider) Search(context.Context, string) ([]market.Quote, error) { return nil, nil }

// testSeries builds a deterministic price history with enough shape in it for
// a crossing strategy to trade on.
func testSeries(symbol string, n int, base float64) *market.Series {
	bars := make([]market.Bar, 0, n)
	d := market.Day("2020-01-01")
	for i := 0; i < n; i++ {
		p := base + float64(i%40) + float64(i)/10
		bars = append(bars, market.Bar{
			Date: d, Open: p - 0.25, High: p + 1, Low: p - 1,
			Close: p, AdjClose: p, Volume: 1_000_000 + float64(i),
		})
		d = d.Add(1)
	}
	s := market.NewSeries(symbol, bars)
	s.Name = symbol + " test series"
	return s
}

const testCode = `
function onDay(ctx) {
	if (ctx.dayIndex % 50 === 0) ctx.buy("TEST", { pctCash: 0.5 }, "scheduled entry");
	if (ctx.dayIndex % 70 === 0) ctx.sell("TEST", {}, "scheduled exit");
}
`

func testSpec() engine.Spec {
	spec := engine.Spec{
		Name:       "bundle test",
		Code:       testCode,
		Universe:   []string{"TEST"},
		Benchmarks: []string{"BENCH"},
		Start:      "2020-02-01",
		End:        "2021-06-30",
		Warmup:     10,
	}
	spec.ApplyDefaults()
	return spec
}

// runOnce executes the test spec against hand-built bars and returns
// everything an export needs.
func runOnce(t *testing.T, series map[string]*market.Series) (engine.Spec, *engine.Result, map[string]*market.Series) {
	t.Helper()
	fund, err := market.LoadFundamentals("")
	if err != nil {
		t.Fatalf("fundamentals: %v", err)
	}
	store := market.NewStore(&staticProvider{series: series}, nil, fund)
	eng := engine.New(testSpec(), store)
	res, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res.Spec, res, eng.LoadedSeries()
}

func defaultSeries() map[string]*market.Series {
	return map[string]*market.Series{
		"TEST":  testSeries("TEST", 600, 100),
		"BENCH": testSeries("BENCH", 600, 50),
	}
}

// writeTestBundle exports a run and returns the path.
func writeTestBundle(t *testing.T, series map[string]*market.Series) string {
	t.Helper()
	spec, res, loaded := runOnce(t, series)
	fund, err := market.LoadFundamentals("")
	if err != nil {
		t.Fatalf("fundamentals: %v", err)
	}
	path := filepath.Join(t.TempDir(), "run.pyrite")
	if _, err := Write(path, Input{
		Spec: spec, Result: res, Series: loaded, Fundamentals: fund, Version: "test",
	}); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	return path
}

// A bundle is only worth handing to somebody if the second run gets the same
// numbers as the first, to the last bit.
func TestRoundTripReproducesExactly(t *testing.T) {
	path := writeTestBundle(t, defaultSeries())

	b, err := Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	if b.Modified {
		t.Errorf("a freshly written bundle reports itself modified: %s against %s",
			b.Manifest.ContentSHA256, b.ComputedSHA256)
	}

	cmp, err := b.Rerun(context.Background())
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if !cmp.Match {
		t.Fatalf("bundle did not reproduce: %s", cmp.Summary())
	}
	if cmp.Compared == 0 {
		t.Fatal("the comparison walked no sessions, so it proved nothing")
	}

	// Exactly, not nearly: every recorded curve point must come back bit for
	// bit, and a tolerance would hide the vendor revisions this exists for.
	got := cmp.Replayed
	if len(got.Curve) != len(b.Recorded.Curve) {
		t.Fatalf("curve length %d, recorded %d", len(got.Curve), len(b.Recorded.Curve))
	}
	for i := range got.Curve {
		if got.Curve[i] != b.Recorded.Curve[i] {
			t.Fatalf("session %d differs: recorded %+v, replayed %+v",
				i, b.Recorded.Curve[i], got.Curve[i])
		}
	}
	if got.Metrics.EndValue != b.Recorded.Metrics.EndValue ||
		got.Metrics.TotalReturn != b.Recorded.Metrics.TotalReturn ||
		got.Metrics.MaxDrawdown != b.Recorded.Metrics.MaxDrawdown {
		t.Errorf("metrics differ: recorded %+v, replayed %+v", b.Recorded.Metrics, got.Metrics)
	}
}

// The bars have to come out of the bundle, not out of whatever the machine
// running it happens to have cached.
func TestRerunServesOnlyTheBundledBars(t *testing.T) {
	path := writeTestBundle(t, defaultSeries())
	b, err := Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}

	store := b.Store()
	if store.ProviderName() != "bundle" {
		t.Errorf("re-run provider is %q, expected the bundle's own", store.ProviderName())
	}
	if _, err := store.Get(context.Background(), "AAPL", "2020-01-01", "2020-12-31"); err == nil {
		t.Error("the bundle served a symbol it does not carry")
	}
}

// A bundle is a file from a stranger, and the oldest trick in the archive
// format is a name that writes outside the directory it was opened in.
func TestTraversalNamesAreRefused(t *testing.T) {
	for _, name := range []string{"../escape", "/etc/x", "bars/../../x.csv", `bars\..\x.csv`, "C:/x.csv"} {
		t.Run(name, func(t *testing.T) {
			path := repack(t, writeTestBundle(t, defaultSeries()), func(entries map[string][]byte, order []string) []string {
				entries[name] = []byte("date,open,high,low,close,adj_close,volume\n")
				return append(order, name)
			})
			_, err := Open(path)
			if err == nil {
				t.Fatalf("a bundle carrying %q was accepted", name)
			}
			if !strings.Contains(err.Error(), "refusing") {
				t.Errorf("the refusal does not say what it refused: %v", err)
			}
			if !strings.Contains(err.Error(), strconv.Quote(name)) {
				t.Errorf("the refusal does not name the entry it refused: %v", err)
			}
		})
	}
}

// A manifest that points a symbol at a path outside the archive gets the same
// treatment as the archive's own names: the two lists have the same author.
func TestTraversalInTheManifestIsRefused(t *testing.T) {
	path := repack(t, writeTestBundle(t, defaultSeries()), func(entries map[string][]byte, order []string) []string {
		entries[manifestEntry] = bytes.Replace(entries[manifestEntry],
			[]byte(`"file": "bars/`), []byte(`"file": "../../bars/`), 1)
		return order
	})
	_, err := Open(path)
	if err == nil {
		t.Fatal("a manifest pointing outside the bundle was accepted")
	}
	if !strings.Contains(err.Error(), "..") {
		t.Errorf("the refusal does not name the offending path: %v", err)
	}
}

// Anything the reader does not understand is refused rather than skipped: a
// bundle carrying an extra file is not the bundle this reader thinks it is.
func TestUnknownEntriesAreRefused(t *testing.T) {
	path := repack(t, writeTestBundle(t, defaultSeries()), func(entries map[string][]byte, order []string) []string {
		entries["notes.txt"] = []byte("hello")
		return append(order, "notes.txt")
	})
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "notes.txt") {
		t.Fatalf("expected a refusal naming notes.txt, got %v", err)
	}
}

// A file that is not a bundle, a bundle cut in half, and a bundle with its
// middle scrambled all have to fail with something a person can act on.
func TestCorruptBundlesFailClearly(t *testing.T) {
	good := writeTestBundle(t, defaultSeries())
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	cases := map[string][]byte{
		"truncated":  raw[:len(raw)/2],
		"empty":      nil,
		"not-a-zip":  []byte("this is not a zip file, it is a sentence"),
		"scrambled":  scramble(raw),
		"header-cut": raw[:20],
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".pyrite")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			// A panic here would be the real failure; the test asserting an
			// error is what catches one.
			b, err := Open(path)
			if err == nil {
				t.Fatalf("a %s bundle was accepted: %+v", name, b.Manifest)
			}
			if !strings.Contains(err.Error(), "bundle") {
				t.Errorf("the error does not say which file it is about: %v", err)
			}
		})
	}
}

// Editing a price is the failure a bundle exists to catch, and the useful
// report is the date it happened on rather than "did not match".
func TestEditedBarsDivergeOnTheRightDate(t *testing.T) {
	path := repack(t, writeTestBundle(t, defaultSeries()), func(entries map[string][]byte, order []string) []string {
		for name := range entries {
			if strings.HasPrefix(name, barsPrefix) && strings.Contains(name, "TEST") {
				entries[name] = editBar(entries[name], "2020-06-15", 5, "1234.5")
			}
		}
		return order
	})

	b, err := Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	if !b.Modified {
		t.Error("an edited bundle still matches its own content hash")
	}

	cmp, err := b.Rerun(context.Background())
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if cmp.Match {
		t.Fatal("an edited price reproduced exactly, so nothing is being compared")
	}
	if cmp.Divergence == nil {
		t.Fatal("the re-run did not match but named no divergence")
	}
	if cmp.Divergence.Date != "2020-06-15" {
		t.Errorf("diverged on %s, expected the day that was edited: %s",
			cmp.Divergence.Date, cmp.Divergence.String())
	}
	line := cmp.Divergence.String()
	for _, want := range []string{"2020-06-15", "against"} {
		if !strings.Contains(line, want) {
			t.Errorf("the divergence line %q does not carry %q", line, want)
		}
	}
	if len(cmp.Notes) == 0 {
		t.Error("an edited bundle produced no note saying it had been changed")
	}
}

// The hash is what makes "this is the same bundle" checkable, so it has to
// move when anything in the bundle moves, and stay put when nothing does.
func TestContentHashCoversEveryInput(t *testing.T) {
	spec, res, loaded := runOnce(t, defaultSeries())
	dir := t.TempDir()

	write := func(name string, in Input) string {
		t.Helper()
		path := filepath.Join(dir, name+".pyrite")
		if _, err := Write(path, in); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return hashOf(t, path)
	}

	base := write("base", Input{Spec: spec, Result: res, Series: loaded, Version: "test"})
	// The same result twice, so the hash is a function of the contents and
	// not of the clock or of Go's map order.
	if again := write("again", Input{Spec: spec, Result: res, Series: loaded, Version: "test"}); again != base {
		t.Fatalf("the same result hashed to %s and then %s", base, again)
	}

	t.Run("a changed bar", func(t *testing.T) {
		edited := map[string]*market.Series{}
		for sym, s := range loaded {
			edited[sym] = s
		}
		bars := append([]market.Bar(nil), loaded["TEST"].Bars...)
		bars[100].AdjClose += 0.01
		edited["TEST"] = market.NewSeries("TEST", bars)
		if got := write("bar", Input{Spec: spec, Result: res, Series: edited, Version: "test"}); got == base {
			t.Error("changing a price left the content hash alone")
		}
	})

	t.Run("a changed spec", func(t *testing.T) {
		other := spec
		other.InitialCash = 12345
		if got := write("spec", Input{Spec: other, Result: res, Series: loaded, Version: "test"}); got == base {
			t.Error("changing the starting capital left the content hash alone")
		}
	})

	t.Run("changed code", func(t *testing.T) {
		other := spec
		other.Code += "\n// a comment nobody executes\n"
		if got := write("code", Input{Spec: other, Result: res, Series: loaded, Version: "test"}); got == base {
			t.Error("changing the strategy left the content hash alone")
		}
	})

	t.Run("a changed result", func(t *testing.T) {
		other := *res
		other.Metrics.EndValue++
		if got := write("result", Input{Spec: spec, Result: &other, Series: loaded, Version: "test"}); got == base {
			t.Error("changing the recorded result left the content hash alone")
		}
	})
}

// A manifest that contradicts its own payload is caught by the contradiction,
// whether or not the hash was updated to cover the lie.
func TestManifestMustAgreeWithTheBars(t *testing.T) {
	path := repack(t, writeTestBundle(t, defaultSeries()), func(entries map[string][]byte, order []string) []string {
		entries[manifestEntry] = bytes.Replace(entries[manifestEntry],
			[]byte(`"bars": 600`), []byte(`"bars": 599`), 1)
		return order
	})
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "599") {
		t.Fatalf("expected a refusal naming the miscount, got %v", err)
	}
}

// A bundle carries executable JavaScript, and it is read before it is run.
func TestStrategyIsCheckedBeforeItRuns(t *testing.T) {
	path := repack(t, writeTestBundle(t, defaultSeries()), func(entries map[string][]byte, order []string) []string {
		entries[codeEntry] = []byte("function onDay(ctx) { fetch('http://example.com'); }")
		return order
	})
	_, err := Open(path)
	if err == nil {
		t.Fatal("a strategy calling fetch() was accepted")
	}
	if !strings.Contains(err.Error(), "sandbox") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// An entry that lies about how far it expands is what a zip bomb is.
func TestOversizedEntriesAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bomb.pyrite")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create(codeEntry)
	if err != nil {
		t.Fatal(err)
	}
	// Highly compressible, and far past the per-entry cap.
	chunk := bytes.Repeat([]byte("A"), 1<<20)
	for i := 0; i < (maxEntryBytes>>20)+2; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("a bomb was accepted")
	}
	if !strings.Contains(err.Error(), "zip bomb") {
		t.Errorf("the refusal does not say what it thinks this is: %v", err)
	}
}

// A price that is not a number would travel through the whole run and come
// out the far end as a plausible one.
func TestNonFinitePricesAreRefused(t *testing.T) {
	path := repack(t, writeTestBundle(t, defaultSeries()), func(entries map[string][]byte, order []string) []string {
		for name := range entries {
			if strings.HasPrefix(name, barsPrefix) {
				entries[name] = editBar(entries[name], "2020-06-15", 5, "+Inf")
			}
		}
		return order
	})
	_, err := Open(path)
	if err == nil {
		t.Fatal("a bundle with an infinite price was accepted")
	}
	if !strings.Contains(err.Error(), "not a price") {
		t.Errorf("the refusal does not say what is wrong with it: %v", err)
	}
}

// editBar replaces one field of the row for a day, leaving the shape of the
// file alone so the refusal is about the value and not the format.
func editBar(data []byte, day string, field int, value string) []byte {
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, day+",") {
			continue
		}
		f := strings.Split(line, ",")
		f[field] = value
		lines[i] = strings.Join(f, ",")
	}
	return []byte(strings.Join(lines, "\n"))
}

func TestShowWorksWithoutRunning(t *testing.T) {
	b, err := Open(writeTestBundle(t, defaultSeries()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if b.Manifest.Format != Format {
		t.Errorf("format %d, want %d", b.Manifest.Format, Format)
	}
	if len(b.Manifest.Series) != 2 {
		t.Errorf("manifest lists %d series, want TEST and BENCH", len(b.Manifest.Series))
	}
	if b.Spec.Code == "" {
		t.Error("the spec came back without its strategy")
	}
	if b.Recorded.Metrics.TradingDays == 0 {
		t.Error("the recorded result carries no sessions")
	}
	if b.Bytes <= 0 {
		t.Error("the bundle reports no size")
	}
}

// repack rewrites a bundle's entries, which is how a tampered bundle is built
// without a shell.
func repack(t *testing.T, src string, edit func(entries map[string][]byte, order []string) []string) string {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string][]byte{}
	var order []string
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		entries[f.Name] = data
		order = append(order, f.Name)
	}
	zr.Close()

	order = edit(entries, order)

	path := filepath.Join(t.TempDir(), "repacked.pyrite")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, name := range order {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entries[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return path
}

func hashOf(t *testing.T, path string) string {
	t.Helper()
	b, err := Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if b.Modified {
		t.Fatalf("%s does not match its own content hash", path)
	}
	return b.Manifest.ContentSHA256
}

// scramble corrupts the middle of a file, leaving the zip's end record intact
// so the damage is found while reading rather than while opening.
func scramble(raw []byte) []byte {
	out := append([]byte(nil), raw...)
	for i := len(out) / 4; i < len(out)/2; i++ {
		out[i] ^= 0xff
	}
	return out
}
