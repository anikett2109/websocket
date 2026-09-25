package orderbook

import (
	"testing"

	"cryptofeed/internal/generator"
)

const tick = 50

func newGen() *generator.Generator {
	return generator.New(generator.Config{Seed: 42, StartPrice: 6_500_000, TickSize: tick, DepthSkipPct: 0})
}

// TestSnapshotThenDeltas: snapshot at seq N, then deltas N->N+1->N+2->N+3
// reproduce the canonical book exactly.
func TestSnapshotThenDeltas(t *testing.T) {
	g := newGen()
	e := NewEngine(tick, 256)
	for i := 0; i < 100; i++ {
		if err := e.Apply(*g.Step(int64(i)).Book); err != nil {
			t.Fatal(err)
		}
	}
	local := e.Book() // REST snapshot, seq 100
	if local.Seq != 100 {
		t.Fatalf("seq=%d", local.Seq)
	}
	for i := 0; i < 3; i++ {
		base := e.Book()
		if err := e.Apply(*g.Step(int64(100 + i)).Book); err != nil {
			t.Fatal(err)
		}
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

// TestCoalescedDepthDeltas: a slow tier sends one delta spanning many canonical
// updates, including price-level shifts; the result must still be exact.
func TestCoalescedDepthDeltas(t *testing.T) {
	for _, every := range []int{1, 3, 5, 17} {
		g := newGen()
		e := NewEngine(tick, 256)
		_ = e.Apply(g.Book())
		local := e.Book()
		shifts := 0
		for i := 1; i <= 2000; i++ {
			if err := e.Apply(*g.Step(int64(i)).Book); err != nil {
				t.Fatal(err)
			}
			if i%every != 0 {
				continue
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
				t.Fatalf("every=%d i=%d diverged", every, i)
			}
		}
		if shifts == 0 {
			t.Fatalf("every=%d: test never exercised a price shift", every)
		}
	}
}

// TestGapDetected: 100, 101, 103 -> the 103 delta's base (102) does not match
// the local seq (101) and must be rejected so the client re-snapshots.
func TestGapDetected(t *testing.T) {
	g := newGen()
	e := NewEngine(tick, 256)
	for i := 0; i < 100; i++ {
		_ = e.Apply(*g.Step(int64(i)).Book)
	}
	local := e.Book()
	var deltas []Delta
	for i := 0; i < 3; i++ {
		base := e.Book()
		_ = e.Apply(*g.Step(int64(100 + i)).Book)
		d, _ := Diff(base, e.Book())
		deltas = append(deltas, d)
	}
	local, err := ApplyDelta(local, deltas[0], tick) // 100 -> 101
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyDelta(local, deltas[2], tick); err != ErrSeqMismatch { // 102 -> 103
		t.Fatalf("want ErrSeqMismatch on gap, got %v", err)
	}
	// Recovery: fresh snapshot replaces the local book.
	local = e.Book()
	if local.Seq != 103 {
		t.Fatalf("snapshot seq=%d", local.Seq)
	}
}

func TestValidateRejectsCrossedBook(t *testing.T) {
	b := newGen().Book()
	b.Asks[0].Price = b.Bids[0].Price
	if Validate(b, tick) == nil {
		t.Fatal("crossed book accepted")
	}
	b = newGen().Book()
	b.Bids[3].Qty = 0
	if Validate(b, tick) == nil {
		t.Fatal("empty level accepted")
	}
}
