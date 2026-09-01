package bundle

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
)

// Input is everything a bundle is written from.
type Input struct {
	// Spec is the resolved spec, as the engine ran it rather than as the
	// user typed it: an empty start date has been turned into the day the
	// warm-up ended, and the defaults have been applied.
	Spec   engine.Spec
	Result *engine.Result
	// Series is what the run actually read, from Engine.LoadedSeries.
	Series map[string]*market.Series
	// Fundamentals and Membership are the reference tables the run could have
	// consulted. Both may be nil.
	Fundamentals *market.Fundamentals
	Membership   *market.Membership
	// Version is the pyrite build that produced the run.
	Version string
}

// Write assembles a bundle at path and returns its manifest.
func Write(path string, in Input) (*Manifest, error) {
	if in.Result == nil {
		return nil, fmt.Errorf("write bundle %s: there is no result to bundle", path)
	}
	if len(in.Series) == 0 {
		return nil, fmt.Errorf("write bundle %s: the run read no price data, so there is nothing to carry", path)
	}
	if len(in.Series) > maxSeries {
		return nil, fmt.Errorf("write bundle %s: %d symbols is more than a bundle carries", path, len(in.Series))
	}

	spec := in.Spec
	spec.ApplyDefaults()
	code := strings.TrimSpace(spec.Code)
	if code == "" {
		return nil, fmt.Errorf("write bundle %s: the spec carries no strategy code", path)
	}

	symbols := make([]string, 0, len(in.Series))
	for sym := range in.Series {
		symbols = append(symbols, sym)
	}
	// Sorted so that writing one result twice gives byte-identical archives,
	// which is what makes the content hash a function of the contents rather
	// than of Go's map order.
	sort.Strings(symbols)

	entries := map[string][]byte{}
	man := Manifest{
		Format:        Format,
		PyriteVersion: firstNonEmpty(in.Version, engine.Version),
		GoVersion:     runtime.Version(),
		CreatedAt:     time.Now().UTC(),
		Strategy:      spec.Name,
		Start:         spec.Start,
		End:           spec.End,
		Interval:      spec.Interval,
		Index:         spec.Index,
		DataProvider:  in.Result.Manifest.DataProvider,
		Reproducible:  in.Result.Manifest.Reproducible(),
		AICallCount:   in.Result.Manifest.AICallCount,
	}

	for i, sym := range symbols {
		s := in.Series[sym]
		if s == nil || len(s.Bars) == 0 {
			continue
		}
		file := fmt.Sprintf("%s%03d-%s.csv", barsPrefix, i, fileLabel(sym))
		var buf bytes.Buffer
		if err := writeBars(&buf, s); err != nil {
			return nil, fmt.Errorf("write bundle %s: render bars for %s: %w", path, sym, err)
		}
		entries[file] = buf.Bytes()
		man.Series = append(man.Series, SeriesEntry{
			Symbol: sym, Name: s.Name, File: file, Bars: len(s.Bars),
			First: s.Bars[0].Date, Last: s.Bars[len(s.Bars)-1].Date,
		})
	}
	if len(man.Series) == 0 {
		return nil, fmt.Errorf("write bundle %s: every series the run read was empty", path)
	}

	// The code travels in strategy.js and nowhere else. Two copies of the
	// same JavaScript in one archive would be two things to keep in step, and
	// a bundle that disagreed with itself would have no defensible answer for
	// which copy ran.
	bare := spec
	bare.Code = ""
	specJSON, err := json.MarshalIndent(bare, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("write bundle %s: encode spec: %w", path, err)
	}
	entries[specEntry] = specJSON
	entries[codeEntry] = []byte(code)

	recorded := RecordedResult{
		Metrics:        in.Result.Metrics,
		Curve:          in.Result.Curve,
		TradeStats:     in.Result.TradeStats,
		Manifest:       in.Result.Manifest,
		Warnings:       in.Result.Warnings,
		StrategyErrors: in.Result.StrategyErrors,
		Fills:          len(in.Result.Fills),
	}
	resultJSON, err := json.MarshalIndent(recorded, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("write bundle %s: encode result: %w", path, err)
	}
	entries[resultEntry] = resultJSON

	// Reference data goes in only when the run could actually have read it.
	// A megabyte of share counts for companies this strategy never ranked is
	// a megabyte somebody has to download.
	if in.Fundamentals != nil && in.Fundamentals.HasRows(symbols) {
		var buf bytes.Buffer
		if err := market.WriteSharesCSV(&buf, in.Fundamentals, symbols); err != nil {
			return nil, fmt.Errorf("write bundle %s: render share counts: %w", path, err)
		}
		entries[sharesEntry] = buf.Bytes()
		man.Reference = append(man.Reference, sharesEntry)
	}
	if spec.Index != "" {
		if in.Membership == nil {
			return nil, fmt.Errorf("write bundle %s: the run used the %s index universe "+
				"but no membership table was supplied, and without it the re-run would "+
				"choose a different universe", path, spec.Index)
		}
		var buf bytes.Buffer
		if err := market.WriteMembershipCSV(&buf, in.Membership.Index, in.Membership.Tenures(), 0); err != nil {
			return nil, fmt.Errorf("write bundle %s: render index membership: %w", path, err)
		}
		name := membershipEntry(in.Membership.Index)
		entries[name] = buf.Bytes()
		man.Reference = append(man.Reference, name)
	}

	man.ContentSHA256 = contentHash(entries)
	manJSON, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("write bundle %s: encode manifest: %w", path, err)
	}
	entries[manifestEntry] = manJSON

	if err := writeZip(path, entries); err != nil {
		return nil, err
	}
	return &man, nil
}

