// Package model holds the plain data types shared by the market engine,
// the wire protocol and the API. Prices and quantities are fixed-point integers;
// floating point is never used for market values.
package model

const (
	PriceScale = 100     // 1 unit = 0.01 USDT
	QtyScale   = 1000000 // 1 unit = 0.000001 BTC
	Levels     = 10      // depth levels per side
)

type Side int8

const (
	Buy  Side = 1
	Sell Side = -1
)

// Trade is one generated execution. ID is the unambiguous, monotonically
// increasing ordering identifier and doubles as the canonical chart sequence.
type Trade struct {
	ID    uint32 `json:"id"`
	TS    int64  `json:"ts"` // unix ms
	Price int64  `json:"price"`
	Qty   int64  `json:"qty"`
	Side  Side   `json:"side"`
}

type Level struct {
	Price int64 `json:"price"`
	Qty   int64 `json:"qty"`
}

// Book is the top-of-book view. Levels sit on a contiguous tick grid:
// Bids[i].Price = best bid - i*tick, Asks[i].Price = best ask + i*tick.
type Book struct {
	Seq  uint32
	Bids [Levels]Level
	Asks [Levels]Level
}

func (b Book) BestBid() int64 { return b.Bids[0].Price }
func (b Book) BestAsk() int64 { return b.Asks[0].Price }

// Candle is one OHLCV bar. CloseSeq is the chart sequence (trade id) of the last
// trade included; it is 0 for synthetic startup history.
type Candle struct {
	Start    int64  `json:"t"` // unix ms, interval-aligned
	Open     int64  `json:"o"`
	High     int64  `json:"h"`
	Low      int64  `json:"l"`
	Close    int64  `json:"c"`
	Volume   int64  `json:"v"`
	CloseSeq uint32 `json:"-"`
}

// ChartState is the canonical active-candle state after chart sequence Seq.
// Start == 0 means "no active candle yet".
type ChartState struct {
	Seq    uint32
	Candle Candle
}
