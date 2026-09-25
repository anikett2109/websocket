// Package history reads and writes the historical candle data file that the
// backend loads into its candle cache at startup.
//
// The file holds 1m candles only; 5m (and any other interval) is aggregated
// from them at load time, so all intervals are consistent. Values are
// fixed-point integers (price x PriceScale, qty x QtyScale).
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

const minute = int64(60_000)

// File is the on-disk format. Each candle is [t, o, h, l, c, v] with t the
// candle start in unix ms.
type File struct {
	Symbol      string     `json:"symbol"`
	Interval    string     `json:"interval"`
	PriceScale  int64      `json:"priceScale"`
	QtyScale    int64      `json:"qtyScale"`
	TickSize    int64      `json:"tickSize"`
	Seed        int64      `json:"seed"`
	GeneratedAt int64      `json:"generatedAt"`
	Candles     [][6]int64 `json:"candles"`
}

// Generate builds `days` of 1m candles ending just before endMs's minute, using
// the same deterministic algorithm the generator uses, closing at endPrice.
func Generate(symbol string, seed int64, days int, endMs, endPrice, tick int64) File {
	cs := generator.History(seed, days*24*60, endMs-endMs%minute, endPrice, tick)
	f := File{
		Symbol: symbol, Interval: "1m", PriceScale: model.PriceScale, QtyScale: model.QtyScale,
		TickSize: tick, Seed: seed, GeneratedAt: endMs, Candles: make([][6]int64, len(cs)),
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
		Seed        int64  `json:"seed"`
		GeneratedAt int64  `json:"generatedAt"`
	}{f.Symbol, f.Interval, f.PriceScale, f.QtyScale, f.TickSize, f.Seed, f.GeneratedAt})
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

// Validate checks the file matches this service's symbol, scales and tick,
// and that candles are contiguous, minute-aligned and internally consistent.
func Validate(f File, symbol string, tick int64) error {
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
	case f.TickSize != tick:
		return bad("tick size %d, want %d", f.TickSize, tick)
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

// ToCandles converts the file rows to model candles.
func (f File) ToCandles() []model.Candle {
	out := make([]model.Candle, len(f.Candles))
	for i, c := range f.Candles {
		out[i] = model.Candle{Start: c[0], Open: c[1], High: c[2], Low: c[3], Close: c[4], Volume: c[5]}
	}
	return out
}

// Rebase shifts candle times by a whole number of minutes so the last candle
// ends exactly at the start of nowMs's minute. The file is a fixed dataset
// generated at some past time; rebasing makes it read as "the last 3 days"
// and lets live trading continue seamlessly from its final close.
func Rebase(cs []model.Candle, nowMs int64) []model.Candle {
	if len(cs) == 0 {
		return cs
	}
	shift := (nowMs - nowMs%minute) - (cs[len(cs)-1].Start + minute)
	out := make([]model.Candle, len(cs))
	for i, c := range cs {
		c.Start += shift
		out[i] = c
	}
	return out
}
