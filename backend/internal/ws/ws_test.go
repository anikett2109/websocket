package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"cryptofeed/internal/config"
	"cryptofeed/internal/generator"
	"cryptofeed/internal/history"
	"cryptofeed/internal/market"
	"cryptofeed/internal/model"
	"cryptofeed/internal/orderbook"
	"cryptofeed/internal/protocol"
)

type harness struct {
	t    *testing.T
	mkt  *market.Market
	conn *websocket.Conn
}

func setup(t *testing.T) *harness {
	t.Helper()
	cfg := config.Load()
	fast := config.TierRates{Depth: 10 * time.Millisecond, Chart: 20 * time.Millisecond, Trade: 30 * time.Millisecond}
	cfg.Full, cfg.Degraded, cfg.Minimal = fast, fast, fast
	cfg.Minimal.Depth = 40 * time.Millisecond

	gen := generator.New(generator.Config{Seed: 1, StartPrice: 6_500_000, TickSize: 50, DepthSkipPct: 0})
	mkt := market.New(market.Config{Symbol: "BTCUSDT", StartPrice: 6_500_000, TickSize: 50, HistoryCandles: 50, TradeBuffer: 1000, StateRing: 1024})
	now := time.Now().UnixMilli()
	past := history.Rebase(history.Generate("BTCUSDT", 1, 1, now, 6_500_000, 50).ToCandles(), now)
	if err := mkt.Seed(past, now, gen.Book()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv := NewServer(cfg, mkt)
	go srv.Run(ctx)
	go func() { // drive the market like the real generator, at 5ms
		tk := time.NewTicker(5 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tk.C:
				mkt.Process(gen.Step(now.UnixMilli()))
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

	applied := 0
	for applied < 20 {
		b, _ := h.next()
		if b == nil || b[0] != protocol.TypeDepthDelta {
			continue
		}
		d, err := protocol.DecodeDepthDelta(b)
		if err != nil {
			t.Fatal(err)
		}
		if local, err = orderbook.ApplyDelta(local, d, 50); err != nil {
			t.Fatalf("delta %d->%d on local %d: %v", d.BaseSeq, d.Seq, local.Seq, err)
		}
		if canon, ok := stateAt(h.mkt, local.Seq); ok && canon != local {
			t.Fatalf("local book diverged at seq %d", local.Seq)
		}
		applied++
	}

	// Inject a gap: the server advances its view of this client without sending.
	h.send(map[string]any{"type": "DEBUG", "action": "DROP_DEPTH"})
	gap := false
	for !gap {
		b, _ := h.next()
		if b == nil || b[0] != protocol.TypeDepthDelta {
			continue
		}
		d, _ := protocol.DecodeDepthDelta(b)
		if _, err := orderbook.ApplyDelta(local, d, 50); err == orderbook.ErrSeqMismatch {
			gap = true
		} else if err == nil {
			local, _ = orderbook.ApplyDelta(local, d, 50)
		}
	}

	// Recovery: fresh snapshot + SYNC, then deltas apply cleanly again.
	local = h.syncDepth()
	for applied = 0; applied < 5; {
		b, _ := h.next()
		if b == nil || b[0] != protocol.TypeDepthDelta {
			continue
		}
		d, _ := protocol.DecodeDepthDelta(b)
		var err error
		if local, err = orderbook.ApplyDelta(local, d, 50); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
		applied++
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

func stateAt(m *market.Market, seq uint32) (model.Book, bool) {
	// Compare against the live book only when it has not moved on since.
	cur := m.BookSnapshot().Book
	if cur.Seq != seq {
		return model.Book{}, false // moved on; skip the equality check
	}
	return cur, true
}

func httpHandler(s *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.Handle)
	return mux
}
