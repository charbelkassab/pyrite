// Package examples ships the bundled strategies inside the binary.
//
// The Go file lives here, beside the .js files, rather than under internal/
// with a copy of them. Two copies would drift, and examples/ at the repository
// root is where anyone browsing on GitHub looks first.
//
// Embedding is what makes `natural-quant run --example golden-cross` work from
// a downloaded binary with nothing checked out, no API key and nothing else
// installed. That is the shortest path from finding the project to seeing a
// real result on real data, and it is worth a few kilobytes.
package examples

import (
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed *.js
var files embed.FS

// Example is one bundled strategy.
type Example struct {
	Name string `json:"name"`
	// Title and Summary are read from the leading comment block, so each file
	// stays the single source of truth rather than duplicating a description
	// into a Go table that will drift from it.
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Code    string `json:"code"`
	// Universe, Benchmarks and Warmup come from header directives in the
	// file, so a bundled example runs correctly with no flags at all.
	Universe   []string `json:"universe,omitempty"`
	Benchmarks []string `json:"benchmarks,omitempty"`
	Warmup     int      `json:"warmup,omitempty"`
	AllowShort bool     `json:"allow_short,omitempty"`
	// NeedsModel marks an example that calls a model inside the backtest, so
	// the CLI can say why it will not run without one.
	NeedsModel bool `json:"needs_model,omitempty"`
}

// Names lists the bundled example names, sorted.
func Names() []string {
	entries, err := files.ReadDir(".")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".js") {
			out = append(out, strings.TrimSuffix(e.Name(), ".js"))
		}
	}
	sort.Strings(out)
	return out
}

// All returns every bundled example.
func All() []Example {
	names := Names()
	out := make([]Example, 0, len(names))
	for _, n := range names {
		if ex, err := Get(n); err == nil {
			out = append(out, ex)
		}
	}
	return out
}

// Get loads one example by name.
func Get(name string) (Example, error) {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".js")
	b, err := files.ReadFile(name + ".js")
	if err != nil {
		return Example{}, fmt.Errorf("no bundled example named %q.\nAvailable: %s",
			name, strings.Join(Names(), ", "))
	}
	return parse(name, string(b)), nil
}

// parse pulls the header directives and the descriptive comment out of a file.
//
// The directives are ordinary comments, so each file remains a valid strategy
// that can be copied out and edited without stripping anything first.
func parse(name, src string) Example {
	ex := Example{Name: name, Code: src}
	var summary []string
	titleDone := false

	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "//") {
			if trimmed != "" {
				break // the leading comment block has ended
			}
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(trimmed, "//"))
		if body == "" {
			if ex.Title != "" {
				titleDone = true
			}
			summary = append(summary, "")
			continue
		}
		if directive, value, ok := strings.Cut(body, ":"); ok {
			value = strings.TrimSpace(value)
			switch strings.ToLower(strings.TrimSpace(directive)) {
			case "universe":
				ex.Universe = splitList(value)
				continue
			case "benchmarks", "benchmark":
				ex.Benchmarks = splitList(value)
				continue
			case "warmup":
				if n, err := strconv.Atoi(value); err == nil {
					ex.Warmup = n
				}
				continue
			case "allow_short", "short":
				ex.AllowShort = value == "true" || value == "yes"
				continue
			case "needs_model", "needs_ai":
				ex.NeedsModel = value == "true" || value == "yes"
				continue
			}
		}
		// The title is the first paragraph, not the first line: a one-
		// sentence description that happens to wrap in the source should
		// not be truncated at the wrap.
		if !titleDone {
			if ex.Title == "" {
				ex.Title = body
			} else {
				ex.Title += " " + body
			}
			continue
		}
		summary = append(summary, body)
	}
	ex.Title = strings.Join(strings.Fields(ex.Title), " ")
	ex.Summary = strings.Join(strings.Fields(strings.Join(summary, " ")), " ")
	return ex
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
