// Package protocol implements the binary WebSocket wire format.
//
// All packets start with a 15-byte little-endian header:
//
//	type uint8 | length uint16 (whole packet) | seq uint32 | timestamp int64
//
// Packets are serialised field by field with encoding/binary so Go struct
// alignment never leaks into the wire format.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"

	"cryptofeed/internal/candle"
	"cryptofeed/internal/model"
	"cryptofeed/internal/orderbook"
)

const (
	TypeChartDelta  uint8 = 1
	TypeDepthDelta  uint8 = 2
	TypeTradeUpdate uint8 = 3
	TypePing        uint8 = 4
	TypePong        uint8 = 5
)

const (
	HeaderSize      = 15
	ChartDeltaSize  = HeaderSize + 6*8                          // 63
	DepthDeltaSize  = HeaderSize + 4 + 8 + 8 + 2*model.Levels*4 // 115
	TradeUpdateSize = HeaderSize + 8 + 10*12                    // 143
	PingSize        = HeaderSize                                // 15
	PongSize        = HeaderSize + 4                            // 19: + server hold (µs)
	TradesPerUpdate = 10
)

var le = binary.LittleEndian

var ErrMalformed = errors.New("protocol: malformed packet")

type Header struct {
	Type   uint8
	Length uint16
	Seq    uint32
	TS     int64
}

func putHeader(b []byte, typ uint8, seq uint32, ts int64) {
	b[0] = typ
	le.PutUint16(b[1:], uint16(len(b)))
	le.PutUint32(b[3:], seq)
	le.PutUint64(b[7:], uint64(ts))
}

func ReadHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("%w: %d bytes", ErrMalformed, len(b))
	}
	h := Header{Type: b[0], Length: le.Uint16(b[1:]), Seq: le.Uint32(b[3:]), TS: int64(le.Uint64(b[7:]))}
	if int(h.Length) != len(b) {
		return h, fmt.Errorf("%w: length field %d != %d", ErrMalformed, h.Length, len(b))
	}
	return h, nil
}

// EncodeChartDelta: header.seq = chart seq (trade id), header.ts = candle start.
// The first int64 payload slot carries baseSeq (it replaces ltpDelta, which is
// redundant because the active candle's close is always the LTP).
func EncodeChartDelta(t candle.Transition) []byte {
	b := make([]byte, ChartDeltaSize)
	putHeader(b, TypeChartDelta, t.Seq, t.Start)
	le.PutUint64(b[15:], uint64(t.BaseSeq))
	for i, d := range t.Delta {
		le.PutUint64(b[23+i*8:], uint64(d))
	}
	return b
}

func DecodeChartDelta(b []byte) (candle.Transition, error) {
	h, err := ReadHeader(b)
	if err != nil || h.Type != TypeChartDelta || len(b) != ChartDeltaSize {
		return candle.Transition{}, ErrMalformed
	}
	t := candle.Transition{BaseSeq: uint32(le.Uint64(b[15:])), Seq: h.Seq, Start: h.TS}
	for i := range t.Delta {
		t.Delta[i] = int64(le.Uint64(b[23+i*8:]))
	}
	return t, nil
}

// EncodeDepthDelta: header.seq = depthSeq; payload = baseSeq u32, bestBid i64,
// bestAsk i64, 10 bid qty deltas i32, 10 ask qty deltas i32.
func EncodeDepthDelta(d orderbook.Delta, ts int64) []byte {
	b := make([]byte, DepthDeltaSize)
	putHeader(b, TypeDepthDelta, d.Seq, ts)
	le.PutUint32(b[15:], d.BaseSeq)
	le.PutUint64(b[19:], uint64(d.BestBid))
	le.PutUint64(b[27:], uint64(d.BestAsk))
	for i := 0; i < model.Levels; i++ {
		le.PutUint32(b[35+i*4:], uint32(d.Bid[i]))
		le.PutUint32(b[75+i*4:], uint32(d.Ask[i]))
	}
	return b
}

