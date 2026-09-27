// Package candle maintains canonical OHLCV candles for one interval.
//
// The chart sequence is the market tick (see generator): the canonical chart
// state's seq is the tick of the last trade applied. The series keeps a ring of
// recent states keyed by that tick so a delivery layer can compute a coalesced
// transition from any recent base sequence to the current one.
//
// Series is not safe for concurrent use; the market package owns locking.
package candle

import (
	"errors"

	"cryptofeed/internal/model"
)

var ErrBaseUnavailable = errors.New("candle: base sequence no longer retained")

type Series struct {
	Name       string
	IntervalMs int64

	maxHistory int
	closed     []model.Candle // oldest first
	active     model.Candle   // Start==0 means none
	lastSeq    uint32
	initial    model.ChartState // seeded state (before any live trade)
	ring       []model.ChartState
}

func NewSeries(name string, intervalMs int64, maxHistory, ringSize int) *Series {
	return &Series{
		Name: name, IntervalMs: intervalMs,
		maxHistory: maxHistory,
		ring:       make([]model.ChartState, ringSize),
	}
}

// Seed installs history and the state at sequence seq. active may be
// zero-valued (no active candle).
func (s *Series) Seed(closed []model.Candle, active model.Candle, seq uint32) {
	s.closed = append([]model.Candle(nil), closed...)
	s.trim()
	s.active = active
	s.active.CloseSeq = seq
	s.lastSeq = seq
	s.initial = model.ChartState{Seq: seq, Candle: s.active}
}

// OnTrade applies one trade that happened at chart sequence (tick) seq.
// Trades must arrive in order; several trades may share a tick, in which case
// the ring keeps the state after the last of them.
func (s *Series) OnTrade(t model.Trade, seq uint32) {
	start := t.TS - t.TS%s.IntervalMs
	switch {
	case s.active.Start == 0 || start > s.active.Start:
		if s.active.Start != 0 {
			s.active.CloseSeq = s.lastSeq
			s.closed = append(s.closed, s.active)
			s.trim()
		}
		s.active = model.Candle{Start: start, Open: t.Price, High: t.Price, Low: t.Price, Close: t.Price, Volume: t.Qty}
	default:
		// A trade stamped slightly before the active start (never happens with a
		// monotonic generator) is folded into the active candle rather than reopening history.
		s.active.High = max(s.active.High, t.Price)
		s.active.Low = min(s.active.Low, t.Price)
		s.active.Close = t.Price
		s.active.Volume += t.Qty
	}
	s.lastSeq = seq
	s.active.CloseSeq = seq
	s.ring[int(seq)%len(s.ring)] = model.ChartState{Seq: seq, Candle: s.active}
}

func (s *Series) trim() {
	if over := len(s.closed) - s.maxHistory; over > 0 {
		s.closed = append(s.closed[:0:0], s.closed[over:]...)
	}
}

func (s *Series) Seq() uint32          { return s.lastSeq }
func (s *Series) Active() model.Candle { return s.active }
func (s *Series) Current() model.ChartState {
	return model.ChartState{Seq: s.lastSeq, Candle: s.active}
}

// History returns up to limit most recent closed candles, oldest first.
func (s *Series) History(limit int) []model.Candle {
	from := max(0, len(s.closed)-limit)
	return append([]model.Candle(nil), s.closed[from:]...)
}

// StateAt returns the canonical state after chart sequence seq.
func (s *Series) StateAt(seq uint32) (model.ChartState, bool) {
	if seq == s.initial.Seq {
		return s.initial, true
	}
	if seq > s.lastSeq {
		return model.ChartState{}, false
	}
	st := s.ring[int(seq)%len(s.ring)]
	return st, st.Seq == seq
}

// Transition moves a client's active candle from BaseSeq to Seq.
// If Start is newer than the client's active candle, Delta holds absolute
// values for a fresh candle (a delta from zero); otherwise Delta is added to
// the client's candle. Order of Delta: open, high, low, close, volume.
type Transition struct {
	BaseSeq uint32
	Seq     uint32
	Start   int64
	Delta   [5]int64
}

// Transitions returns the ordered transitions that take a client from the
// canonical state at baseSeq to the current state. Crossing a candle boundary
// yields one transition finalising the old candle and one per newer candle.
func (s *Series) Transitions(baseSeq uint32) ([]Transition, error) {
	cur := s.Current()
	if baseSeq == cur.Seq {
		return nil, nil
	}
	base, ok := s.StateAt(baseSeq)
	if !ok {
		return nil, ErrBaseUnavailable
	}
	if base.Candle.Start == cur.Candle.Start {
		return []Transition{diff(baseSeq, cur.Seq, cur.Candle.Start, base.Candle, cur.Candle)}, nil
	}

	var out []Transition
	prev := baseSeq
	if base.Candle.Start != 0 {
		final, ok := s.closedAt(base.Candle.Start)
		if !ok {
			return nil, ErrBaseUnavailable
		}
		if final.CloseSeq > baseSeq {
			out = append(out, diff(baseSeq, final.CloseSeq, final.Start, base.Candle, final))
			prev = final.CloseSeq
		}
	}
	for _, c := range s.closed {
		if c.Start > base.Candle.Start && c.CloseSeq > prev {
			out = append(out, diff(prev, c.CloseSeq, c.Start, model.Candle{}, c))
			prev = c.CloseSeq
		}
	}
	out = append(out, diff(prev, cur.Seq, cur.Candle.Start, model.Candle{}, cur.Candle))
	return out, nil
}

func (s *Series) closedAt(start int64) (model.Candle, bool) {
	for i := len(s.closed) - 1; i >= 0; i-- {
		if s.closed[i].Start == start {
			return s.closed[i], true
		}
		if s.closed[i].Start < start {
			break
		}
	}
	return model.Candle{}, false
}

func diff(base, seq uint32, start int64, from, to model.Candle) Transition {
	return Transition{BaseSeq: base, Seq: seq, Start: start, Delta: [5]int64{
		to.Open - from.Open, to.High - from.High, to.Low - from.Low, to.Close - from.Close, to.Volume - from.Volume,
	}}
}
