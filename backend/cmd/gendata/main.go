// Command gendata writes the historical candle file the server loads at startup.
//
//	go run ./cmd/gendata                  # 3 days of 1m candles -> data/history_1m.json
//	go run ./cmd/gendata -days 7 -seed 7  # different length / dataset
//
// It uses the same deterministic algorithm as the live generator (generator.History),
// so a given seed always produces the same prices and volumes.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"cryptofeed/internal/history"
)

func main() {
	out := flag.String("out", "data/history_1m.json", "output file")
	days := flag.Int("days", 3, "days of 1m candles")
	seed := flag.Int64("seed", 42, "random seed")
	symbol := flag.String("symbol", "BTCUSDT", "symbol")
	price := flag.Int64("close", 6_500_000, "final close, fixed-point cents (6500000 = 65000.00)")
	tick := flag.Int64("tick", 50, "tick size, fixed-point cents")
	flag.Parse()

	f := history.Generate(*symbol, *seed, *days, time.Now().UnixMilli(), *price, *tick)
	if err := history.Validate(f, *symbol, *tick); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := history.Save(*out, f); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	first, last := f.Candles[0][0], f.Candles[len(f.Candles)-1][0]
	fmt.Printf("wrote %s: %d 1m candles, %s .. %s, seed %d\n", *out, len(f.Candles),
		time.UnixMilli(first).UTC().Format(time.RFC3339), time.UnixMilli(last).UTC().Format(time.RFC3339), *seed)
}
