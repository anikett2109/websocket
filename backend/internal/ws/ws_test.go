package ws

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"cryptofeed/internal/config"
	"cryptofeed/internal/generator"
	"cryptofeed/internal/market"
	"cryptofeed/internal/model"
	"cryptofeed/internal/orderbook"
	"cryptofeed/internal/protocol"
)

var mdl = generator.Model{Base: 6_500_000, Tick: 50}

// seededMarket builds a market whose live processing starts at tick n0,
// 100 ticks into a normal regime (1 trade and 1 book change per tick).
func seededMarket(t *testing.T) (*market.Market, uint32) {
	t.Helper()
	now := generator.TickAt(time.Now().UnixMilli())
	n0 := now - now%generator.CycleTicks + 100
	mkt := market.New(market.Config{Symbol: "BTCUSDT", BasePrice: 6_500_000, TickSize: 50, HistoryCandles: 500, TradeBuffer: 1000, StateRing: 1024})
	var recent []model.Trade
	for n := n0 - 200; n < n0; n++ {
		recent = append(recent, mdl.Trades(n)...)
	}
	past := mdl.Candles(n0-2*generator.CycleTicks, n0, 60_000)
	if err := mkt.Seed(past, generator.TickTime(n0), generator.LastTradeTick(n0-1), recent, mdl.Book(n0-1)); err != nil {
		t.Fatal(err)
	}
	return mkt, n0
}

type harness struct {
	t    *testing.T
	mkt  *market.Market
	conn *websocket.Conn
}

