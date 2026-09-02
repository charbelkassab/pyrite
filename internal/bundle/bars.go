package bundle

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/charbelkassab/pyrite/internal/market"
)

// barHeader is the first line of every bars file.
var barHeader = []string{"date", "open", "high", "low", "close", "adj_close", "volume"}

// writeBars renders a series as CSV.
//
// CSV rather than JSON because a decade of daily bars for forty symbols is
// several times smaller this way before compression, and because somebody
// checking a claim should be able to open the prices and read them. The
// numbers are written at shortest round-trip precision, so parsing one back
// yields the identical float64 and the promise of an exact re-run survives the
// file format.
func writeBars(w io.Writer, s *market.Series) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(barHeader); err != nil {
		return err
	}
	for _, b := range s.Bars {
		rec := []string{
			string(b.Date),
			num(b.Open), num(b.High), num(b.Low),
			num(b.Close), num(b.AdjClose), num(b.Volume),
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func num(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// readBars parses a bars file back into a series.
//
// Every value is checked here rather than left to fail somewhere downstream:
// a bundle is untrusted, and a NaN or an infinity in a price would propagate
// silently through the whole run and come out the far end as a plausible
// number.
func readBars(symbol, name string, data []byte) ([]market.Bar, error) {
	cr := csv.NewReader(bytes.NewReader(data))
	cr.FieldsPerRecord = len(barHeader)
	cr.ReuseRecord = true

	head, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("refusing %s: the bars for %s have no header row: %w", name, symbol, err)
	}
	for i, want := range barHeader {
		if strings.TrimSpace(head[i]) != want {
			return nil, fmt.Errorf("refusing %s: the bars for %s have column %d named %q, expected %q",
				name, symbol, i+1, head[i], want)
		}
	}

	bars := make([]market.Bar, 0, 1024)
	for row := 2; ; row++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("refusing %s: the bars for %s are unreadable at row %d: %w",
				name, symbol, row, err)
		}
		if len(bars) >= maxBarsPerSeries {
			return nil, fmt.Errorf("refusing %s: the bars for %s run past %d rows, which is more "+
				"history than any bar size produces", name, symbol, maxBarsPerSeries)
		}
		day, err := market.ParseDay(rec[0])
		if err != nil {
			return nil, fmt.Errorf("refusing %s: the bars for %s have a bad date at row %d: %w",
				name, symbol, row, err)
		}
		vals := make([]float64, 6)
		for i := range vals {
			v, err := strconv.ParseFloat(strings.TrimSpace(rec[i+1]), 64)
			if err != nil {
				return nil, fmt.Errorf("refusing %s: the bars for %s have a bad %s at row %d: %q is not a number",
					name, symbol, barHeader[i+1], row, rec[i+1])
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, fmt.Errorf("refusing %s: the bars for %s have a %s of %q at row %d, "+
					"which is not a price", name, symbol, barHeader[i+1], rec[i+1], row)
			}
			vals[i] = v
		}
		bars = append(bars, market.Bar{
			Date: day, Open: vals[0], High: vals[1], Low: vals[2],
			Close: vals[3], AdjClose: vals[4], Volume: vals[5],
		})
	}
	if len(bars) == 0 {
		return nil, fmt.Errorf("refusing %s: the bars for %s are empty", name, symbol)
	}
	return bars, nil
}
