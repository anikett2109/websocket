// Package generator produces the synthetic market as a pure function of time.
//
// Everything is driven by one clock: tick n = ⌊(t − Epoch) / 50 ms⌋. What
// happens at tick n (how many trades, their ids, prices and sizes, whether the
// book changes and what it looks like) is a closed-form function of n. There
// is no random number generator and no hidden state, so:
//
//   - the feed is exactly repeatable (tests can assert exact values),
//   - a restarted process resumes the same trade ids, sequences and prices,
//   - history and live data are continuous by construction.
//
// A 60 s cycle of 1200 ticks has three regimes chosen to demonstrate the
// cases the assignment calls out:
//
//	normal  ticks   0–599  (30 s)  1 trade / tick       = 20 trades/s
//	burst   ticks 600–799  (10 s)  3 trades / tick      = 60 trades/s  (coalescing)
//	quiet   ticks 800–1199 (20 s)  1 trade / 20 ticks   =  1 trade/s   (no invented updates)
//
// giving 600 + 600 + 20 = 1220 trades per cycle (20.33/s). The book changes
// every tick in normal and burst and every 5th tick in quiet: 800 + 80 = 880
// book states per cycle (14.67/s).
package generator

import "time"

const (
	TickMs     = 50   // τ: the market clock and the unit of every delivery interval
	CycleTicks = 1200 // 60 s regime cycle
	NormalEnd  = 600  // phase < 600: normal
	BurstEnd   = 800  // 600 <= phase < 800: burst; >= 800: quiet

	BurstTradesPerTick = 3
	QuietTradeEvery    = 20 // ticks between trades in quiet (1/s)
	QuietBookEvery     = 5  // ticks between book changes in quiet (4/s)

	TradesPerCycle     = NormalEnd + BurstTradesPerTick*(BurstEnd-NormalEnd) + (CycleTicks-BurstEnd)/QuietTradeEvery // 1220
	BookStatesPerCycle = BurstEnd + (CycleTicks-BurstEnd)/QuietBookEvery                                             // 880
)

// Epoch is tick 0: 2026-01-01T00:00:00Z. uint32 tick numbers last ~6.8 years
// from it at 20 ticks/s; trade ids (20.33/s) about as long.
var Epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()

// TickAt returns the tick containing unix-ms time ms.
func TickAt(ms int64) uint32 { return uint32((ms - Epoch) / TickMs) }

// TickTime returns the unix-ms start of tick n.
func TickTime(n uint32) int64 { return Epoch + int64(n)*TickMs }

func phase(n uint32) uint32 { return n % CycleTicks }

// TradesAt is the number of trades generated in tick n.
func TradesAt(n uint32) int {
	switch p := phase(n); {
	case p < NormalEnd:
		return 1
	case p < BurstEnd:
		return BurstTradesPerTick
	case (p-BurstEnd)%QuietTradeEvery == 0:
		return 1
	default:
		return 0
	}
}

// tradesBeforePhase counts trades in ticks [0, p) of a cycle.
func tradesBeforePhase(p uint32) uint32 {
	switch {
	case p <= NormalEnd:
		return p
	case p <= BurstEnd:
		return NormalEnd + BurstTradesPerTick*(p-NormalEnd)
	default:
		return NormalEnd + BurstTradesPerTick*(BurstEnd-NormalEnd) + (p-BurstEnd+QuietTradeEvery-1)/QuietTradeEvery
	}
}

// TradesBefore is the closed-form count of trades in ticks [0, n). Trade j
// (0-based) of tick n has id TradesBefore(n)+j+1.
func TradesBefore(n uint32) uint32 {
	return TradesPerCycle*(n/CycleTicks) + tradesBeforePhase(phase(n))
}

// BookChangesAt reports whether the book changes in tick n.
func BookChangesAt(n uint32) bool {
	p := phase(n)
	return p < BurstEnd || (p-BurstEnd)%QuietBookEvery == 0
}

// LastTradeTick is the latest tick <= n that has a trade (the chart sequence
// of the canonical state after tick n).
func LastTradeTick(n uint32) uint32 {
	if p := phase(n); p >= BurstEnd {
		return n - (p-BurstEnd)%QuietTradeEvery
	}
	return n
}

// LastBookTick is the latest tick <= n where the book changed (the depth
// sequence of the canonical book after tick n).
func LastBookTick(n uint32) uint32 {
	if p := phase(n); p >= BurstEnd {
		return n - (p-BurstEnd)%QuietBookEvery
	}
	return n
}
