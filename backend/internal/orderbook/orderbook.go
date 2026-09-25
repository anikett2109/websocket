// Package orderbook maintains the authoritative server-side top-10 book,
// its canonical depth sequence, and the delta encoding between two book states.
//
// Engine is not safe for concurrent use; the market package owns locking.
package orderbook

import (
	"errors"
	"fmt"
	"math"

	"cryptofeed/internal/model"
)

var (
	ErrBaseUnavailable = errors.New("orderbook: base sequence no longer retained")
	ErrSeqMismatch     = errors.New("orderbook: base sequence mismatch")
)

type Engine struct {
	tick int64
	book model.Book
	ring []model.Book
}

func NewEngine(tick int64, ringSize int) *Engine {
	return &Engine{tick: tick, ring: make([]model.Book, ringSize)}
}

// Apply validates a proposed book, assigns the next depthSeq and makes it canonical.
func (e *Engine) Apply(b model.Book) error {
	if err := Validate(b, e.tick); err != nil {
		return err
	}
	b.Seq = e.book.Seq + 1
	e.book = b
	e.ring[int(b.Seq)%len(e.ring)] = b
	return nil
}

func (e *Engine) Book() model.Book { return e.book }

func (e *Engine) StateAt(seq uint32) (model.Book, bool) {
	if seq > e.book.Seq || seq == 0 {
		return model.Book{}, false
	}
	b := e.ring[int(seq)%len(e.ring)]
	return b, b.Seq == seq
}

// Validate enforces the book invariants the wire format relies on:
// ten positive levels per side on a contiguous tick grid and bestBid < bestAsk.
func Validate(b model.Book, tick int64) error {
	if b.BestBid() >= b.BestAsk() {
		return fmt.Errorf("orderbook: crossed book bid=%d ask=%d", b.BestBid(), b.BestAsk())
	}
	for i := 0; i < model.Levels; i++ {
		if b.Bids[i].Price != b.BestBid()-int64(i)*tick || b.Asks[i].Price != b.BestAsk()+int64(i)*tick {
			return fmt.Errorf("orderbook: level %d off tick grid", i)
		}
		if b.Bids[i].Qty <= 0 || b.Asks[i].Qty <= 0 {
			return fmt.Errorf("orderbook: level %d has non-positive quantity", i)
		}
	}
	return nil
}

// Delta is the wire-level transition between two book states. Level prices are
// implied by the anchors: bid[i] = BestBid - i*tick, ask[i] = BestAsk + i*tick.
// Each quantity delta is relative to the client's quantity at that same price
// (zero if the client had no level at that price).
type Delta struct {
	BaseSeq uint32
	Seq     uint32
	BestBid int64
	BestAsk int64
	Bid     [model.Levels]int32
	Ask     [model.Levels]int32
}

func qtyAt(levels *[model.Levels]model.Level, price int64) int64 {
	for _, l := range levels {
		if l.Price == price {
			return l.Qty
		}
	}
	return 0
}

// Diff computes the delta that takes a client from base to cur.
func Diff(base, cur model.Book) (Delta, error) {
	d := Delta{BaseSeq: base.Seq, Seq: cur.Seq, BestBid: cur.BestBid(), BestAsk: cur.BestAsk()}
	for i := 0; i < model.Levels; i++ {
		b := cur.Bids[i].Qty - qtyAt(&base.Bids, cur.Bids[i].Price)
		a := cur.Asks[i].Qty - qtyAt(&base.Asks, cur.Asks[i].Price)
		if b > math.MaxInt32 || b < math.MinInt32 || a > math.MaxInt32 || a < math.MinInt32 {
			return Delta{}, errors.New("orderbook: quantity delta overflows int32")
		}
		d.Bid[i], d.Ask[i] = int32(b), int32(a)
	}
	return d, nil
}

// ApplyDelta is the reference client-side reconstruction, mirroring the web app.
func ApplyDelta(local model.Book, d Delta, tick int64) (model.Book, error) {
	if d.BaseSeq != local.Seq {
		return local, ErrSeqMismatch
	}
	next := model.Book{Seq: d.Seq}
	for i := 0; i < model.Levels; i++ {
		bp := d.BestBid - int64(i)*tick
		ap := d.BestAsk + int64(i)*tick
		next.Bids[i] = model.Level{Price: bp, Qty: qtyAt(&local.Bids, bp) + int64(d.Bid[i])}
		next.Asks[i] = model.Level{Price: ap, Qty: qtyAt(&local.Asks, ap) + int64(d.Ask[i])}
	}
	return next, nil
}
