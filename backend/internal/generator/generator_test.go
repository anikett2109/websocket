package generator

import (
	"testing"

	"cryptofeed/internal/model"
)

var m = Model{Base: 6_500_000, Tick: 50}

// A tick number well after Epoch, at the start of a cycle.
const n0 = uint32(1200 * 400_000)

func TestRegimeSchedule(t *testing.T) {
	cases := []struct {
		p      uint32
		reg    Regime
		trades int
		book   bool
	}{
		{0, Normal, 1, true}, {599, Normal, 1, true},
		{600, Burst, 3, true}, {799, Burst, 3, true},
		{800, Quiet, 1, true}, {801, Quiet, 0, false}, {805, Quiet, 0, true}, {819, Quiet, 0, false}, {820, Quiet, 1, true},
	}
	for _, c := range cases {
		n := n0 + c.p
		if RegimeOf(n) != c.reg || TradesAt(n) != c.trades || BookChangesAt(n) != c.book {
			t.Errorf("phase %d: regime=%v trades=%d book=%v", c.p, RegimeOf(n), TradesAt(n), BookChangesAt(n))
		}
	}
}

// The derived rates the whole design rests on.
func TestCycleTotals(t *testing.T) {
	var trades, books int
	for n := n0; n < n0+CycleTicks; n++ {
		trades += TradesAt(n)
		if BookChangesAt(n) {
			books++
		}
	}
	if trades != 1220 || TradesPerCycle != 1220 || books != 880 || BookStatesPerCycle != 880 {
		t.Fatalf("per cycle: trades=%d books=%d", trades, books)
	}
}

// The closed forms must agree with plain counting over several cycles.
func TestClosedFormsMatchIteration(t *testing.T) {
	start := n0 - 3*CycleTicks + 17
	count := TradesBefore(start)
	lastTrade, lastBook := LastTradeTick(start), LastBookTick(start)
	for n := start; n < start+5*CycleTicks; n++ {
		if TradesBefore(n) != count {
			t.Fatalf("TradesBefore(%d)=%d, counted %d", n, TradesBefore(n), count)
		}
		if TradesAt(n) > 0 {
			lastTrade = n
		}
		if BookChangesAt(n) {
			lastBook = n
		}
		if LastTradeTick(n) != lastTrade || LastBookTick(n) != lastBook {
			t.Fatalf("tick %d: LastTradeTick=%d want %d, LastBookTick=%d want %d", n, LastTradeTick(n), lastTrade, LastBookTick(n), lastBook)
		}
		count += uint32(TradesAt(n))
	}
}

// Restart-safety: the event at tick n never depends on what ran before.
func TestPureFunctionOfTick(t *testing.T) {
	for _, n := range []uint32{n0, n0 + 650, n0 + 800, n0 + 1234567} {
		a, b := m.Event(n), m.Event(n)
		if len(a.Trades) != len(b.Trades) || (a.Book == nil) != (b.Book == nil) || (a.Book != nil && *a.Book != *b.Book) {
			t.Fatalf("tick %d not deterministic", n)
		}
		for i := range a.Trades {
			if a.Trades[i] != b.Trades[i] {
				t.Fatalf("tick %d trade %d differs", n, i)
			}
		}
	}
}

func TestTradesAndBookInvariants(t *testing.T) {
	var lastID uint32
	var lastTS int64
	for n := n0; n < n0+3*CycleTicks; n++ {
		book := m.Book(n)
		if book.BestBid() >= book.BestAsk() || book.Seq != LastBookTick(n) {
			t.Fatalf("tick %d: bad book", n)
		}
		for i := 0; i < model.Levels; i++ {
			if book.Bids[i].Qty <= 0 || book.Asks[i].Qty <= 0 || book.Bids[i].Price != book.BestBid()-int64(i)*50 {
				t.Fatalf("tick %d level %d invalid", n, i)
			}
		}
		for _, tr := range m.Trades(n) {
			if lastID != 0 && tr.ID != lastID+1 {
				t.Fatalf("ids not consecutive: %d after %d", tr.ID, lastID)
			}
			if tr.TS <= lastTS {
				t.Fatalf("timestamps not increasing at id %d", tr.ID)
			}
			if tr.Price != book.BestBid() && tr.Price != book.BestAsk() {
				t.Fatalf("trade %d not at the touch", tr.ID)
			}
			lastID, lastTS = tr.ID, tr.TS
		}
	}
}

// The price path is continuous across regime boundaries (no jumps > $5/tick).
func TestPriceContinuity(t *testing.T) {
	for n := n0; n < n0+2*CycleTicks; n++ {
		if d := m.Mid(n+1) - m.Mid(n); d > 500 || d < -500 {
			t.Fatalf("jump of %.0f cents at tick %d (phase %d)", d, n, n%CycleTicks)
		}
	}
}

func TestCandlesAggregateLikeLive(t *testing.T) {
	cs := m.Candles(n0, n0+2*CycleTicks, 60_000)
	if len(cs) < 2 {
		t.Fatalf("got %d candles", len(cs))
	}
	var vol int64
	for n := n0; n < n0+2*CycleTicks; n++ {
		for _, tr := range m.Trades(n) {
			vol += tr.Qty
		}
	}
	var got int64
	for _, c := range cs {
		got += c.Volume
		if c.High < max(c.Open, c.Close) || c.Low > min(c.Open, c.Close) {
			t.Fatalf("inconsistent candle %+v", c)
		}
	}
	if got != vol {
		t.Fatalf("volume %d != %d", got, vol)
	}
}

func TestAggregate(t *testing.T) {
	cs := m.Candles(n0, n0+10*CycleTicks, 60_000)
	agg := Aggregate(cs, 300_000)
	var a, b int64
	for _, c := range cs {
		a += c.Volume
	}
	for _, c := range agg {
		b += c.Volume
	}
	if a != b || agg[len(agg)-1].CloseSeq != cs[len(cs)-1].CloseSeq {
		t.Fatal("aggregation lost volume or close seq")
	}
}
