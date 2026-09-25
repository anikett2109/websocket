// Package ws is the adaptive WebSocket delivery layer.
//
// Server is the client manager: it owns connection lifecycle, each client's
// tier (automatic state machine or debug override) and hub membership. Three
// hubs (FULL, DEGRADED, MINIMAL) deliver depth, chart and trade updates on
// their tier's cadence. Only the Server changes a client's tier.
package ws

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"cryptofeed/internal/candle"
	"cryptofeed/internal/client"
	"cryptofeed/internal/config"
	"cryptofeed/internal/model"
	"cryptofeed/internal/orderbook"
)

// Market is the read-only view of canonical state the delivery layer needs.
type Market interface {
	Symbol() string
	TickSize() int64
	DepthSeq() uint32
	HasDepthState(seq uint32) bool
	DepthDelta(base uint32) (orderbook.Delta, error)
	HasChartState(interval string, seq uint32) bool
	ChartTransitions(interval string, base uint32) ([]candle.Transition, error)
	LatestTrades(n int) []model.Trade
}

type Server struct {
	cfg      config.Config
	market   Market
	hubs     [3]*Hub
	upgrader websocket.Upgrader

	mu      sync.RWMutex
	clients map[string]*Client
}

func NewServer(cfg config.Config, m Market) *Server {
	s := &Server{cfg: cfg, market: m, clients: map[string]*Client{}}
	s.hubs[client.Full] = newHub(client.Full, cfg.Full, m)
	s.hubs[client.Degraded] = newHub(client.Degraded, cfg.Degraded, m)
	s.hubs[client.Minimal] = newHub(client.Minimal, cfg.Minimal, m)
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 4096,
		CheckOrigin:     s.checkOrigin,
	}
	return s
}

func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	for _, o := range s.cfg.AllowedOrigins {
		if o == "*" || o == origin {
			return true
		}
	}
	return origin == ""
}

func (s *Server) Rates(t client.Tier) config.TierRates { return s.hubs[t].rates }

// Run starts the hubs and the missing-report watchdog.
func (s *Server) Run(ctx context.Context) {
	for _, h := range s.hubs {
		go h.run(ctx)
	}
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.mu.RLock()
			for _, c := range s.clients {
				c.close()
			}
			s.mu.RUnlock()
			return
		case now := <-t.C:
			for _, c := range s.snapshotClients() {
				c.mu.Lock()
				reason := c.machine.CheckTimeout(now)
				c.mu.Unlock()
				if reason != "" {
					s.applyTier(c, reason, true)
				}
			}
		}
	}
}

func (s *Server) snapshotClients() []*Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Client, 0, len(s.clients))
	for _, c := range s.clients {
		out = append(out, c)
	}
	return out
}

// Handle upgrades an HTTP request and runs the connection until it closes.
func (s *Server) Handle(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // upgrader already wrote the HTTP error
	}
	now := time.Now()
	c := &Client{
		id:     newID(),
		srv:    s,
		conn:   conn,
		send:   make(chan frame, sendBuffer),
		closed: make(chan struct{}),
		since:  now,
		machine: client.NewMachine(client.MachineConfig{
			WarmupSamples:      s.cfg.WarmupSamples,
			ReportDegradeAfter: s.cfg.ReportDegradeAfter,
			ReportMinimalAfter: s.cfg.ReportMinimalAfter,
		}, now),
		tier: -1,
	}
	c.sendJSON(map[string]any{
		"type": "HELLO", "connId": c.id, "symbol": s.market.Symbol(),
		"priceScale": model.PriceScale, "qtyScale": model.QtyScale, "tickSize": s.market.TickSize(),
		"intervals": []string{"1m", "5m"}, "tiers": s.tierTable(),
	})
	s.applyTier(c, "connected (warming up)", true)
	s.mu.Lock()
	s.clients[c.id] = c
	s.mu.Unlock()
	slog.Info("client connected", "conn", c.id, "remote", r.RemoteAddr)

	go c.writeLoop()
	c.readLoop() // blocks until the connection ends

	c.close()
	c.mu.Lock()
	if c.hub != nil {
		c.hub.remove(c)
		c.hub = nil
	}
	c.mu.Unlock()
	s.mu.Lock()
	delete(s.clients, c.id)
	s.mu.Unlock()
	slog.Info("client disconnected", "conn", c.id, "duration", time.Since(c.since).Round(time.Second))
}

func (s *Server) tierTable() map[string]any {
	out := map[string]any{}
	for i, h := range s.hubs {
		out[client.Tier(i).String()] = rateJSON(h.rates)
	}
	return out
}

func rateJSON(r config.TierRates) map[string]int64 {
	return map[string]int64{"depthMs": r.Depth.Milliseconds(), "chartMs": r.Chart.Milliseconds(), "tradeMs": r.Trade.Milliseconds()}
}

