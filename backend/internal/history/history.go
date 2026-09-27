// Package history reads and writes the historical candle file that the
// backend loads into its candle cache at startup.
//
// The file holds 1m candles sampled from the same deterministic market
// function the live generator uses (generator.Model), so history and live data
// are one continuous series. 5m (and any other interval) is aggregated from
// the 1m candles at load time. Values are fixed-point integers.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"cryptofeed/internal/generator"
	"cryptofeed/internal/model"
)

const (
	minute    = int64(60_000)
	Generator = "tick-v1" // identifies the market function that produced the file
)

// File is the on-disk format. Each candle is [t, o, h, l, c, v] with t the
// candle start in unix ms.
type File struct {
	Symbol      string     `json:"symbol"`
	Interval    string     `json:"interval"`
	PriceScale  int64      `json:"priceScale"`
	QtyScale    int64      `json:"qtyScale"`
	TickSize    int64      `json:"tickSize"`
	BasePrice   int64      `json:"basePrice"`
	Generator   string     `json:"generator"`
	GeneratedAt int64      `json:"generatedAt"`
	Candles     [][6]int64 `json:"candles"`
}

// Generate samples `days` of complete 1m candles ending at the minute boundary
// at or before endMs.
func Generate(symbol string, m generator.Model, days int, endMs int64) File {
	end := endMs - endMs%minute
	start := end - int64(days)*24*60*minute
	cs := m.Candles(generator.TickAt(start), generator.TickAt(end), minute)
	f := File{
		Symbol: symbol, Interval: "1m", PriceScale: model.PriceScale, QtyScale: model.QtyScale,
		TickSize: m.Tick, BasePrice: m.Base, Generator: Generator, GeneratedAt: endMs,
		Candles: make([][6]int64, len(cs)),
	}
	for i, c := range cs {
		f.Candles[i] = [6]int64{c.Start, c.Open, c.High, c.Low, c.Close, c.Volume}
	}
	return f
}

// Save writes the file with one candle per line so it stays diffable and readable.
func Save(path string, f File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	head, _ := json.Marshal(struct {
		Symbol      string `json:"symbol"`
		Interval    string `json:"interval"`
		PriceScale  int64  `json:"priceScale"`
		QtyScale    int64  `json:"qtyScale"`
		TickSize    int64  `json:"tickSize"`
		BasePrice   int64  `json:"basePrice"`
		Generator   string `json:"generator"`
		GeneratedAt int64  `json:"generatedAt"`
	}{f.Symbol, f.Interval, f.PriceScale, f.QtyScale, f.TickSize, f.BasePrice, f.Generator, f.GeneratedAt})
	w.Write(head[:len(head)-1]) // drop the closing brace; candles follow
	w.WriteString(`,"candles":[` + "\n")
	for i, c := range f.Candles {
		w.WriteString("[")
		for j, v := range c {
			if j > 0 {
				w.WriteString(",")
			}
			w.WriteString(strconv.FormatInt(v, 10))
		}
		if i < len(f.Candles)-1 {
			w.WriteString("],\n")
		} else {
			w.WriteString("]\n")
		}
	}
	w.WriteString("]}\n")
	if err := w.Flush(); err != nil {
		return err
	}
	return out.Close()
}

// Load reads a history file. A missing file is reported as os.ErrNotExist.
func Load(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return File{}, fmt.Errorf("history: parse %s: %w", path, err)
	}
	return f, nil
}

var ErrInvalid = errors.New("history: invalid file")

// Validate checks the file matches this service's symbol, scales and market
// model, and that candles are contiguous, minute-aligned and consistent.
func Validate(f File, symbol string, m generator.Model) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
	}
	switch {
	case f.Symbol != symbol:
		return bad("symbol %q, want %q", f.Symbol, symbol)
	case f.Interval != "1m":
		return bad("interval %q, want 1m", f.Interval)
	case f.PriceScale != model.PriceScale || f.QtyScale != model.QtyScale:
		return bad("scales %d/%d, want %d/%d", f.PriceScale, f.QtyScale, model.PriceScale, model.QtyScale)
	case f.TickSize != m.Tick || f.BasePrice != m.Base || f.Generator != Generator:
		return bad("market model (tick %d, base %d, %q) does not match the server (tick %d, base %d, %q); regenerate with `go run ./cmd/gendata`",
			f.TickSize, f.BasePrice, f.Generator, m.Tick, m.Base, Generator)
	case len(f.Candles) == 0:
		return bad("no candles")
	}
	for i, c := range f.Candles {
		t, o, h, l, cl, v := c[0], c[1], c[2], c[3], c[4], c[5]
		if t%minute != 0 {
			return bad("candle %d start %d not minute-aligned", i, t)
		}
		if i > 0 && t != f.Candles[i-1][0]+minute {
			return bad("candle %d at %d: gap or disorder after %d", i, t, f.Candles[i-1][0])
		}
		if l <= 0 || l > min(o, cl) || h < max(o, cl) || v < 0 {
			return bad("candle %d has inconsistent OHLCV %v", i, c)
		}
	}
	return nil
}

// ToCandles converts the file rows to model candles. CloseSeq (the tick of
// the last trade in each minute) is derived from the market clock.
func (f File) ToCandles() []model.Candle {
	out := make([]model.Candle, len(f.Candles))
	for i, c := range f.Candles {
		out[i] = model.Candle{Start: c[0], Open: c[1], High: c[2], Low: c[3], Close: c[4], Volume: c[5],
			CloseSeq: generator.LastTradeTick(generator.TickAt(c[0]+minute) - 1)}
	}
	return out
}

// Window returns 1m candles covering [startTick's minute, nowTick): the file's
// candles inside that window, plus candles computed from the market function
// for any minutes the file does not cover (e.g. the file was generated
// earlier, or the partial current minute). The last candle may be partial.
func Window(fileCandles []model.Candle, m generator.Model, days int, nowTick uint32) []model.Candle {
	now := generator.TickTime(nowTick)
	from := now - now%minute - int64(days)*24*60*minute
	var out []model.Candle
	for _, c := range fileCandles {
		if c.Start >= from && c.Start+minute <= now-now%minute {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return m.Candles(generator.TickAt(from), nowTick, minute)
	}
	// File candles are contiguous (Validate); compute whatever lies before and after them.
	before := m.Candles(generator.TickAt(from), generator.TickAt(out[0].Start), minute)
	after := m.Candles(generator.TickAt(out[len(out)-1].Start+minute), nowTick, minute)
	return append(append(before, out...), after...)
}
