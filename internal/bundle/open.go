package bundle

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/charbelkassab/pyrite/internal/engine"
	"github.com/charbelkassab/pyrite/internal/market"
	"github.com/charbelkassab/pyrite/internal/strategy"
)

// Bundle is a loaded and validated bundle. Nothing in it has been executed.
type Bundle struct {
	Path string
	// Bytes is the size of the file on disk.
	Bytes    int64
	Manifest Manifest
	// Spec is the recorded spec with the strategy code put back into it.
	Spec     engine.Spec
	Code     string
	Recorded RecordedResult
	Series   map[string]*market.Series
	// Fundamentals is the share-count table the bundle carries. It is never
	// nil: a bundle without one gets an empty table rather than the local
	// machine's, so a re-run cannot quietly rank against different data.
	Fundamentals *market.Fundamentals
	// Membership is the index constituent table, nil when the run used a
	// fixed universe.
	Membership *market.Membership
	// Modified reports that the content hash does not match what the bundle
	// claims. It is a finding rather than a refusal: the hash is the author's
	// own checksum and not a signature, so the useful response is to run the
	// thing anyway and let the comparison say exactly what changed.
	Modified bool
	// ComputedSHA256 is the hash of what was actually read.
	ComputedSHA256 string
}

// Open reads a bundle, checks it, and leaves it ready to re-run.
func Open(path string) (*Bundle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open bundle: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("open bundle %s: %w", path, err)
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return nil, fmt.Errorf("open bundle %s: it is not a readable zip archive, so it is "+
			"either truncated or not a pyrite bundle: %w", path, err)
	}

	if err := checkArchive(path, zr); err != nil {
		return nil, err
	}

	// The manifest alone, first. It is what says whether this build can read
	// the bundle at all, and answering that before inflating several hundred
	// megabytes of bars is the whole reason the format has a directory.
	b := &Bundle{Path: path, Bytes: info.Size(), Series: map[string]*market.Series{}}
	raw, err := readOne(path, zr, manifestEntry)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("open bundle %s: it has no %s, so it is not a pyrite bundle", path, manifestEntry)
	}
	if err := strictJSON(raw, &b.Manifest); err != nil {
		return nil, fmt.Errorf("open bundle %s: the manifest is unreadable: %w", path, err)
	}
	if b.Manifest.Format != Format {
		return nil, fmt.Errorf("open bundle %s: it is format %d and this build reads format %d; "+
			"use the pyrite version that wrote it (%s)",
			path, b.Manifest.Format, Format, firstNonEmpty(b.Manifest.PyriteVersion, "unrecorded"))
	}

	entries, err := readEntries(path, zr)
	if err != nil {
		return nil, err
	}
	b.ComputedSHA256 = contentHash(entries)
	b.Modified = b.ComputedSHA256 != b.Manifest.ContentSHA256

	if err := b.loadSpec(entries); err != nil {
		return nil, err
	}
	if err := b.loadResult(entries); err != nil {
		return nil, err
	}
	if err := b.loadSeries(entries); err != nil {
		return nil, err
	}
	if err := b.loadReference(entries); err != nil {
		return nil, err
	}
	return b, nil
}

// checkArchive reads the directory and refuses anything oddly named,
// duplicated, or declaring an implausible size.
//
// This is the cheap pass: it touches no compressed data at all, which is what
// makes refusing a hostile file cost a few hundred bytes rather than a few
// hundred megabytes.
func checkArchive(path string, zr *zip.Reader) error {
	if len(zr.File) == 0 {
		return fmt.Errorf("open bundle %s: the archive is empty", path)
	}
	if len(zr.File) > maxEntries {
		return fmt.Errorf("open bundle %s: it holds %d entries, and no bundle needs more than %d",
			path, len(zr.File), maxEntries)
	}

	var declared uint64
	seen := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		if err := checkName(f.Name); err != nil {
			return err
		}
		if err := allowedName(f.Name); err != nil {
			return err
		}
		if seen[f.Name] {
			return fmt.Errorf("refusing bundle entry %q: it appears twice, and which of the two "+
				"is the real one has no answer", f.Name)
		}
		seen[f.Name] = true
		if f.UncompressedSize64 > maxEntryBytes {
			return fmt.Errorf("refusing bundle entry %q: it declares %d bytes uncompressed, "+
				"over the %d byte limit, which is what a zip bomb looks like and a price series does not",
				f.Name, f.UncompressedSize64, maxEntryBytes)
		}
		declared += f.UncompressedSize64
	}
	if declared > maxTotalBytes {
		return fmt.Errorf("open bundle %s: it declares %d bytes uncompressed, over the %d byte limit",
			path, declared, maxTotalBytes)
	}
	return nil
}

