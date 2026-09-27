package orderbook

import (
	"testing"

	"cryptofeed/internal/generator"
)

const tick = 50

var mkt = generator.Model{Base: 6_500_000, Tick: tick}

// n0 is the start of a regime cycle; its first 800 ticks change the book every tick.
const n0 = uint32(1200 * 400_000)

// feed applies every book change in ticks [from, to) to e.
func feed(t *testing.T, e *Engine, from, to uint32) {
	t.Helper()
	for n := from; n < to; n++ {
		if ev := mkt.Event(n); ev.Book != nil {
			if err := e.Apply(*ev.Book); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestSnapshotThenDeltas: snapshot at seq S, then deltas S->S+1->S+2->S+3
// reproduce the canonical book exactly.
func TestSnapshotThenDeltas(t *testing.T) {
	e := NewEngine(tick, 256)
	feed(t, e, n0, n0+101)
	local := e.Book() // REST snapshot
	if local.Seq != n0+100 {
		t.Fatalf("seq=%d", local.Seq)
	}
	for n := n0 + 101; n <= n0+103; n++ {
		base := e.Book()
		feed(t, e, n, n+1)
		d, err := Diff(base, e.Book())
		if err != nil {
			t.Fatal(err)
		}
		if local, err = ApplyDelta(local, d, tick); err != nil {
			t.Fatal(err)
		}
	}
	if local != e.Book() {
		t.Fatalf("local book diverged:\nlocal=%+v\ncanon=%+v", local, e.Book())
	}
}

// TestCoalescedDepthDeltas: a slower tier sends one delta spanning several
// canonical states, including price-level shifts and the sparse quiet regime;
// the result must still be exact. every = tier depth interval in ticks.
func TestCoalescedDepthDeltas(t *testing.T) {
	for _, every := range []uint32{1, 3, 10} { // FULL, DEGRADED, MINIMAL
		e := NewEngine(tick, 4096)
		feed(t, e, n0, n0+1)
		local := e.Book()
		shifts := 0
		for n := n0 + 1; n < n0+2*generator.CycleTicks; n++ {
			feed(t, e, n, n+1)
			if n%every != 0 || e.Book().Seq == local.Seq {
				continue // not due, or nothing changed: nothing is sent
			}
			base, ok := e.StateAt(local.Seq)
			if !ok {
				t.Fatalf("base %d evicted", local.Seq)
			}
			if base.BestBid() != e.Book().BestBid() {
				shifts++
			}
			d, err := Diff(base, e.Book())
			if err != nil {
				t.Fatal(err)
			}
			if local, err = ApplyDelta(local, d, tick); err != nil {
				t.Fatal(err)
			}
			if local != e.Book() {
				t.Fatalf("every=%d tick=%d diverged", every, n)
			}
		}
		if shifts == 0 {
			t.Fatalf("every=%d: test never exercised a price shift", every)
		}
	}
}

// TestGapDetected: S, S+1, S+3 -> the S+3 delta's base (S+2) does not match the
// local seq (S+1) and must be rejected so the client re-snapshots.
func TestGapDetected(t *testing.T) {
	e := NewEngine(tick, 256)
	feed(t, e, n0, n0+100)
	local := e.Book()
	var deltas []Delta
	for n := n0 + 100; n < n0+103; n++ {
		base := e.Book()
		feed(t, e, n, n+1)
		d, _ := Diff(base, e.Book())
		deltas = append(deltas, d)
	}
	local, err := ApplyDelta(local, deltas[0], tick)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyDelta(local, deltas[2], tick); err != ErrSeqMismatch {
		t.Fatalf("want ErrSeqMismatch on gap, got %v", err)
	}
	if local = e.Book(); local.Seq != n0+102 { // recovery: fresh snapshot
		t.Fatalf("snapshot seq=%d", local.Seq)
	}
}

func TestApplyRejectsOldSeq(t *testing.T) {
	e := NewEngine(tick, 16)
	feed(t, e, n0, n0+5)
	if err := e.Apply(mkt.Book(n0 + 2)); err == nil {
		t.Fatal("out-of-order book accepted")
	}
}

func TestValidateRejectsCrossedBook(t *testing.T) {
	b := mkt.Book(n0)
	b.Asks[0].Price = b.Bids[0].Price
	if Validate(b, tick) == nil {
		t.Fatal("crossed book accepted")
	}
	b = mkt.Book(n0)
	b.Bids[3].Qty = 0
	if Validate(b, tick) == nil {
		t.Fatal("empty level accepted")
	}
}
