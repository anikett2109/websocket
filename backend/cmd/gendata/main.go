// Command gendata writes the historical candle file the server loads at startup.
//
//	go run ./cmd/gendata            # 3 days of 1m candles -> data/history_1m.json
//	go run ./cmd/gendata -days 7    # a longer window
//
// The candles are sampled from the same deterministic market function the live
// generator uses (generator.Model), so the file, any gap the server fills at
// startup and live trading form one continuous series.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"cryptofeed/internal/generator"
	"cryptofeed/internal/history"
)

func main() {
	out := flag.String("out", "data/history_1m.json", "output file")
	days := flag.Int("days", 3, "days of 1m candles")
	symbol := flag.String("symbol", "BTCUSDT", "symbol")
	base := flag.Int64("base", 6_500_000, "centre price, fixed-point cents (6500000 = 65000.00)")
	tick := flag.Int64("tick", 50, "tick size, fixed-point cents")
	flag.Parse()

	m := generator.Model{Base: *base, Tick: *tick}
	f := history.Generate(*symbol, m, *days, time.Now().UnixMilli())
	if err := history.Validate(f, *symbol, m); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := history.Save(*out, f); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	first, last := f.Candles[0][0], f.Candles[len(f.Candles)-1][0]
	fmt.Printf("wrote %s: %d 1m candles, %s .. %s (%s)\n", *out, len(f.Candles),
		time.UnixMilli(first).UTC().Format(time.RFC3339), time.UnixMilli(last).UTC().Format(time.RFC3339), history.Generator)
}
