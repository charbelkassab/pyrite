package engine

import (
	"fmt"
	"math"
	"sort"
	"strconv"
)

// ParamDecl is a tunable a strategy declared for itself in setup().
//
// Declaring a parameter is what makes a strategy sweepable. Without it, "the
// 50 day average" is a constant buried in the code and the only way to try 20
// and 100 is to recompile — which is why a backtester that cannot sweep is
// really a backtester that can only ever tell you about one point.
type ParamDecl struct {
	Name    string `json:"name"`
	Default any    `json:"default"`
	// Grid is the set of values a sweep should try. When empty the parameter
	// is fixed: it still appears in the manifest and can be overridden by
	// hand, but it contributes no dimension to a search.
	Grid        []any  `json:"grid,omitempty"`
	Description string `json:"description,omitempty"`
}

// Values returns the candidates to sweep, which is the grid when one was
// declared and the default alone otherwise.
func (p ParamDecl) Values() []any {
	if len(p.Grid) > 0 {
		return p.Grid
	}
	return []any{p.Default}
}

// declareParam registers a parameter and returns the value in force.
//
// The value in force is the override from the spec when there is one — which
// is how a sweep injects a combination — and the declared default otherwise.
func (e *Engine) declareParam(name string, def any, grid []any, desc string) any {
	if name == "" {
		return def
	}
	if e.paramIdx == nil {
		e.paramIdx = map[string]int{}
	}
	if i, ok := e.paramIdx[name]; ok {
		// Re-declaration (a strategy calling ctx.param inside onDay) keeps
		// the first declaration rather than churning the grid every session.
		return e.paramValue(name, e.paramDecls[i].Default)
	}
	e.paramIdx[name] = len(e.paramDecls)
	e.paramDecls = append(e.paramDecls, ParamDecl{
		Name: name, Default: def, Grid: grid, Description: desc,
	})
	return e.paramValue(name, def)
}

// paramValue resolves the active value for a parameter.
func (e *Engine) paramValue(name string, def any) any {
	if v, ok := e.spec.Params[name]; ok {
		return v
	}
	return def
}

// ExpandRange builds a numeric grid from min, max and step.
//
// Values are rounded to a sane number of decimals: floating point accumulation
// otherwise turns a 0.1 step into 0.30000000000000004, which then shows up as
// a parameter label in the interface.
func ExpandRange(min, max, step float64) []any {
	if step <= 0 || max < min {
		return nil
	}
	n := int(math.Floor((max-min)/step + 1e-9))
	if n < 0 {
		return nil
	}
	// A grid large enough to be a mistake is a mistake.
	if n > 10000 {
		return nil
	}
	out := make([]any, 0, n+1)
	for i := 0; i <= n; i++ {
		v := min + float64(i)*step
		out = append(out, roundTo(v, decimalsFor(step)))
	}
	return out
}

func decimalsFor(step float64) int {
	for d := 0; d <= 8; d++ {
		scaled := step * math.Pow(10, float64(d))
		if math.Abs(scaled-math.Round(scaled)) < 1e-9 {
			return d
		}
	}
	return 8
}

func roundTo(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(v*p) / p
}

// Combos expands parameter declarations into every combination, in a stable
// order.
//
// Order matters more than it looks: a sweep that returns its rows in map
// iteration order produces a different heatmap on every run over identical
// data, and nobody trusts a tool that will not sit still.
func Combos(decls []ParamDecl, limit int) ([]map[string]any, error) {
	swept := make([]ParamDecl, 0, len(decls))
	for _, d := range decls {
		if len(d.Values()) > 1 {
			swept = append(swept, d)
		}
	}
	sort.Slice(swept, func(i, j int) bool { return swept[i].Name < swept[j].Name })

	total := 1
	for _, d := range swept {
		total *= len(d.Values())
		if limit > 0 && total > limit {
			return nil, fmt.Errorf("that is %d+ combinations, over the limit of %d — "+
				"narrow a grid or raise --max-combos", total, limit)
		}
	}

	// Fixed parameters still travel with every combination so a row is a
	// complete description of the run that produced it.
	base := map[string]any{}
	for _, d := range decls {
		if len(d.Values()) <= 1 {
			base[d.Name] = d.Default
		}
	}

	out := []map[string]any{cloneParams(base)}
	for _, d := range swept {
		vals := d.Values()
		next := make([]map[string]any, 0, len(out)*len(vals))
		for _, combo := range out {
			for _, v := range vals {
				c := cloneParams(combo)
				c[d.Name] = v
				next = append(next, c)
			}
		}
		out = next
	}
	return out, nil
}

func cloneParams(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// FormatParams renders a combination as a short stable label.
func FormatParams(p map[string]any) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := ""
	for i, k := range keys {
		if i > 0 {
			s += " "
		}
		s += k + "=" + formatValue(p[k])
	}
	return s
}

func formatValue(v any) string {
	switch t := v.(type) {
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		return strconv.FormatBool(t)
	case string:
		return t
	default:
		return fmt.Sprint(v)
	}
}
