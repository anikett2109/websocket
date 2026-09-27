package ws

import (
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"cryptofeed/internal/client"
	"cryptofeed/internal/market"
	"cryptofeed/internal/protocol"
)

const (
	sendBuffer   = 256
	writeTimeout = 5 * time.Second
	readTimeout  = 45 * time.Second // refreshed by any client message or control pong
	ctrlPing     = 15 * time.Second // websocket control ping, detects dead TCP
	maxMessage   = 4096
)

type frame struct {
	text bool
	data []byte
}

type counters struct{ depth, chart, trade, frames, bytes uint64 }

// Client is one WebSocket connection. mu guards all mutable fields; hubs and
// the reader goroutine both take it. Only the writer goroutine touches conn writes.
type Client struct {
	id     string
	srv    *Server
	conn   *websocket.Conn
	send   chan frame
	closed chan struct{}
	once   sync.Once
	since  time.Time

	mu          sync.Mutex
	machine     *client.Machine
	override    *client.Tier
	tier        client.Tier // effective tier (hub membership)
	hub         *Hub
	interval    string
	chartSynced bool
	chartSeq    uint32
	depthSynced bool
	depthSeq    uint32
	lastTradeID uint32
	simLatency  time.Duration
	spikeNext   bool      // debug: delay the next PONG by SpikeDelay
	tierSentAt  time.Time // last TIER message (keepalive every tierKeepalive)
	tierReason  string    // reason for the last tier change, repeated in keepalives
	dropDepth   bool
	dropChart   bool
	stats       counters
}

// enqueue never blocks: a full buffer means this flush is skipped and the next
// flush sends a larger coalesced delta instead (natural backpressure).
func (c *Client) enqueue(f frame) bool {
	select {
	case <-c.closed:
		return false
	default:
	}
	select {
	case c.send <- f:
		return true
	default:
		return false
	}
}

func (c *Client) sendJSON(v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return c.enqueue(frame{text: true, data: b})
}

func (c *Client) close() {
	c.once.Do(func() { close(c.closed) })
}

// resyncLocked tells the client its base is no longer retained; it must re-snapshot.
func (c *Client) resyncLocked(stream string, err error) {
	if stream == "depth" {
		c.depthSynced = false
	} else {
		c.chartSynced = false
	}
	slog.Warn("sequence base unavailable, forcing resync", "conn", c.id, "stream", stream, "err", err)
	c.sendJSON(map[string]any{"type": "RESYNC", "stream": stream})
}

func (c *Client) writeLoop() {
	ping := time.NewTicker(ctrlPing)
	defer func() {
		ping.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case <-c.closed:
			_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			return
		case f := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			typ := websocket.BinaryMessage
			if f.text {
				typ = websocket.TextMessage
			}
			if err := c.conn.WriteMessage(typ, f.data); err != nil {
				c.close()
				return
			}
		case <-ping.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *Client) readLoop() {
	defer c.close()
	c.conn.SetReadLimit(maxMessage)
	_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	})
	for {
		typ, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
		if typ == websocket.BinaryMessage {
			c.handleBinary(data)
		} else {
			c.handleText(data)
		}
	}
}

// handleBinary answers application PINGs. The PONG echoes the client's probe
// seq and timestamp so the client computes RTT on its own clock.
func (c *Client) handleBinary(b []byte) {
	h, err := protocol.ReadHeader(b)
	if err != nil || h.Type != protocol.TypePing {
		c.sendError("malformed or unexpected binary packet")
		return
	}
	received := time.Now()
	c.mu.Lock()
	delay := c.simLatency
	if c.spikeNext {
		delay += SpikeDelay
		c.spikeNext = false
	}
	c.mu.Unlock()
	// hold = time spent in this process beyond the intended delay. On a
	// 0.1-CPU host the process is paused when its CPU quota runs out, so
	// timers fire late; that lateness is the server's, not the client's network.
	send := func() {
		hold := max(0, time.Since(received)-delay)
		c.enqueue(frame{data: protocol.EncodePong(h.Seq, h.TS, uint32(min(hold.Microseconds(), 1<<32-1)))})
	}
	if delay > 0 {
		afterFunc(delay, send)
		return
	}
	send()
}

type controlMsg struct {
	Type      string  `json:"type"`
	Symbol    string  `json:"symbol"`
	Interval  string  `json:"interval"`
	Stream    string  `json:"stream"`
	Seq       *uint32 `json:"seq"`
	Tier      string  `json:"tier"`
	Action    string  `json:"action"`
	Ms        float64 `json:"ms"`
	RTTMs     float64 `json:"rttMs"`
	LatencyMs float64 `json:"latencyMs"`
	JitterMs  float64 `json:"jitterMs"`
}

func (c *Client) sendError(msg string) {
	c.sendJSON(map[string]any{"type": "ERROR", "message": msg})
}

