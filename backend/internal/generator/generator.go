package generator

import (
	"context"
	"math"
	"time"

	"cryptofeed/internal/model"
)

// Model holds the only two market parameters: the centre price and tick size
// (both fixed-point cents). Every other value is derived from the tick index.
type Model struct {
	Base int64 // centre price, e.g. 6_500_000 = 65,000.00
	Tick int64 // price grid, e.g. 50 = 0.50
}

// Price-path components (amplitude in cents, period in seconds). Slopes are
// A·2π/P: 0.07 + 0.13 + 0.79 + 1.36 ≈ 2.3 $/s at most, so a 1m candle spans
// roughly $20–90, which is plausible for BTC around $65k.
var waves = [...]struct{ amp, period float64 }{
	{25_000, 6 * 3600}, // ±$250 over 6 h: multi-day trend in the 3-day history
	{12_000, 97 * 60},  // ±$120 over 97 min
	{3_000, 240},       // ±$30 over 4 min: 1m candle bodies
	{800, 37},          // ±$8 over 37 s: short oscillation
}

const (
	burstMove   = 6_000 // ±$60 triangular move across each burst window
	burstWiggle = 1_500 // ±$15, 2 s period, enveloped to 0 at the burst edges
)

// Mid is the fair price at tick n (cents, not yet on the grid). Burst terms are
// zero at the window edges so the path is continuous across regimes; the
// burst direction alternates by cycle.
func (m Model) Mid(n uint32) float64 {
	s := float64(n) * TickMs / 1000
	v := float64(m.Base)
	for _, w := range waves {
		v += w.amp * math.Sin(2*math.Pi*s/w.period)
	}
	if p := phase(n); p >= NormalEnd && p < BurstEnd {
		u := float64(p-NormalEnd) / (BurstEnd - NormalEnd) // 0..1 across the burst
		dir := 1.0
		if (n/CycleTicks)%2 == 1 {
			dir = -1
		}
		v += dir*burstMove*(1-math.Abs(2*u-1)) + burstWiggle*math.Sin(2*math.Pi*s/2)*math.Sin(math.Pi*u)
	}
	return v
}

// BestBid is the book's best bid at book tick b: the mid floored to the grid.
// The spread is always one tick, so BestAsk = BestBid + Tick.
func (m Model) BestBid(b uint32) int64 {
	return int64(math.Floor(m.Mid(b)/float64(m.Tick))) * m.Tick
}

// levelQty pulses each level between its floor and floor+0.30 BTC with a 2 s
// period, phase-shifted per level (and by half a period for asks).
func levelQty(i int, phaseTicks uint32) int64 {
	q := 200_000 + 100_000*float64(i) + 150_000*(1+math.Sin(2*math.Pi*float64(phaseTicks+uint32(7*i))/40))
	return int64(q/1000) * 1000 // 0.001 BTC lots
}

// Book is the canonical book after tick n; its Seq is the tick it last changed.
func (m Model) Book(n uint32) model.Book {
	b := LastBookTick(n)
	bid := m.BestBid(b)
	book := model.Book{Seq: b}
	for i := 0; i < model.Levels; i++ {
		book.Bids[i] = model.Level{Price: bid - int64(i)*m.Tick, Qty: levelQty(i, b)}
		book.Asks[i] = model.Level{Price: bid + m.Tick + int64(i)*m.Tick, Qty: levelQty(i, b+20)}
	}
	return book
}

// Trades returns the trades of tick n (possibly none). Trades execute against
// the current book: even ids buy at the ask, odd ids sell at the bid. Size
// cycles through 13 values, 0.010–0.058 BTC.
func (m Model) Trades(n uint32) []model.Trade {
	k := TradesAt(n)
	if k == 0 {
		return nil
	}
	bid := m.BestBid(LastBookTick(n))
	first := TradesBefore(n) + 1
	out := make([]model.Trade, k)
	for j := range out {
		id := first + uint32(j)
		t := model.Trade{ID: id, TS: TickTime(n) + int64(j*TickMs/k), Qty: 10_000 + 4_000*int64((7*id)%13)}
		if id%2 == 0 {
			t.Side, t.Price = model.Buy, bid+m.Tick
		} else {
			t.Side, t.Price = model.Sell, bid
		}
		out[j] = t
	}
	return out
}

// Event is everything that happens in one tick.
type Event struct {
	Tick   uint32
	Trades []model.Trade // may be empty
	Book   *model.Book   // nil when the book does not change this tick
}

func (m Model) Event(n uint32) Event {
	ev := Event{Tick: n, Trades: m.Trades(n)}
	if BookChangesAt(n) {
		b := m.Book(n)
		ev.Book = &b
	}
	return ev
}

// Generator emits one Event per tick, following the wall clock.
type Generator struct {
	m    Model
	next uint32
	now  func() time.Time
}

// New starts generating at tick start (normally TickAt(now)).
func New(m Model, start uint32) *Generator {
	return &Generator{m: m, next: start, now: time.Now}
}

// Run emits events until ctx is cancelled. If the process falls behind (GC
// pause, slow host) it emits every missed tick in order, so the stream is
// always complete; nothing is ever skipped.
func (g *Generator) Run(ctx context.Context, out chan<- Event) {
	t := time.NewTicker(TickMs * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for due := TickAt(g.now().UnixMilli()); g.next <= due; g.next++ {
				select {
				case out <- g.m.Event(g.next):
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// Candles aggregates ticks [from, to) into candles of intervalMs, exactly as
// live trading would build them. The last candle may be partial.
func (m Model) Candles(from, to uint32, intervalMs int64) []model.Candle {
	var out []model.Candle
	for n := from; n < to; n++ {
		for _, t := range m.Trades(n) {
			start := t.TS - t.TS%intervalMs
			if k := len(out); k == 0 || out[k-1].Start != start {
				out = append(out, model.Candle{Start: start, Open: t.Price, High: t.Price, Low: t.Price, Close: t.Price})
			}
			c := &out[len(out)-1]
			c.High = max(c.High, t.Price)
			c.Low = min(c.Low, t.Price)
			c.Close = t.Price
			c.Volume += t.Qty
			c.CloseSeq = n
		}
	}
	return out
}
