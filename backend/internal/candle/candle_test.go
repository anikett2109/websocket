package candle

import (
	"math/rand"
	"testing"

	"cryptofeed/internal/model"
)

func tr(id uint32, ts, price, qty int64) model.Trade {
	return model.Trade{ID: id, TS: ts, Price: price, Qty: qty, Side: model.Buy}
}

func TestOHLCV(t *testing.T) {
	s := NewSeries("1m", 60_000, 100, 64)
	s.OnTrade(tr(1, 60_000, 100, 1), 1)
	s.OnTrade(tr(2, 61_000, 105, 2), 2)
	s.OnTrade(tr(3, 62_000, 95, 3), 3)
	s.OnTrade(tr(4, 63_000, 101, 4), 4)
	want := model.Candle{Start: 60_000, Open: 100, High: 105, Low: 95, Close: 101, Volume: 10}
	if a := s.Active(); a.Start != want.Start || a.Open != want.Open || a.High != want.High ||
		a.Low != want.Low || a.Close != want.Close || a.Volume != want.Volume {
		t.Fatalf("active=%+v want %+v", a, want)
	}
	// Next minute finalises the candle and opens a fresh one from the new trade only.
	s.OnTrade(tr(5, 120_000, 110, 7), 5)
	h := s.History(10)
	if len(h) != 1 || h[0].Close != 101 || h[0].CloseSeq != 4 {
		t.Fatalf("closed=%+v", h)
	}
	if a := s.Active(); a.Open != 110 || a.High != 110 || a.Low != 110 || a.Volume != 7 {
		t.Fatalf("new candle must not inherit previous values: %+v", a)
	}
}

// TestCoalescedDeliveryMatchesCanonical is the core guarantee of adaptive
// delivery: however coarsely transitions are coalesced (i.e. whatever the
// tier), a client applying them ends with exactly the canonical candles.
func TestCoalescedDeliveryMatchesCanonical(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var trades []model.Trade
	ts, price := int64(1_000_000), int64(6_500_000)
	for id := uint32(1); id <= 6000; id++ {
		ts += int64(20 + rng.Intn(80)) // ~50ms apart => ~5 minutes of trading
		price += int64(rng.Intn(5)-2) * 50
		trades = append(trades, tr(id, ts, price, int64(100+rng.Intn(10000))))
	}

	// 3 trades share each tick (like the burst regime); the delivery layer only
	// ever observes whole ticks, so flushes happen on tick boundaries.
	for _, every := range []int{1, 2, 5, 20, 400, 1500} { // ticks per flush
		s := NewSeries("1m", 60_000, 1000, 4096)
		local := &Local{}
		for i, trd := range trades {
			tick := uint32(i/3 + 1)
			s.OnTrade(trd, tick)
			lastOfTick := i%3 == 2 || i == len(trades)-1
			if !lastOfTick || (int(tick)%every != 0 && i != len(trades)-1) {
				continue
			}
			steps, err := s.Transitions(local.Seq)
			if err != nil {
				t.Fatalf("every=%d: transitions: %v", every, err)
			}
			for _, st := range steps {
				if err := local.Apply(st); err != nil {
					t.Fatalf("every=%d: apply %+v: %v", every, st, err)
				}
			}
		}
		canon := s.History(1000)
		if len(local.Closed) != len(canon) {
			t.Fatalf("every=%d: closed %d candles, canonical %d", every, len(local.Closed), len(canon))
		}
		for i := range canon {
			if !sameOHLCV(local.Closed[i], canon[i]) {
				t.Fatalf("every=%d: candle %d local=%+v canonical=%+v", every, i, local.Closed[i], canon[i])
			}
		}
		if !sameOHLCV(local.Active, s.Active()) || local.Seq != s.Seq() {
			t.Fatalf("every=%d: active local=%+v canonical=%+v", every, local.Active, s.Active())
		}
	}
}

func TestBaseMismatchRejected(t *testing.T) {
	s := NewSeries("1m", 60_000, 10, 64)
	for i := uint32(1); i <= 5; i++ {
		s.OnTrade(tr(i, int64(i)*1000, 100, 1), i)
	}
	steps, _ := s.Transitions(3)
	l := &Local{Seq: 2}
	if err := l.Apply(steps[0]); err != ErrSeqMismatch {
		t.Fatalf("want ErrSeqMismatch, got %v", err)
	}
}

func TestEvictedBase(t *testing.T) {
	s := NewSeries("1m", 60_000, 10, 8)
	for i := uint32(1); i <= 20; i++ {
		s.OnTrade(tr(i, int64(i)*1000, 100, 1), i)
	}
	if _, err := s.Transitions(3); err != ErrBaseUnavailable {
		t.Fatalf("want ErrBaseUnavailable, got %v", err)
	}
}

func sameOHLCV(a, b model.Candle) bool {
	return a.Start == b.Start && a.Open == b.Open && a.High == b.High && a.Low == b.Low && a.Close == b.Close && a.Volume == b.Volume
}
