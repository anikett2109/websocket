// Package generator produces a deterministic synthetic market for one symbol.
//
// For a given seed the sequence of events (trade prices/quantities and book
// shapes) is identical on every run; only wall-clock timestamps differ. The
// generator owns its own model of the book so trades happen at the touch and
// the book follows a random-walking mid price.
package generator

import (
	"context"
	"math/rand"
	"time"

	"cryptofeed/internal/model"
)

// Event is one generator tick. Trade is always set; Book is nil on ticks where
// the depth update is skipped (giving ~50 ms depth cadence with occasional ~100 ms gaps).
type Event struct {
	Trade model.Trade
	Book  *model.Book
}

type Config struct {
	Seed         int64
	StartPrice   int64
	TickSize     int64
	DepthSkipPct int
}

type Generator struct {
	cfg     Config
	rng     *rand.Rand
	bestBid int64
	spread  int64 // in ticks
	bids    [model.Levels]int64
	asks    [model.Levels]int64
	nextID  uint32
	lastTS  int64
}

func New(cfg Config) *Generator {
	g := &Generator{
		cfg:     cfg,
		rng:     rand.New(rand.NewSource(cfg.Seed)),
		bestBid: cfg.StartPrice - cfg.StartPrice%cfg.TickSize,
		spread:  1,
		nextID:  1,
	}
	for i := range g.bids {
		g.bids[i] = g.levelQty(i)
		g.asks[i] = g.levelQty(i)
	}
	return g
}

// Run emits one event per tick until ctx is cancelled.
func (g *Generator) Run(ctx context.Context, every time.Duration, out chan<- Event) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			select {
			case out <- g.Step(now.UnixMilli()):
			case <-ctx.Done():
				return
			}
		}
	}
}

// Step advances the simulation by one tick at wall time nowMs.
func (g *Generator) Step(nowMs int64) Event {
	if nowMs < g.lastTS {
		nowMs = g.lastTS // timestamps never go backwards
	}
	g.lastTS = nowMs

	g.walk()
	tr := g.trade(nowMs)
	g.mutate()

	ev := Event{Trade: tr}
	if g.rng.Intn(100) >= g.cfg.DepthSkipPct {
		b := g.Book()
		ev.Book = &b
	}
	return ev
}

// Book returns the generator's current top-10 book (Seq is assigned by the engine).
func (g *Generator) Book() model.Book {
	var b model.Book
	ask := g.bestBid + g.spread*g.cfg.TickSize
	for i := 0; i < model.Levels; i++ {
		b.Bids[i] = model.Level{Price: g.bestBid - int64(i)*g.cfg.TickSize, Qty: g.bids[i]}
		b.Asks[i] = model.Level{Price: ask + int64(i)*g.cfg.TickSize, Qty: g.asks[i]}
	}
	return b
}

// walk moves the mid price by 0-3 ticks, with a weak pull back toward the
// start price so a long demo does not drift away indefinitely.
func (g *Generator) walk() {
	if g.rng.Intn(100) < 50 {
		k := int64(1 + g.rng.Intn(3))
		dir := int64(1)
		if g.rng.Intn(2) == 0 {
			dir = -1
		}
		drift := (g.bestBid - g.cfg.StartPrice) / g.cfg.TickSize
		if drift > 400 && g.rng.Intn(100) < 60 {
			dir = -1
		} else if drift < -400 && g.rng.Intn(100) < 60 {
			dir = 1
		}
		g.shift(dir * k)
	}
	// Spread mostly one tick, occasionally two.
	if g.rng.Intn(100) < 10 {
		g.spread = 2
	} else if g.spread == 2 && g.rng.Intn(100) < 40 {
		g.spread = 1
	}
}

// shift moves the whole book by k ticks, keeping quantities attached to prices
// where they overlap and inventing fresh quantities for newly exposed levels.
func (g *Generator) shift(k int64) {
	if k == 0 {
		return
	}
	var nb, na [model.Levels]int64
	for i := 0; i < model.Levels; i++ {
		// bid at index i has price bestBid' - i*tick = bestBid - (i-k)*tick
		if j := int64(i) - k; j >= 0 && j < model.Levels {
			nb[i] = g.bids[j]
		} else {
			nb[i] = g.levelQty(i)
		}
		// ask at index i: price ask' + i*tick = ask + (i+k)*tick
		if j := int64(i) + k; j >= 0 && j < model.Levels {
			na[i] = g.asks[j]
		} else {
			na[i] = g.levelQty(i)
		}
	}
	g.bids, g.asks = nb, na
	g.bestBid += k * g.cfg.TickSize
}

func (g *Generator) trade(ts int64) model.Trade {
	side := model.Buy
	if g.rng.Intn(2) == 0 {
		side = model.Sell
	}
	// Mostly at the touch, occasionally one level through.
	lvl := 0
	if g.rng.Intn(100) < 8 {
		lvl = 1
	}
	// Exponential-ish size, mean ~0.08 BTC, rounded to 0.0001 BTC.
	qty := int64(g.rng.ExpFloat64()*800)*100 + 100

	var price int64
	if side == model.Buy {
		price = g.bestBid + (g.spread+int64(lvl))*g.cfg.TickSize
		g.asks[lvl] = g.consume(g.asks[lvl], qty, lvl)
	} else {
		price = g.bestBid - int64(lvl)*g.cfg.TickSize
		g.bids[lvl] = g.consume(g.bids[lvl], qty, lvl)
	}
	t := model.Trade{ID: g.nextID, TS: ts, Price: price, Qty: qty, Side: side}
	g.nextID++
	return t
}

// consume removes traded liquidity; an exhausted level is replenished so the
// book always keeps ten non-empty levels per side.
func (g *Generator) consume(level, qty int64, i int) int64 {
	if level-qty < 1000 {
		return g.levelQty(i)
	}
	return level - qty
}

// mutate changes 2-4 random levels' resting quantity by -40%..+40%.
func (g *Generator) mutate() {
	n := 2 + g.rng.Intn(3)
	for k := 0; k < n; k++ {
		i := g.rng.Intn(model.Levels)
		side := &g.bids
		if g.rng.Intn(2) == 0 {
			side = &g.asks
		}
		pct := int64(60 + g.rng.Intn(81)) // 60..140
		q := side[i] * pct / 100
		q -= q % 1000
		q = min(max(q, 1000), 20_000_000) // keep 0.001..20 BTC per level
		side[i] = q
	}
}

// levelQty returns a fresh resting quantity; deeper levels are thicker.
func (g *Generator) levelQty(i int) int64 {
	base := int64(50_000 + g.rng.Intn(600_000) + i*100_000) // 0.05..0.65 BTC + depth
	return base - base%1000
}
