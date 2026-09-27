// Package market is the canonical market state: it consumes the complete
// generated event stream and updates candles, recent trades, LTP/LTQ and the
// order book. It decides *what happened*; the websocket layer only decides
// *when* each client is told.
//
// A single goroutine (Run) writes; REST handlers and websocket hubs read
// consistent views under an RWMutex.
package market

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"cryptofeed/internal/candle"
	"cryptofeed/internal/generator"
	"cryptofeed/internal/model"
	"cryptofeed/internal/orderbook"
	"cryptofeed/internal/trades"
)

var ErrUnknownInterval = errors.New("market: unknown interval")

// Intervals are the supported candle intervals in milliseconds.
var Intervals = map[string]int64{"1m": 60_000, "5m": 300_000}

type Config struct {
	Symbol         string
	BasePrice      int64
	TickSize       int64
	HistoryCandles int
	TradeBuffer    int
	StateRing      int
}

type Market struct {
	cfg Config

	mu     sync.RWMutex
	series map[string]*candle.Series
	trades *trades.Store
	ltp    int64
	ltq    int64

	bookMu sync.RWMutex
	book   *orderbook.Engine

	tradeCount atomic.Uint64
	depthCount atomic.Uint64

	subMu sync.Mutex
	subs  []chan uint32
}

// Subscribe returns a channel that receives each processed market tick. The
// delivery hubs flush on these ticks, so they sample the canonical state
// exactly on the market clock (a slow reader only ever sees the newest tick).
func (m *Market) Subscribe() <-chan uint32 {
	ch := make(chan uint32, 1)
	m.subMu.Lock()
	m.subs = append(m.subs, ch)
	m.subMu.Unlock()
	return ch
}

func (m *Market) publish(n uint32) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	for _, ch := range m.subs {
		select {
		case ch <- n:
		default: // reader is behind: replace the pending tick with the newest
			select {
			case <-ch:
			default:
			}
			ch <- n
		}
	}
}

func New(cfg Config) *Market {
	m := &Market{
		cfg:    cfg,
		series: map[string]*candle.Series{},
		trades: trades.NewStore(cfg.TradeBuffer),
		ltp:    cfg.BasePrice - cfg.BasePrice%cfg.TickSize,
		book:   orderbook.NewEngine(cfg.TickSize, cfg.StateRing),
	}
	for name, ms := range Intervals {
		m.series[name] = candle.NewSeries(name, ms, cfg.HistoryCandles, cfg.StateRing)
	}
	return m
}

func (m *Market) Symbol() string  { return m.cfg.Symbol }
func (m *Market) TickSize() int64 { return m.cfg.TickSize }

// Seed installs the state as of just before live tick processing starts at
// time nowMs: 1m history (oldest first; the last candle may be the partial
// current minute), the chart sequence of that state, the most recent trades
// (oldest first) and the book. Other intervals are aggregated from the 1m
// candles; a candle whose window contains nowMs becomes the active candle.
func (m *Market) Seed(oneMin []model.Candle, nowMs int64, chartSeq uint32, recent []model.Trade, initial model.Book) error {
	m.mu.Lock()
	if n := len(oneMin); n > 0 {
		m.ltp = oneMin[n-1].Close
	}
	for name, ms := range Intervals {
		all := generator.Aggregate(oneMin, ms)
		var active model.Candle
		if n := len(all); n > 0 && all[n-1].Start == nowMs-nowMs%ms {
			active, all = all[n-1], all[:n-1]
		}
		m.series[name].Seed(all, active, chartSeq)
	}
	for _, t := range recent {
		m.trades.Add(t)
		m.ltp, m.ltq = t.Price, t.Qty
	}
	m.mu.Unlock()

	m.bookMu.Lock()
	defer m.bookMu.Unlock()
	return m.book.Apply(initial)
}

// Run consumes generator events until ctx is cancelled or the channel closes.
func (m *Market) Run(ctx context.Context, events <-chan generator.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			m.Process(ev)
		}
	}
}