// applyTier recomputes the effective tier (override wins over the automatic
// machine), moves the client between hubs if needed and optionally notifies it.
func (s *Server) applyTier(c *Client, reason string, notify bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	auto := c.machine.Tier()
	eff := auto
	if c.override != nil {
		eff = *c.override
	}
	changed := eff != c.tier
	if changed {
		if c.hub != nil {
			c.hub.remove(c)
		}
		if c.tier >= 0 {
			slog.Info("client tier changed", "conn", c.id, "from", c.tier, "to", eff, "reason", reason,
				"srtt", c.srtt, "rttvar", c.rttvar, "effective", c.machine.Score())
		}
		c.tier = eff
		c.hub = s.hubs[eff]
		c.hub.add(c)
	}
	if !notify {
		return
	}
	override := "AUTO"
	if c.override != nil {
		override = c.override.String()
	}
	msg := map[string]any{
		"type": "TIER", "tier": eff.String(), "autoTier": auto.String(), "override": override,
		"changed": changed, "reason": reason,
		"effectiveLatencyMs": float64(c.machine.Score().Microseconds()) / 1000,
		"srttMs":             float64(c.srtt.Microseconds()) / 1000,
		"rttvarMs":           float64(c.rttvar.Microseconds()) / 1000,
		"samples":            c.machine.Samples(), "warmedUp": c.machine.WarmedUp(),
		"rates": rateJSON(s.hubs[eff].rates),
	}
	c.sendJSON(msg)
}

var ErrBadTier = errors.New("tier must be AUTO, FULL, DEGRADED or MINIMAL")

// setOverride is the debug control. AUTO returns the client to the automatic
// state machine, which keeps running while an override is active.
func (s *Server) setOverride(c *Client, tier string) error {
	c.mu.Lock()
	if tier == "AUTO" || tier == "auto" {
		c.override = nil
	} else {
		t, ok := client.ParseTier(tier)
		if !ok {
			c.mu.Unlock()
			return ErrBadTier
		}
		c.override = &t
	}
	c.mu.Unlock()
	slog.Info("tier override set", "conn", c.id, "override", tier)
	s.applyTier(c, "debug override "+tier, true)
	return nil
}

var ErrNoClient = errors.New("no such connection")

// SetOverrideByID lets the REST debug endpoint drive the same control.
func (s *Server) SetOverrideByID(id, tier string) error {
	s.mu.RLock()
	c := s.clients[id]
	s.mu.RUnlock()
	if c == nil {
		return ErrNoClient
	}
	return s.setOverride(c, tier)
}

type ClientStatus struct {
	ID                 string            `json:"id"`
	ConnectedAt        time.Time         `json:"connectedAt"`
	Tier               string            `json:"tier"`
	AutoTier           string            `json:"autoTier"`
	Override           string            `json:"override"`
	SRTTMs             float64           `json:"srttMs"`
	RTTVarMs           float64           `json:"rttvarMs"`
	EffectiveLatencyMs float64           `json:"effectiveLatencyMs"`
	Samples            int               `json:"samples"`
	LastReportAgoMs    int64             `json:"lastReportAgoMs"`
	Interval           string            `json:"interval"`
	ChartSeq           uint32            `json:"chartSeq"`
	DepthSeq           uint32            `json:"depthSeq"`
	Rates              map[string]int64  `json:"rates"`
	Sent               map[string]uint64 `json:"sent"`
}

func (s *Server) Status() []ClientStatus {
	out := []ClientStatus{}
	for _, c := range s.snapshotClients() {
		c.mu.Lock()
		ov := "AUTO"
		if c.override != nil {
			ov = c.override.String()
		}
		st := ClientStatus{
			ID: c.id, ConnectedAt: c.since, Tier: c.tier.String(), AutoTier: c.machine.Tier().String(), Override: ov,
			SRTTMs:             float64(c.srtt.Microseconds()) / 1000,
			RTTVarMs:           float64(c.rttvar.Microseconds()) / 1000,
			EffectiveLatencyMs: float64(c.machine.Score().Microseconds()) / 1000,
			Samples:            c.machine.Samples(),
			LastReportAgoMs:    time.Since(c.machine.LastReport()).Milliseconds(),
			Interval:           c.interval, ChartSeq: c.chartSeq, DepthSeq: c.depthSeq,
			Rates: rateJSON(s.hubs[c.tier].rates),
			Sent:  map[string]uint64{"depth": c.stats.depth, "chart": c.stats.chart, "trade": c.stats.trade},
		}
		c.mu.Unlock()
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectedAt.Before(out[j].ConnectedAt) })
	return out
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
