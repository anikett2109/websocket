package generator

import "cryptofeed/internal/model"

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
			a.CloseSeq = c.CloseSeq
			continue
		}
		c.Start = start
		out = append(out, c)
	}
	return out
}