func DecodeDepthDelta(b []byte) (orderbook.Delta, error) {
	h, err := ReadHeader(b)
	if err != nil || h.Type != TypeDepthDelta || len(b) != DepthDeltaSize {
		return orderbook.Delta{}, ErrMalformed
	}
	d := orderbook.Delta{
		Seq: h.Seq, BaseSeq: le.Uint32(b[15:]),
		BestBid: int64(le.Uint64(b[19:])), BestAsk: int64(le.Uint64(b[27:])),
	}
	for i := 0; i < model.Levels; i++ {
		d.Bid[i] = int32(le.Uint32(b[35+i*4:]))
		d.Ask[i] = int32(le.Uint32(b[75+i*4:]))
	}
	return d, nil
}

// EncodeTradeUpdate encodes exactly 10 trades, newest first.
// header.seq = newest trade id (ids are consecutive: seq, seq-1, ... seq-9),
// header.ts = newest trade time, LTP = newest price.
// Per trade: priceDelta = price - LTP, quantity (sign = aggressor side, + buy / - sell),
// timeDelta = trade ts - header ts (ms, <= 0).
func EncodeTradeUpdate(ts []model.Trade) ([]byte, error) {
	if len(ts) != TradesPerUpdate {
		return nil, fmt.Errorf("protocol: need %d trades, got %d", TradesPerUpdate, len(ts))
	}
	b := make([]byte, TradeUpdateSize)
	newest := ts[0]
	putHeader(b, TypeTradeUpdate, newest.ID, newest.TS)
	le.PutUint64(b[15:], uint64(newest.Price))
	for i, t := range ts {
		off := 23 + i*12
		le.PutUint32(b[off:], uint32(int32(t.Price-newest.Price)))
		le.PutUint32(b[off+4:], uint32(int32(t.Qty)*int32(t.Side)))
		le.PutUint32(b[off+8:], uint32(int32(t.TS-newest.TS)))
	}
	return b, nil
}

func DecodeTradeUpdate(b []byte) ([]model.Trade, error) {
	h, err := ReadHeader(b)
	if err != nil || h.Type != TypeTradeUpdate || len(b) != TradeUpdateSize {
		return nil, ErrMalformed
	}
	ltp := int64(le.Uint64(b[15:]))
	out := make([]model.Trade, TradesPerUpdate)
	for i := range out {
		off := 23 + i*12
		q := int64(int32(le.Uint32(b[off+4:])))
		side := model.Buy
		if q < 0 {
			side, q = model.Sell, -q
		}
		out[i] = model.Trade{
			ID:    h.Seq - uint32(i),
			Price: ltp + int64(int32(le.Uint32(b[off:]))),
			Qty:   q, Side: side,
			TS: h.TS + int64(int32(le.Uint32(b[off+8:]))),
		}
	}
	return out, nil
}

// EncodePong echoes a PING's seq and client timestamp and appends the server
// hold time in microseconds: how long the server kept this probe beyond any
// intentionally simulated delay (CPU scheduling, timer lateness). The client
// subtracts it from the RTT, as NTP subtracts server processing time
// (delay = (t4−t1) − (t3−t2)), so the tier reflects the client's network, not
// the server's CPU.
func EncodePong(seq uint32, ts int64, holdMicros uint32) []byte {
	b := make([]byte, PongSize)
	putHeader(b, TypePong, seq, ts)
	le.PutUint32(b[15:], holdMicros)
	return b
}

// EncodeProbe builds a PING: a bare header. For PING/PONG, seq is the probe
// sequence and ts is the sender's clock (echoed back unchanged in PONG).
func EncodeProbe(typ uint8, seq uint32, ts int64) []byte {
	b := make([]byte, PingSize)
	putHeader(b, typ, seq, ts)
	return b
}

// SplitFrame splits one WebSocket binary frame into its packets. A hub batches
// every packet due on the same tick into one frame; the header length field
// makes packets self-delimiting.
func SplitFrame(b []byte) ([][]byte, error) {
	var out [][]byte
	for len(b) > 0 {
		if len(b) < HeaderSize {
			return out, fmt.Errorf("%w: %d trailing bytes", ErrMalformed, len(b))
		}
		n := int(le.Uint16(b[1:]))
		if n < HeaderSize || n > len(b) {
			return out, fmt.Errorf("%w: packet length %d with %d bytes left", ErrMalformed, n, len(b))
		}
		out = append(out, b[:n])
		b = b[n:]
	}
	return out, nil
}
