package ws

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"cryptofeed/internal/candle"
	"cryptofeed/internal/client"
	"cryptofeed/internal/config"
	"cryptofeed/internal/protocol"
)

// Hub delivers to every client currently in one tier, on that tier's cadence.
// All clients flushed on the same tick converge on the same base sequence, so
// packets are encoded once per (stream, base) and shared.
//
// A hub only reads canonical state from the market; it never computes candles
// or owns the book.
type Hub struct {
	tier   client.Tier
	rates  config.TierRates
	market Market

	mu      sync.Mutex
	clients map[*Client]struct{}
}

func newHub(t client.Tier, r config.TierRates, m Market) *Hub {
	return &Hub{tier: t, rates: r, market: m, clients: map[*Client]struct{}{}}
}

func (h *Hub) add(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *Hub) members() []*Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, c)
	}
	return out
}

func (h *Hub) run(ctx context.Context) {
	depth := time.NewTicker(h.rates.Depth)
	chart := time.NewTicker(h.rates.Chart)
	trade := time.NewTicker(h.rates.Trade)
	defer depth.Stop()
	defer chart.Stop()
	defer trade.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-depth.C:
			h.flushDepth()
		case <-chart.C:
			h.flushChart()
		case <-trade.C:
			h.flushTrades()
		}
	}
}

func (h *Hub) flushDepth() {
	cur := h.market.DepthSeq()
	cache := map[uint32]struct {
		pkt []byte
		seq uint32
	}{}
	now := time.Now().UnixMilli()
	for _, c := range h.members() {
		c.mu.Lock()
		if c.depthSynced && c.depthSeq != cur {
			e, ok := cache[c.depthSeq]
			if !ok {
				d, err := h.market.DepthDelta(c.depthSeq)
				if err != nil {
					c.resyncLocked("depth", err)
					c.mu.Unlock()
					continue
				}
				e.pkt, e.seq = protocol.EncodeDepthDelta(d, now), d.Seq
				cache[c.depthSeq] = e
			}
			switch {
			case c.dropDepth:
				c.dropDepth = false
				c.depthSeq = e.seq // advance without sending: the client will see a gap
				slog.Info("debug: dropped depth packet", "conn", c.id, "seq", e.seq)
			case c.enqueue(frame{data: e.pkt}):
				c.depthSeq = e.seq
				c.stats.depth++
			}
		}
		c.mu.Unlock()
	}
}

func (h *Hub) flushChart() {
	type key struct {
		interval string
		base     uint32
	}
	cache := map[key][]candle.Transition{}
	for _, c := range h.members() {
		c.mu.Lock()
		if c.chartSynced {
			k := key{c.interval, c.chartSeq}
			ts, ok := cache[k]
			if !ok {
				var err error
				ts, err = h.market.ChartTransitions(c.interval, c.chartSeq)
				if err != nil {
					c.resyncLocked("chart", err)
					c.mu.Unlock()
					continue
				}
				cache[k] = ts
			}
			for _, t := range ts {
				if c.dropChart {
					c.dropChart = false
					c.chartSeq = t.Seq
					slog.Info("debug: dropped chart packet", "conn", c.id, "seq", t.Seq)
					continue
				}
				if !c.enqueue(frame{data: protocol.EncodeChartDelta(t)}) {
					break // buffer full: stop here, the rest coalesces into the next flush
				}
				c.chartSeq = t.Seq
				c.stats.chart++
			}
		}
		c.mu.Unlock()
	}
}

func (h *Hub) flushTrades() {
	latest := h.market.LatestTrades(protocol.TradesPerUpdate)
	if len(latest) < protocol.TradesPerUpdate {
		return
	}
	pkt, err := protocol.EncodeTradeUpdate(latest)
	if err != nil {
		slog.Error("encode trades", "err", err)
		return
	}
	newest := latest[0].ID
	for _, c := range h.members() {
		c.mu.Lock()
		if c.lastTradeID < newest && c.enqueue(frame{data: pkt}) {
			c.lastTradeID = newest
			c.stats.trade++
		}
		c.mu.Unlock()
	}
}
