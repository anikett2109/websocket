package ws

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"cryptofeed/internal/candle"
	"cryptofeed/internal/client"
	"cryptofeed/internal/config"
	"cryptofeed/internal/generator"
	"cryptofeed/internal/protocol"
)

// Hub delivers to every client currently in one tier, on that tier's cadence.
//
// A hub is driven by the market clock itself: after the market processes tick
// n it publishes n, and each stream fires when n is a multiple of k
// (k = interval / τ, τ = 50 ms). A DEGRADED chart (k = 6) therefore always
// samples ticks …, n−6, n, n+6, …. Streams that fall due together are
// concatenated into a single binary WebSocket frame (packets are
// self-delimiting via the header length field). All clients flushed on the same
// tick converge on the same base sequence, so packets are encoded once per
// (stream, base) and shared.
//
// A hub only reads canonical state from the market; it never computes candles
// or owns the book.
type Hub struct {
	tier   client.Tier
	rates  config.TierRates
	every  [3]uint64 // depth, chart, trades: fire every k ticks
	market Market

	mu      sync.Mutex
	clients map[*Client]struct{}
}

func ticksOf(d time.Duration) uint64 {
	return max(1, uint64(d/(generator.TickMs*time.Millisecond)))
}

func newHub(t client.Tier, r config.TierRates, m Market) *Hub {
	return &Hub{
		tier: t, rates: r, market: m, clients: map[*Client]struct{}{},
		every: [3]uint64{ticksOf(r.Depth), ticksOf(r.Chart), ticksOf(r.Trade)},
	}
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

func (h *Hub) run(ctx context.Context, ticks <-chan uint32) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticks:
			n := uint64(t)
			h.flush(n%h.every[0] == 0, n%h.every[1] == 0, n%h.every[2] == 0)
		}
	}
}

type depthPkt struct {
	data []byte
	seq  uint32
}

type chartKey struct {
	interval string
	base     uint32
}

// flush builds one frame per client from the streams that are due this tick.
func (h *Hub) flush(depthDue, chartDue, tradeDue bool) {
	if !depthDue && !chartDue && !tradeDue {
		return
	}
	var (
		curDepth   uint32
		depthCache = map[uint32]depthPkt{}
		chartCache = map[chartKey][]candle.Transition{}
		tradePkt   []byte
		newest     uint32
		now        = time.Now().UnixMilli()
	)
	if depthDue {
		curDepth = h.market.DepthSeq()
	}
	if tradeDue {
		if latest := h.market.LatestTrades(protocol.TradesPerUpdate); len(latest) == protocol.TradesPerUpdate {
			var err error
			if tradePkt, err = protocol.EncodeTradeUpdate(latest); err != nil {
				slog.Error("encode trades", "err", err)
			}
			newest = latest[0].ID
		}
	}

	for _, c := range h.members() {
		c.mu.Lock()
		var (
			buf                      []byte
			depthSeq, chartSeq, tid  = c.depthSeq, c.chartSeq, c.lastTradeID
			nDepth, nChart, nTrade   uint64
			droppedDepth, droppedCht bool
		)

		if depthDue && c.depthSynced && c.depthSeq != curDepth {
			p, ok := depthCache[c.depthSeq]
			if !ok {
				d, err := h.market.DepthDelta(c.depthSeq)
				if err != nil {
					c.resyncLocked("depth", err)
				} else {
					p = depthPkt{protocol.EncodeDepthDelta(d, now), d.Seq}
					depthCache[c.depthSeq] = p
					ok = true
				}
			}
			if ok {
				depthSeq = p.seq
				if c.dropDepth {
					droppedDepth = true // advance without sending: the client will see a gap
				} else {
					buf = append(buf, p.data...)
					nDepth++
				}
			}
		}

		if chartDue && c.chartSynced {
			k := chartKey{c.interval, c.chartSeq}
			ts, ok := chartCache[k]
			if !ok {
				var err error
				if ts, err = h.market.ChartTransitions(c.interval, c.chartSeq); err != nil {
					c.resyncLocked("chart", err)
				} else {
					chartCache[k] = ts
				}
			}
			for _, t := range ts {
				chartSeq = t.Seq
				if c.dropChart && !droppedCht {
					droppedCht = true
					continue
				}
				buf = append(buf, protocol.EncodeChartDelta(t)...)
				nChart++
			}
		}

		if tradePkt != nil && c.lastTradeID < newest {
			buf = append(buf, tradePkt...)
			tid = newest
			nTrade++
		}

		// Commit only what was actually handed to the writer. A full buffer
		// skips this flush entirely; the next one sends a larger coalesced delta.
		if len(buf) == 0 || c.enqueue(frame{data: buf}) {
			c.depthSeq, c.chartSeq, c.lastTradeID = depthSeq, chartSeq, tid
			c.stats.depth += nDepth
			c.stats.chart += nChart
			c.stats.trade += nTrade
			if len(buf) > 0 {
				c.stats.frames++
				c.stats.bytes += uint64(len(buf))
			}
			if droppedDepth {
				c.dropDepth = false
				slog.Info("debug: dropped depth packet", "conn", c.id, "seq", depthSeq)
			}
			if droppedCht {
				c.dropChart = false
				slog.Info("debug: dropped chart packet", "conn", c.id)
			}
		}
		c.mu.Unlock()
	}
}