func setup(t *testing.T) *harness {
	t.Helper()
	cfg := config.Load()
	fast := config.TierRates{Depth: 50 * time.Millisecond, Chart: 50 * time.Millisecond, Trade: 50 * time.Millisecond}
	cfg.Full, cfg.Degraded, cfg.Minimal = fast, fast, fast

	mkt, n0 := seededMarket(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv := NewServer(cfg, mkt)
	go srv.Run(ctx)
	go func() { // drive the market 10x faster than real time: one tick per 5 ms
		tk := time.NewTicker(5 * time.Millisecond)
		defer tk.Stop()
		for n := n0; ; n++ {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				mkt.Process(mdl.Event(n))
			}
		}
	}()

	hs := httptest.NewServer(httpHandler(srv))
	t.Cleanup(hs.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(hs.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &harness{t: t, mkt: mkt, conn: conn}
}

// next returns the next message; text messages are decoded into a map.
func (h *harness) next() (bin []byte, txt map[string]any) {
	h.t.Helper()
	_ = h.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	typ, data, err := h.conn.ReadMessage()
	if err != nil {
		h.t.Fatalf("read: %v", err)
	}
	if typ == websocket.BinaryMessage {
		return data, nil
	}
	_ = json.Unmarshal(data, &txt)
	return nil, txt
}

// nextDepth returns the next DEPTH_DELTA packet, looking inside batched frames.
func (h *harness) nextDepth() orderbook.Delta {
	h.t.Helper()
	for {
		b, _ := h.next()
		if b == nil {
			continue
		}
		parts, err := protocol.SplitFrame(b)
		if err != nil {
			h.t.Fatal(err)
		}
		for _, p := range parts {
			if p[0] == protocol.TypeDepthDelta {
				d, err := protocol.DecodeDepthDelta(p)
				if err != nil {
					h.t.Fatal(err)
				}
				return d
			}
		}
	}
}

func (h *harness) waitText(typ string) map[string]any {
	h.t.Helper()
	for {
		if _, m := h.next(); m != nil && m["type"] == typ {
			return m
		}
	}
}

func (h *harness) send(v any) {
	h.t.Helper()
	if err := h.conn.WriteJSON(v); err != nil {
		h.t.Fatal(err)
	}
}

// syncDepth performs REST-snapshot + SYNC and returns the local book.
func (h *harness) syncDepth() model.Book {
	local := h.mkt.BookSnapshot().Book
	h.send(map[string]any{"type": "SYNC", "stream": "depth", "seq": local.Seq})
	m := h.waitText("SYNCED")
	if uint32(m["seq"].(float64)) != local.Seq {
		h.t.Fatalf("SYNCED seq %v != %d", m["seq"], local.Seq)
	}
	return local
}

func TestDepthSyncGapAndRecovery(t *testing.T) {
	h := setup(t)
	h.waitText("HELLO")
	local := h.syncDepth()

	for applied := 0; applied < 20; applied++ {
		d := h.nextDepth()
		var err error
		if local, err = orderbook.ApplyDelta(local, d, 50); err != nil {
			t.Fatalf("delta %d->%d on local %d: %v", d.BaseSeq, d.Seq, local.Seq, err)
		}
		if canon := mdl.Book(local.Seq); canon != local {
			t.Fatalf("local book diverged from the market function at tick %d", local.Seq)
		}
	}

	// Inject a gap: the server advances its view of this client without sending.
	h.send(map[string]any{"type": "DEBUG", "action": "DROP_DEPTH"})
	gap := false
	for !gap {
		d := h.nextDepth()
		if _, err := orderbook.ApplyDelta(local, d, 50); err == orderbook.ErrSeqMismatch {
			gap = true
		} else if err == nil {
			local, _ = orderbook.ApplyDelta(local, d, 50)
		}
	}

	// Recovery: fresh snapshot + SYNC, then deltas apply cleanly again.
	local = h.syncDepth()
	for applied := 0; applied < 5; applied++ {
		var err error
		if local, err = orderbook.ApplyDelta(local, h.nextDepth(), 50); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
	}
}

func TestTierOverride(t *testing.T) {
	h := setup(t)
	h.waitText("HELLO")
	if m := h.waitText("TIER"); m["tier"] != "DEGRADED" {
		t.Fatalf("initial tier %v, want DEGRADED during warmup", m["tier"])
	}
	h.send(map[string]any{"type": "SET_TIER_OVERRIDE", "tier": "MINIMAL"})
	if m := h.waitText("TIER"); m["tier"] != "MINIMAL" || m["override"] != "MINIMAL" {
		t.Fatalf("override not applied: %v", m)
	}
	h.send(map[string]any{"type": "SET_TIER_OVERRIDE", "tier": "BOGUS"})
	if m := h.waitText("ERROR"); m["message"] == "" {
		t.Fatal("expected error for bogus tier")
	}
	h.send(map[string]any{"type": "SET_TIER_OVERRIDE", "tier": "AUTO"})
	if m := h.waitText("TIER"); m["override"] != "AUTO" || m["tier"] != "DEGRADED" {
		t.Fatalf("AUTO should restore the automatic tier: %v", m)
	}
}

func TestMalformedInputDoesNotKillConnection(t *testing.T) {
	h := setup(t)
	h.waitText("HELLO")
	_ = h.conn.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3})
	h.waitText("ERROR")
	_ = h.conn.WriteMessage(websocket.TextMessage, []byte("{not json"))
	h.waitText("ERROR")
	h.send(map[string]any{"type": "SUBSCRIBE", "symbol": "BTCUSDT", "interval": "7m"})
	h.waitText("ERROR")
	// Still alive: a PING gets its PONG with the probe echoed.
	_ = h.conn.WriteMessage(websocket.BinaryMessage, protocol.EncodeProbe(protocol.TypePing, 9, 12345))
	for {
		b, _ := h.next()
		if b != nil && b[0] == protocol.TypePong {
			hd, _ := protocol.ReadHeader(b)
			if hd.Seq != 9 || hd.TS != 12345 {
				t.Fatalf("pong %+v", hd)
			}
			return
		}
	}
}

func httpHandler(s *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.Handle)
	return mux
}

