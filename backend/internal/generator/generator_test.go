package generator

import (
	"testing"

	"cryptofeed/internal/model"
)

func TestDeterministic(t *testing.T) {
	cfg := Config{Seed: 42, StartPrice: 6_500_000, TickSize: 50, DepthSkipPct: 15}
	a, b := New(cfg), New(cfg)
	for i := 0; i < 5000; i++ {
		ea, eb := a.Step(int64(i)), b.Step(int64(i))
		if ea.Trade != eb.Trade || (ea.Book == nil) != (eb.Book == nil) || (ea.Book != nil && *ea.Book != *eb.Book) {
			t.Fatalf("step %d diverged", i)
		}
	}
	c := New(Config{Seed: 43, StartPrice: 6_500_000, TickSize: 50, DepthSkipPct: 15})
	if c.Step(0).Trade == New(cfg).Step(0).Trade && c.Step(1).Trade == New(cfg).Step(1).Trade {
		t.Fatal("different seeds should differ")
	}
}

func TestInvariants(t *testing.T) {
	g := New(Config{Seed: 7, StartPrice: 6_500_000, TickSize: 50, DepthSkipPct: 15})
	var lastID uint32
	for i := 0; i < 20000; i++ {
		ev := g.Step(int64(i))
		if ev.Trade.ID != lastID+1 {
			t.Fatalf("trade ids not consecutive: %d after %d", ev.Trade.ID, lastID)
		}
		lastID = ev.Trade.ID
		if ev.Trade.Price%50 != 0 || ev.Trade.Qty <= 0 {
			t.Fatalf("bad trade %+v", ev.Trade)
		}
		b := g.Book()
		if b.BestBid() >= b.BestAsk() {
			t.Fatalf("crossed book at %d", i)
		}
		for l := 0; l < model.Levels; l++ {
			if b.Bids[l].Qty <= 0 || b.Asks[l].Qty <= 0 {
				t.Fatalf("empty level at step %d", i)
			}
		}
	}
}

func TestHistoryEndsAtStartPrice(t *testing.T) {
	h := History(42, 100, 6_000_000, 6_500_000, 50)
	if len(h) != 100 || h[99].Close != 6_500_000 || h[99].Start != 6_000_000-60_000 {
		t.Fatalf("last=%+v", h[len(h)-1])
	}
	agg := Aggregate(h, 300_000)
	var vol int64
	for _, c := range h {
		vol += c.Volume
	}
	var aggVol int64
	for _, c := range agg {
		aggVol += c.Volume
		if c.High < c.Open || c.High < c.Close || c.Low > c.Open || c.Low > c.Close {
			t.Fatalf("inconsistent aggregated candle %+v", c)
		}
	}
	if vol != aggVol {
		t.Fatalf("aggregation lost volume")
	}
}
