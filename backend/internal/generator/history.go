package generator

import (
	"math/rand"

	"cryptofeed/internal/model"
)

// History returns n deterministic synthetic 1m candles ending just before
// endStart (exclusive), with the last close equal to endPrice so live trading
// continues seamlessly from the history.
func History(seed int64, n int, endStart, endPrice, tick int64) []model.Candle {
	if n <= 0 {
		return nil
	}
	rng := rand.New(rand.NewSource(seed + 7919))
	const minute = int64(60_000)

	// Walk forward from 0, then offset so the final close lands on endPrice.
	out := make([]model.Candle, n)
	var price int64
	for i := 0; i < n; i++ {
		open := price
		closeP := open + snap(int64(rng.NormFloat64()*2600), tick) // ~$26 std per minute
		hi := max(open, closeP) + snap(int64(rng.ExpFloat64()*900), tick)
		lo := min(open, closeP) - snap(int64(rng.ExpFloat64()*900), tick)
		vol := int64(60_000_000 + rng.Intn(80_000_000)) // 60..140 BTC
		out[i] = model.Candle{
			Start: endStart - int64(n-i)*minute,
			Open:  open, High: hi, Low: lo, Close: closeP, Volume: vol,
		}
		price = closeP
	}
	off := endPrice - price
	for i := range out {
		out[i].Open += off
		out[i].High += off
		out[i].Low += off
		out[i].Close += off
	}
	return out
}

func snap(v, tick int64) int64 { return v - v%tick }

// Aggregate groups 1m candles into interval-aligned candles of size intervalMs.
func Aggregate(src []model.Candle, intervalMs int64) []model.Candle {
	var out []model.Candle
	for _, c := range src {
		start := c.Start - c.Start%intervalMs
		if n := len(out); n > 0 && out[n-1].Start == start {
			a := &out[n-1]
			a.High = max(a.High, c.High)
			a.Low = min(a.Low, c.Low)
			a.Close = c.Close
			a.Volume += c.Volume
			continue
		}
		c.Start = start
		out = append(out, c)
	}
	return out
}