// TestHubBatchesAndCoalescesByTick pins down the sequence model: one unified
// clock (the market tick). A DEGRADED client flushed every 6 ticks receives
// chart base n -> n+6 and depth base n -> n+6 (depth fires every 3 ticks but
// both are due on tick 6), plus the latest trades, all in ONE frame.
func TestHubBatchesAndCoalescesByTick(t *testing.T) {
	mkt, n0 := seededMarket(t)
	base := generator.LastTradeTick(n0 - 1) // = n0-1 in the normal regime
	c := &Client{id: "t", send: make(chan frame, 8), closed: make(chan struct{}),
		interval: "1m", chartSynced: true, chartSeq: base, depthSynced: true, depthSeq: mkt.DepthSeq()}
	h := newHub(0, config.TierRates{Depth: 150 * time.Millisecond, Chart: 300 * time.Millisecond, Trade: 500 * time.Millisecond}, mkt)
	h.add(c)
	if h.every != [3]uint64{3, 6, 10} {
		t.Fatalf("intervals in ticks = %v, want [3 6 10]", h.every)
	}

	for n := n0; n < n0+6; n++ {
		mkt.Process(mdl.Event(n))
	}
	h.flush(true, true, true)
	f := <-c.send
	parts, err := protocol.SplitFrame(f.data)
	if err != nil || len(parts) != 3 {
		t.Fatalf("want depth+chart+trades in one frame, got %d packets (err %v)", len(parts), err)
	}
	d, _ := protocol.DecodeDepthDelta(parts[0])
	ch, _ := protocol.DecodeChartDelta(parts[1])
	tr, _ := protocol.DecodeTradeUpdate(parts[2])
	if d.BaseSeq != n0-1 || d.Seq != n0+5 {
		t.Fatalf("depth %d->%d, want %d->%d", d.BaseSeq, d.Seq, n0-1, n0+5)
	}
	if ch.BaseSeq != base || ch.Seq != n0+5 {
		t.Fatalf("chart %d->%d, want %d->%d (6 ticks = 6 trades coalesced)", ch.BaseSeq, ch.Seq, base, n0+5)
	}
	if tr[0].ID != generator.TradesBefore(n0+6) {
		t.Fatalf("newest trade id %d, want %d", tr[0].ID, generator.TradesBefore(n0+6))
	}

	// Nothing changed since: nothing is sent (no manufactured updates).
	h.flush(true, true, true)
	select {
	case f := <-c.send:
		t.Fatalf("unexpected frame of %d bytes with no market change", len(f.data))
	default:
	}
}

// TestPongReportsServerHold models a CPU-starved host: the timer for a
// simulated-latency PONG fires 80 ms late. The PONG must carry that 80 ms as
// server hold (so the client can subtract it) and exclude the intended delay.
func TestPongReportsServerHold(t *testing.T) {
	orig := afterFunc
	afterFunc = func(d time.Duration, f func()) *time.Timer { return time.AfterFunc(d+80*time.Millisecond, f) }
	t.Cleanup(func() { afterFunc = orig })

	h := setup(t)
	h.waitText("HELLO")
	h.send(map[string]any{"type": "DEBUG", "action": "SIM_LATENCY", "ms": 100})
	h.waitText("DEBUG_ACK")
	_ = h.conn.WriteMessage(websocket.BinaryMessage, protocol.EncodeProbe(protocol.TypePing, 7, 1))
	for {
		b, _ := h.next()
		if b == nil {
			continue
		}
		parts, _ := protocol.SplitFrame(b)
		for _, p := range parts {
			if p[0] != protocol.TypePong {
				continue
			}
			hold := time.Duration(binary.LittleEndian.Uint32(p[15:])) * time.Microsecond
			if hold < 75*time.Millisecond || hold > 150*time.Millisecond {
				t.Fatalf("hold %v, want ≈80ms (the lateness only, not the 100ms simulated delay)", hold)
			}
			return
		}
	}
}
