package protocol

import (
	"testing"

	"cryptofeed/internal/candle"
	"cryptofeed/internal/model"
	"cryptofeed/internal/orderbook"
)

func TestPacketSizes(t *testing.T) {
	if ChartDeltaSize != 63 || DepthDeltaSize != 115 || TradeUpdateSize != 143 || PingSize != 15 {
		t.Fatalf("sizes chart=%d depth=%d trade=%d ping=%d", ChartDeltaSize, DepthDeltaSize, TradeUpdateSize, PingSize)
	}
}

func TestChartRoundTrip(t *testing.T) {
	in := candle.Transition{BaseSeq: 41, Seq: 45, Start: 1_700_000_040_000, Delta: [5]int64{0, 150, -50, 100, 123456}}
	b := EncodeChartDelta(in)
	if len(b) != 63 {
		t.Fatalf("len=%d", len(b))
	}
	out, err := DecodeChartDelta(b)
	if err != nil || out != in {
		t.Fatalf("got %+v err %v", out, err)
	}
}

func TestDepthRoundTrip(t *testing.T) {
	in := orderbook.Delta{BaseSeq: 7, Seq: 9, BestBid: 6_500_000, BestAsk: 6_500_050}
	for i := range in.Bid {
		in.Bid[i] = int32(i*1000 - 3000)
		in.Ask[i] = int32(-i * 77)
	}
	out, err := DecodeDepthDelta(EncodeDepthDelta(in, 123))
	if err != nil || out != in {
		t.Fatalf("got %+v err %v", out, err)
	}
}

func TestTradeRoundTrip(t *testing.T) {
	var in []model.Trade
	for i := 0; i < 10; i++ {
		side := model.Buy
		if i%3 == 0 {
			side = model.Sell
		}
		in = append(in, model.Trade{ID: uint32(500 - i), TS: 1_000_000 - int64(i*50), Price: 6_500_000 + int64(i%4-2)*50, Qty: int64(1000 + i), Side: side})
	}
	b, err := EncodeTradeUpdate(in)
	if err != nil || len(b) != 143 {
		t.Fatalf("len=%d err=%v", len(b), err)
	}
	out, err := DecodeTradeUpdate(b)
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		if in[i] != out[i] {
			t.Fatalf("trade %d: %+v != %+v", i, out[i], in[i])
		}
	}
	if _, err := EncodeTradeUpdate(in[:9]); err == nil {
		t.Fatal("must require exactly 10 trades")
	}
}

func TestMalformed(t *testing.T) {
	cases := [][]byte{
		nil,
		{1, 2, 3},
		EncodeChartDelta(candle.Transition{})[:40], // truncated
		append(EncodeProbe(TypePing, 1, 2), 0),     // length field mismatch
	}
	for i, b := range cases {
		if _, err := ReadHeader(b); err == nil {
			if _, err := DecodeChartDelta(b); err == nil {
				t.Fatalf("case %d accepted", i)
			}
		}
	}
	// Right size but wrong type.
	if _, err := DecodeChartDelta(EncodeProbe(TypePing, 1, 1)); err == nil {
		t.Fatal("ping decoded as chart")
	}
}