func (c *Client) handleText(b []byte) {
	var m controlMsg
	if err := json.Unmarshal(b, &m); err != nil {
		c.sendError("malformed JSON control message")
		return
	}
	switch strings.ToUpper(m.Type) {
	case "SUBSCRIBE":
		c.subscribe(m)
	case "SYNC":
		c.sync(m)
	case "NET_REPORT":
		c.netReport(m)
	case "SET_TIER_OVERRIDE":
		if err := c.srv.setOverride(c, m.Tier); err != nil {
			c.sendError(err.Error())
		}
	case "DEBUG":
		c.debug(m)
	default:
		c.sendError("unknown message type " + m.Type)
	}
}

func (c *Client) subscribe(m controlMsg) {
	if m.Symbol != "" && m.Symbol != c.srv.market.Symbol() {
		c.sendError("unknown symbol " + m.Symbol)
		return
	}
	if _, ok := market.Intervals[m.Interval]; !ok {
		c.sendError("invalid interval " + m.Interval)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Changing (or re-sending) the subscription drops the chart baseline: no
	// chart packets flow until the client SYNCs from a REST snapshot of this interval.
	c.interval = m.Interval
	c.chartSynced = false
	c.sendJSON(map[string]any{"type": "SUBSCRIBED", "symbol": c.srv.market.Symbol(), "interval": m.Interval})
}

// sync sets the client's delivery baseline to a canonical state it obtained
// via REST. The SYNCED ack is enqueued under c.mu, so every delta after it on
// this ordered connection is relative to that seq.
func (c *Client) sync(m controlMsg) {
	if m.Seq == nil {
		c.sendError("SYNC requires seq")
		return
	}
	seq := *m.Seq
	c.mu.Lock()
	defer c.mu.Unlock()
	fail := func(reason string) {
		slog.Info("sync failed", "conn", c.id, "stream", m.Stream, "seq", seq, "reason", reason)
		c.sendJSON(map[string]any{"type": "SYNC_FAILED", "stream": m.Stream, "seq": seq, "reason": reason})
	}
	switch m.Stream {
	case "depth":
		if !c.srv.market.HasDepthState(seq) {
			fail("depth seq not retained")
			return
		}
		c.depthSeq, c.depthSynced = seq, true
		c.sendJSON(map[string]any{"type": "SYNCED", "stream": "depth", "seq": seq})
	case "chart":
		if c.interval == "" || m.Interval != c.interval {
			fail("interval does not match subscription")
			return
		}
		if !c.srv.market.HasChartState(c.interval, seq) {
			fail("chart seq not retained")
			return
		}
		c.chartSeq, c.chartSynced = seq, true
		c.sendJSON(map[string]any{"type": "SYNCED", "stream": "chart", "interval": c.interval, "seq": seq})
	default:
		c.sendError("unknown stream " + m.Stream)
	}
}

func validMs(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < 60_000 }

// afterFunc schedules delayed PONGs (replaceable in tests to model a late timer).
var afterFunc = time.AfterFunc

// SpikeDelay is the debug "Spike" action: one PONG delayed by 900 ms, a single
// outlier the robust median must ignore (the tier should not change).
const SpikeDelay = 900 * time.Millisecond

// netReport is the health report: the browser's latency (median of its last
// 5 RTTs) and jitter (their MAD). The server scores L = latency + 4*jitter.
func (c *Client) netReport(m controlMsg) {
	if !validMs(m.LatencyMs) || !validMs(m.JitterMs) || !validMs(m.RTTMs) {
		c.sendError("invalid NET_REPORT: latencyMs, jitterMs and rttMs must be 0..60000")
		return
	}
	ms := func(v float64) time.Duration { return time.Duration(v * float64(time.Millisecond)) }
	latency, jitter := ms(m.LatencyMs), ms(m.JitterMs)
	c.mu.Lock()
	reason := c.machine.Report(latency, jitter, time.Now())
	score := c.machine.Score()
	c.mu.Unlock()
	if c.srv.cfg.Debug {
		slog.Debug("net report", "conn", c.id, "latency", latency, "jitter", jitter, "score", score)
	}
	c.srv.applyTier(c, reason, false)
}

func (c *Client) debug(m controlMsg) {
	c.mu.Lock()
	switch strings.ToUpper(m.Action) {
	case "DROP_DEPTH":
		c.dropDepth = true
	case "DROP_CHART":
		c.dropChart = true
	case "SIM_LATENCY":
		if !validMs(m.Ms) || m.Ms > 5000 {
			c.mu.Unlock()
			c.sendError("SIM_LATENCY ms must be 0..5000")
			return
		}
		c.simLatency = time.Duration(m.Ms) * time.Millisecond
	case "SPIKE":
		c.spikeNext = true
	default:
		c.mu.Unlock()
		c.sendError("unknown debug action " + m.Action)
		return
	}
	c.mu.Unlock()
	slog.Info("debug control", "conn", c.id, "action", m.Action, "ms", m.Ms)
	c.sendJSON(map[string]any{"type": "DEBUG_ACK", "action": strings.ToUpper(m.Action), "ms": m.Ms})
}