// readOne inflates a single entry, or returns nil if the archive has no such
// name.
func readOne(path string, zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return inflate(path, f)
		}
	}
	return nil, nil
}

// readEntries inflates the whole archive into memory. checkArchive must have
// passed first.
func readEntries(path string, zr *zip.Reader) (map[string][]byte, error) {
	entries := make(map[string][]byte, len(zr.File))
	var total int64
	for _, f := range zr.File {
		data, err := inflate(path, f)
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		if total > maxTotalBytes {
			return nil, fmt.Errorf("refusing bundle %s: its entries expand past the %d byte limit",
				path, maxTotalBytes)
		}
		entries[f.Name] = data
	}
	return entries, nil
}

// inflate decompresses one entry under the per-entry cap.
//
// The cap is enforced here as well as against the declared size, because the
// declaration and the payload have the same author: an entry that says it is
// small and expands forever is precisely a zip bomb.
func inflate(path string, f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open bundle %s: entry %q will not open: %w", path, f.Name, err)
	}
	defer rc.Close()

	// One byte past the cap, so an entry that lied about its size is detected
	// rather than silently truncated.
	data, err := io.ReadAll(io.LimitReader(rc, maxEntryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("open bundle %s: entry %q is unreadable, so the bundle is "+
			"truncated or corrupt: %w", path, f.Name, err)
	}
	if int64(len(data)) > maxEntryBytes {
		return nil, fmt.Errorf("refusing bundle entry %q: it expands past the %d byte limit "+
			"despite declaring %d, which is what a zip bomb does",
			f.Name, maxEntryBytes, f.UncompressedSize64)
	}
	return data, nil
}

// loadSpec reads the spec and the strategy, and refuses code the sandbox
// could not run, before anything is in a position to run it.
func (b *Bundle) loadSpec(entries map[string][]byte) error {
	raw, ok := entries[specEntry]
	if !ok {
		return fmt.Errorf("open bundle %s: it has no %s, so there is no run to reproduce", b.Path, specEntry)
	}
	if err := strictJSON(raw, &b.Spec); err != nil {
		return fmt.Errorf("open bundle %s: the spec is unreadable: %w", b.Path, err)
	}

	code, ok := entries[codeEntry]
	if !ok {
		return fmt.Errorf("open bundle %s: it has no %s, so there is no strategy to run", b.Path, codeEntry)
	}
	if len(code) > maxCodeBytes {
		return fmt.Errorf("refusing bundle %s: the strategy is %d bytes, over the %d byte limit",
			b.Path, len(code), maxCodeBytes)
	}
	b.Code = string(code)
	b.Spec.Code = b.Code
	b.Spec.ApplyDefaults()

	// Read before it is executed. This is the same reading the compiler gives
	// code a model wrote, and a bundle from a stranger has more claim on it.
	if problems := strategy.Check(b.Spec.Code); len(problems) > 0 {
		return fmt.Errorf("refusing the strategy in %s: %s", b.Path, strings.Join(problems, "; "))
	}
	if !b.Spec.Interval.Valid() {
		return fmt.Errorf("refusing bundle %s: %q is not a bar size this build knows",
			b.Path, b.Spec.Interval)
	}
	// The index name is turned into an entry name below, so it is checked
	// here rather than trusted. Nothing in it may be a path.
	for _, r := range b.Spec.Index {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return fmt.Errorf("refusing bundle %s: %q is not a usable index name", b.Path, b.Spec.Index)
	}
	return nil
}

func (b *Bundle) loadResult(entries map[string][]byte) error {
	raw, ok := entries[resultEntry]
	if !ok {
		return fmt.Errorf("open bundle %s: it has no %s, so there is nothing to compare a re-run against",
			b.Path, resultEntry)
	}
	if err := strictJSON(raw, &b.Recorded); err != nil {
		return fmt.Errorf("open bundle %s: the recorded result is unreadable: %w", b.Path, err)
	}
	if len(b.Recorded.Curve) == 0 {
		return fmt.Errorf("open bundle %s: the recorded result has no equity curve, so a re-run "+
			"would have nothing to agree with", b.Path)
	}
	return nil
}