// Process applies one tick. Every trade updates every interval; client tiers
// never influence this path. All trades of a tick are applied under one lock,
// so readers only ever observe whole ticks (the chart sequence is the tick).
func (m *Market) Process(ev generator.Event) {
	if len(ev.Trades) > 0 {
		m.mu.Lock()
		for _, t := range ev.Trades {
			for _, s := range m.series {
				s.OnTrade(t, ev.Tick)
			}
			m.trades.Add(t)
			m.ltp, m.ltq = t.Price, t.Qty
		}
		m.mu.Unlock()
		m.tradeCount.Add(uint64(len(ev.Trades)))
	}

	if ev.Book != nil {
		m.bookMu.Lock()
		err := m.book.Apply(*ev.Book)
		m.bookMu.Unlock()
		if err != nil {
			slog.Warn("rejected generated book", "err", err)
		} else {
			m.depthCount.Add(1)
		}
	}
	m.publish(ev.Tick)
}

// CandlesView is a consistent snapshot of one interval for REST.
type CandlesView struct {
	Seq     uint32
	LTP     int64
	Candles []model.Candle
	Active  *model.Candle
}

func (m *Market) Candles(interval string, limit int) (CandlesView, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.series[interval]
	if !ok {
		return CandlesView{}, ErrUnknownInterval
	}
	v := CandlesView{Seq: s.Seq(), LTP: m.ltp, Candles: s.History(limit)}
	if a := s.Active(); a.Start != 0 {
		v.Active = &a
	}
	return v, nil
}

// ChartSeq returns the current canonical chart sequence (tick of the latest trade).
func (m *Market) ChartSeq() uint32 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.series["1m"].Seq()
}

// HasChartState reports whether a SYNC to seq on interval can be honoured.
func (m *Market) HasChartState(interval string, seq uint32) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.series[interval]
	if !ok {
		return false
	}
	_, ok = s.StateAt(seq)
	return ok
}

// ChartTransitions returns the coalesced transitions from baseSeq to now.
func (m *Market) ChartTransitions(interval string, baseSeq uint32) ([]candle.Transition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.series[interval]
	if !ok {
		return nil, ErrUnknownInterval
	}
	return s.Transitions(baseSeq)
}

type BookView struct {
	Book model.Book
	LTP  int64
	LTQ  int64
}

func (m *Market) BookSnapshot() BookView {
	m.bookMu.RLock()
	b := m.book.Book()
	m.bookMu.RUnlock()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return BookView{Book: b, LTP: m.ltp, LTQ: m.ltq}
}

func (m *Market) DepthSeq() uint32 {
	m.bookMu.RLock()
	defer m.bookMu.RUnlock()
	return m.book.Book().Seq
}

func (m *Market) HasDepthState(seq uint32) bool {
	m.bookMu.RLock()
	defer m.bookMu.RUnlock()
	_, ok := m.book.StateAt(seq)
	return ok
}

// DepthDelta returns the coalesced delta from baseSeq to the current book.
func (m *Market) DepthDelta(baseSeq uint32) (orderbook.Delta, error) {
	m.bookMu.RLock()
	defer m.bookMu.RUnlock()
	base, ok := m.book.StateAt(baseSeq)
	if !ok {
		return orderbook.Delta{}, orderbook.ErrBaseUnavailable
	}
	return orderbook.Diff(base, m.book.Book())
}

// LatestTrades returns up to n trades, newest first.
func (m *Market) LatestTrades(n int) []model.Trade {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.trades.Latest(n)
}

func (m *Market) TradeRange(from, to int64, limit int) (list []model.Trade, oldest int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.trades.Range(from, to, limit), m.trades.Oldest()
}

type Ticker struct {
	LTP       int64
	LTQ       int64
	Open24h   int64
	High24h   int64
	Low24h    int64
	Volume24h int64
}

// Ticker aggregates the last 24h from 5m candles (plus the active candle).
func (m *Market) Ticker() Ticker {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.series["5m"]
	cs := s.History(288)
	if a := s.Active(); a.Start != 0 {
		cs = append(cs, a)
	}
	t := Ticker{LTP: m.ltp, LTQ: m.ltq}
	if len(cs) > 288 {
		cs = cs[len(cs)-288:]
	}
	for i, c := range cs {
		if i == 0 {
			t.Open24h, t.High24h, t.Low24h = c.Open, c.High, c.Low
		}
		t.High24h = max(t.High24h, c.High)
		t.Low24h = min(t.Low24h, c.Low)
		t.Volume24h += c.Volume
	}
	return t
}

func (m *Market) Counts() (tradesN, depthN uint64) {
	return m.tradeCount.Load(), m.depthCount.Load()
}