// writeZip writes the entries to path, manifest first.
//
// Manifest first so a reader can identify the file, and reject a format it
// does not know, from the first few hundred bytes rather than the whole
// archive.
func writeZip(path string, entries map[string][]byte) error {
	names := make([]string, 0, len(entries))
	for n := range entries {
		if n != manifestEntry {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	names = append([]string{manifestEntry}, names...)

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("write bundle %s: %w", path, err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("write bundle %s: %w", path, err)
	}
	zw := zip.NewWriter(f)
	for _, name := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:   name,
			Method: zip.Deflate,
			// A fixed timestamp, so that bundling the same run twice gives
			// the same bytes and two bundles can be compared with cmp. The
			// date is the earliest a zip can hold; when it was written is in
			// the manifest, where it can be read.
			Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		if err != nil {
			f.Close()
			return fmt.Errorf("write bundle %s: add %s: %w", path, name, err)
		}
		if _, err := w.Write(entries[name]); err != nil {
			f.Close()
			return fmt.Errorf("write bundle %s: add %s: %w", path, name, err)
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return fmt.Errorf("write bundle %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write bundle %s: %w", path, err)
	}
	return nil
}

// contentHash digests every entry except the manifest, which is where the
// digest is written.
//
// Names and bodies are length-prefixed so that moving bytes from one into the
// other cannot leave the digest unchanged. Per-series metadata is deliberately
// not hashed here: it lives in the manifest and is checked on load against the
// bars themselves, which catches a manifest that lies by contradiction rather
// than only by checksum.
func contentHash(entries map[string][]byte) string {
	names := make([]string, 0, len(entries))
	for n := range entries {
		if n != manifestEntry {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%d:%s\n%d:\n", len(n), n, len(entries[n]))
		h.Write(entries[n])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fileLabel maps a ticker to something safe to use as a file name.
//
// The label is decoration: the symbol a file belongs to is read from the
// manifest, never parsed back out of the name. Tickers hold characters like
// ^ = . and / which are illegal or awkward in paths, and one of them is a
// path separator.
func fileLabel(symbol string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(symbol) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 24 {
			break
		}
	}
	if b.Len() == 0 {
		return "symbol"
	}
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