// loadSeries turns the bars files into series, checking each against what the
// manifest claims about it.
//
// The claims are checked rather than trusted because a manifest that
// contradicts its own payload is the cheapest possible sign that a bundle has
// been tampered with, and it is caught here whether or not the content hash
// was updated to match.
func (b *Bundle) loadSeries(entries map[string][]byte) error {
	if len(b.Manifest.Series) == 0 {
		return fmt.Errorf("open bundle %s: the manifest lists no price series", b.Path)
	}
	if len(b.Manifest.Series) > maxSeries {
		return fmt.Errorf("refusing bundle %s: it lists %d series, over the limit of %d",
			b.Path, len(b.Manifest.Series), maxSeries)
	}

	for _, se := range b.Manifest.Series {
		// The manifest is written by the same author as the archive, so its
		// file names get the same check as the archive's own.
		if err := checkName(se.File); err != nil {
			return err
		}
		if !strings.HasPrefix(se.File, barsPrefix) {
			return fmt.Errorf("refusing bundle %s: the manifest points %s at %q, which is not "+
				"under %s", b.Path, se.Symbol, se.File, barsPrefix)
		}
		sym := market.NormalizeSymbol(se.Symbol)
		if sym == "" {
			return fmt.Errorf("refusing bundle %s: the manifest lists a series with no symbol", b.Path)
		}
		if _, dup := b.Series[sym]; dup {
			return fmt.Errorf("refusing bundle %s: %s is listed twice, and which set of bars is "+
				"the real one has no answer", b.Path, sym)
		}
		data, ok := entries[se.File]
		if !ok {
			return fmt.Errorf("open bundle %s: the manifest lists %s at %q but the archive has no "+
				"such entry, so the bundle is incomplete", b.Path, sym, se.File)
		}
		bars, err := readBars(sym, se.File, data)
		if err != nil {
			return err
		}
		s := market.NewSeries(sym, bars)
		s.Name = se.Name
		if len(s.Bars) != se.Bars {
			return fmt.Errorf("refusing bundle %s: the manifest says %s has %d bars and %s holds %d",
				b.Path, sym, se.Bars, se.File, len(s.Bars))
		}
		if s.Bars[0].Date != se.First || s.Bars[len(s.Bars)-1].Date != se.Last {
			return fmt.Errorf("refusing bundle %s: the manifest says %s runs %s to %s and %s holds %s to %s",
				b.Path, sym, se.First, se.Last, se.File,
				s.Bars[0].Date, s.Bars[len(s.Bars)-1].Date)
		}
		b.Series[sym] = s
	}

	// Every bars file must be accounted for. One the manifest does not list
	// is a set of prices nobody would ever see, which is a good place to hide
	// something.
	for name := range entries {
		if !strings.HasPrefix(name, barsPrefix) {
			continue
		}
		listed := false
		for _, se := range b.Manifest.Series {
			if se.File == name {
				listed = true
				break
			}
		}
		if !listed {
			return fmt.Errorf("refusing bundle %s: it carries %q, which the manifest does not list",
				b.Path, name)
		}
	}
	return nil
}

// loadReference reads the share-count and index-membership tables.
func (b *Bundle) loadReference(entries map[string][]byte) error {
	// An empty table rather than the local machine's when the bundle carries
	// none: reference data that came from somewhere other than the bundle is
	// exactly the silent input change a bundle exists to prevent.
	fund, err := market.ParseFundamentals(bytes.NewReader(nil))
	if err != nil {
		return fmt.Errorf("open bundle %s: %w", b.Path, err)
	}
	b.Fundamentals = fund

	names := make([]string, 0, len(entries))
	for name := range entries {
		if strings.HasPrefix(name, refPrefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	wantMembers := ""
	if b.Spec.Index != "" {
		wantMembers = membershipEntry(b.Spec.Index)
	}
	for _, name := range names {
		switch name {
		case sharesEntry:
			f, err := market.ParseFundamentals(bytes.NewReader(entries[name]))
			if err != nil {
				return fmt.Errorf("refusing bundle %s: the share-count table is unreadable: %w", b.Path, err)
			}
			b.Fundamentals = f
		case wantMembers:
			m, err := market.ParseMembership(b.Spec.Index, bytes.NewReader(entries[name]))
			if err != nil {
				return fmt.Errorf("refusing bundle %s: the %s membership table is unreadable: %w",
					b.Path, b.Spec.Index, err)
			}
			b.Membership = m
		default:
			return fmt.Errorf("refusing bundle %s: it carries %q, which this run has no use for",
				b.Path, name)
		}
	}

	if b.Spec.Index != "" && b.Membership == nil {
		return fmt.Errorf("open bundle %s: the run traded the %s index universe but the bundle "+
			"carries no membership table, so a re-run would pick a different universe", b.Path, b.Spec.Index)
	}
	return nil
}

// strictJSON decodes with unknown fields refused.
//
// A bundle written by a newer build may carry fields this one does not know,
// and silently dropping them would mean re-running something other than what
// was recorded while reporting an exact match.
func strictJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("there is more than one JSON document in it")
	}
	return nil
}
