// Package client holds the per-connection delivery-tier state machine.
//
// # Health report (browser)
//
// The app probes RTT over the WebSocket (PING/PONG, 1/s after 3 fast-start
// probes) and keeps the last W = 5 RTTs. After every probe it reports
//
//	latency = median(window)                    typical round trip
//	jitter  = median(|RTTi - latency|)  (MAD)   typical deviation
//
// Both are robust: up to 2 of 5 samples can be outliers without moving them.
//
// # Decision (server)
//
// The server owns the tier and scores the reported values as a pessimistic
// round-trip bound (k = 4, as in RFC 6298):
//
//	L = latency + 4 * jitter
//
// A tier is eligible when at most one update is in flight: the one-way delay
// L/2 must fit in the tier's chart interval T, i.e. L <= 2T. With T = 100 ms
// (FULL) and 300 ms (DEGRADED):
//
//	FULL      L < 200 ms
//	DEGRADED  L < 600 ms
//	MINIMAL   otherwise
//
// Promotion requires L below 80 % of the threshold (160 / 480 ms): a uniform
// 20 % dead band. Demotion needs 3 consecutive reports past a threshold,
// promotion 5 consecutive reports inside the band (simulated 3 s bad / 3 s
// good: 5 changes with x5, 21 with x3).
package client

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

type Tier int

const (
	Full Tier = iota
	Degraded
	Minimal
)

var tierNames = [...]string{"FULL", "DEGRADED", "MINIMAL"}

func (t Tier) String() string { return tierNames[t] }

func ParseTier(s string) (Tier, bool) {
	for i, n := range tierNames {
		if strings.EqualFold(s, n) {
			return Tier(i), true
		}
	}
	return 0, false
}

const (
	Window = 5 // probes in the client's robust window

	ChartFull     = 100 * time.Millisecond // FULL chart interval (T)
	ChartDegraded = 300 * time.Millisecond // DEGRADED chart interval

	DemoteToDegraded  = 2 * ChartFull            // 200 ms
	DemoteToMinimal   = 2 * ChartDegraded        // 600 ms
	PromoteToFull     = DemoteToDegraded * 4 / 5 // 160 ms
	PromoteToDegraded = DemoteToMinimal * 4 / 5  // 480 ms

	DemoteSamples  = 3
	PromoteSamples = 5
)

// Classify maps a score to a tier without hysteresis (used once, after warmup).
func Classify(l time.Duration) Tier {
	switch {
	case l < DemoteToDegraded:
		return Full
	case l < DemoteToMinimal:
		return Degraded
	default:
		return Minimal
	}
}

// Summarize is the client's health-report math (mirrors the browser's
// HealthMeter): latency = median, jitter = MAD, score L = latency + 4*jitter.
func Summarize(samples []time.Duration) (l, median, mad time.Duration) {
	median = medianOf(samples)
	dev := make([]time.Duration, len(samples))
	for i, s := range samples {
		dev[i] = (s - median).Abs()
	}
	mad = medianOf(dev)
	return median + 4*mad, median, mad
}

func medianOf(xs []time.Duration) time.Duration {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

type MachineConfig struct {
	WarmupSamples      int           // samples before the first classification (3)
	ReportDegradeAfter time.Duration // no report for this long => at most DEGRADED (3 s = 3 missed probes)
	ReportMinimalAfter time.Duration // no report for this long => MINIMAL (6 s)
}

// Machine is the automatic tier decision for one connection. It is not safe
// for concurrent use; the owning connection serialises access.
type Machine struct {
	cfg        MachineConfig
	tier       Tier
	samples    int
	demote     int
	promote    int
	lastReport time.Time
	score      time.Duration
	latency    time.Duration
	jitter     time.Duration
}

// NewMachine starts in DEGRADED: until warmup completes the link is unknown,
// and DEGRADED is the middle ground.
func NewMachine(cfg MachineConfig, now time.Time) *Machine {
	return &Machine{cfg: cfg, tier: Degraded, lastReport: now}
}

func (m *Machine) Tier() Tier                             { return m.tier }
func (m *Machine) Score() time.Duration                   { return m.score }
func (m *Machine) Stats() (latency, jitter time.Duration) { return m.latency, m.jitter }
func (m *Machine) Samples() int                           { return m.samples }
func (m *Machine) LastReport() time.Time                  { return m.lastReport }
func (m *Machine) WarmedUp() bool                         { return m.samples >= m.cfg.WarmupSamples }
func (m *Machine) Counters() (demote, promote int)        { return m.demote, m.promote }

// Report feeds one health report (the client's latency and jitter). It
// returns a non-empty reason when the tier changed.
func (m *Machine) Report(latency, jitter time.Duration, now time.Time) string {
	m.lastReport = now
	m.samples++
	l := latency + 4*jitter
	m.score, m.latency, m.jitter = l, latency, jitter

	if m.samples < m.cfg.WarmupSamples {
		return ""
	}
	if m.samples == m.cfg.WarmupSamples {
		return m.set(Classify(l), fmt.Sprintf("warmup complete, L=%v", l.Round(time.Millisecond)))
	}

	switch m.tier {
	case Full:
		m.promote = 0
		if l >= DemoteToDegraded {
			m.demote++
		} else {
			m.demote = 0
		}
		if m.demote >= DemoteSamples {
			return m.set(Degraded, fmt.Sprintf("L>=%v for %d reports", DemoteToDegraded, DemoteSamples))
		}
	case Degraded:
		switch {
		case l >= DemoteToMinimal:
			m.demote++
			m.promote = 0
		case l < PromoteToFull:
			m.promote++
			m.demote = 0
		default:
			m.demote, m.promote = 0, 0
		}
		if m.demote >= DemoteSamples {
			return m.set(Minimal, fmt.Sprintf("L>=%v for %d reports", DemoteToMinimal, DemoteSamples))
		}
		if m.promote >= PromoteSamples {
			return m.set(Full, fmt.Sprintf("L<%v for %d reports", PromoteToFull, PromoteSamples))
		}
	case Minimal:
		m.demote = 0
		if l < PromoteToDegraded {
			m.promote++
		} else {
			m.promote = 0
		}
		if m.promote >= PromoteSamples {
			return m.set(Degraded, fmt.Sprintf("L<%v for %d reports", PromoteToDegraded, PromoteSamples))
		}
	}
	return ""
}

// CheckTimeout applies the missing-report policy. Silence is treated as bad
// network: after ReportDegradeAfter (3 missed probes, the demotion count) the
// client is capped at DEGRADED, after ReportMinimalAfter it drops to MINIMAL.
// Normal reports promote it back with hysteresis.
func (m *Machine) CheckTimeout(now time.Time) string {
	silent := now.Sub(m.lastReport)
	switch {
	case silent >= m.cfg.ReportMinimalAfter && m.tier != Minimal:
		return m.set(Minimal, fmt.Sprintf("no latency report for %v", silent.Round(time.Second)))
	case silent >= m.cfg.ReportDegradeAfter && m.tier == Full:
		return m.set(Degraded, fmt.Sprintf("no latency report for %v", silent.Round(time.Second)))
	}
	return ""
}

func (m *Machine) set(t Tier, reason string) string {
	m.demote, m.promote = 0, 0
	if t == m.tier {
		return ""
	}
	m.tier = t
	return reason
}
