// Package client holds the per-connection delivery-tier state machine.
//
// # Signal
//
// The browser probes RTT once per second over the WebSocket and reports each
// sample (plus its RFC 6298 SRTT/RTTVAR, shown in the UI as latency/jitter).
// The server keeps the last W = 5 samples and scores the connection with a
// robust pessimistic latency bound:
//
//	L = median(last 5 RTT) + 4 · MAD(last 5 RTT)
//
// The median ignores up to two outliers in the window, so one or two latency
// spikes cannot move the score; MAD (median absolute deviation) is the robust
// counterpart of RTTVAR and keeps RFC 6298's k = 4.
//
// # Thresholds
//
// A tier is eligible when at most one update is in flight: the pessimistic
// one-way delay L/2 must not exceed the tier's chart interval T, i.e. L ≤ 2T.
// With T = 100 ms (FULL) and 300 ms (DEGRADED):
//
//	FULL      L < 200 ms
//	DEGRADED  L < 600 ms
//	MINIMAL   otherwise
//
// Promotion requires L below 80 % of the threshold (160 / 480 ms): a uniform
// 20 % dead band. Demotion needs 3 consecutive reports past a threshold,
// promotion 5 consecutive reports inside the band; promoting slower than a
// typical flap stops oscillation (simulated 3 s bad / 3 s good: 5 changes with
// ×5, 21 with ×3).
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
	Window = 5 // samples in the robust window

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

// Score computes L = median + 4·MAD over samples (any length >= 1).
func Score(samples []time.Duration) (l, median, mad time.Duration) {
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
	window     []time.Duration
	samples    int
	demote     int
	promote    int
	lastReport time.Time
	score      time.Duration
	median     time.Duration
	mad        time.Duration
}

// NewMachine starts in DEGRADED: until warmup completes the link is unknown,
// and DEGRADED is the middle ground.
func NewMachine(cfg MachineConfig, now time.Time) *Machine {
	return &Machine{cfg: cfg, tier: Degraded, lastReport: now}
}

func (m *Machine) Tier() Tier                         { return m.tier }
func (m *Machine) Score() time.Duration               { return m.score }
func (m *Machine) Stats() (median, mad time.Duration) { return m.median, m.mad }
func (m *Machine) Samples() int                       { return m.samples }
func (m *Machine) LastReport() time.Time              { return m.lastReport }
func (m *Machine) WarmedUp() bool                     { return m.samples >= m.cfg.WarmupSamples }
func (m *Machine) Counters() (demote, promote int)    { return m.demote, m.promote }

// Report feeds one RTT sample. It returns a non-empty reason when the tier changed.
func (m *Machine) Report(rtt time.Duration, now time.Time) string {
	m.lastReport = now
	m.samples++
	m.window = append(m.window, rtt)
	if len(m.window) > Window {
		m.window = m.window[1:]
	}
	l, med, mad := Score(m.window)
	m.score, m.median, m.mad = l, med, mad

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
