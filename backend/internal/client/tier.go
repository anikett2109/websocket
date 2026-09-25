// Package client holds the per-connection delivery-tier state machine.
//
// The browser measures RTT and reports SRTT and RTTVAR (RFC 6298 style,
// alpha=1/8, beta=1/4). The server combines them into one network-quality score
//
//	EffectiveLatency = SRTT + 4*RTTVAR
//
// and classifies it with hysteresis. The 100/250 ms boundaries and the sample
// counts are application choices, not part of RFC 6298.
package client

import (
	"fmt"
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

// Thresholds on EffectiveLatency. Demotion and promotion boundaries differ
// (100 vs 80, 250 vs 200) so a score hovering at a boundary cannot flap.
const (
	DemoteToDegraded  = 100 * time.Millisecond // FULL -> DEGRADED when E >= 100ms
	DemoteToMinimal   = 250 * time.Millisecond // DEGRADED -> MINIMAL when E >= 250ms
	PromoteToFull     = 80 * time.Millisecond  // DEGRADED -> FULL when E < 80ms
	PromoteToDegraded = 200 * time.Millisecond // MINIMAL -> DEGRADED when E < 200ms
	DemoteSamples     = 3
	PromoteSamples    = 5
)

// Classify maps a score to a tier without hysteresis (used once, after warmup).
func Classify(e time.Duration) Tier {
	switch {
	case e < DemoteToDegraded:
		return Full
	case e < DemoteToMinimal:
		return Degraded
	default:
		return Minimal
	}
}

type MachineConfig struct {
	WarmupSamples      int
	ReportDegradeAfter time.Duration // no report for this long => at most DEGRADED
	ReportMinimalAfter time.Duration // no report for this long => MINIMAL
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
	lastScore  time.Duration
}

// NewMachine starts in DEGRADED: until warmup completes we do not know the
// link is good, and DEGRADED is a safe middle ground.
func NewMachine(cfg MachineConfig, now time.Time) *Machine {
	return &Machine{cfg: cfg, tier: Degraded, lastReport: now}
}

func (m *Machine) Tier() Tier                      { return m.tier }
func (m *Machine) Score() time.Duration            { return m.lastScore }
func (m *Machine) Samples() int                    { return m.samples }
func (m *Machine) LastReport() time.Time           { return m.lastReport }
func (m *Machine) WarmedUp() bool                  { return m.samples >= m.cfg.WarmupSamples }
func (m *Machine) Counters() (demote, promote int) { return m.demote, m.promote }

// Report feeds one valid latency report. It returns a non-empty reason when the tier changed.
func (m *Machine) Report(srtt, rttvar time.Duration, now time.Time) string {
	e := srtt + 4*rttvar
	m.lastReport = now
	m.lastScore = e
	m.samples++

	if m.samples < m.cfg.WarmupSamples {
		return ""
	}
	if m.samples == m.cfg.WarmupSamples {
		return m.set(Classify(e), fmt.Sprintf("warmup complete, E=%v", e.Round(time.Millisecond)))
	}

	switch m.tier {
	case Full:
		m.promote = 0
		if e >= DemoteToDegraded {
			m.demote++
		} else {
			m.demote = 0
		}
		if m.demote >= DemoteSamples {
			return m.set(Degraded, fmt.Sprintf("E>=%v for %d reports", DemoteToDegraded, DemoteSamples))
		}
	case Degraded:
		switch {
		case e >= DemoteToMinimal:
			m.demote++
			m.promote = 0
		case e < PromoteToFull:
			m.promote++
			m.demote = 0
		default:
			m.demote, m.promote = 0, 0
		}
		if m.demote >= DemoteSamples {
			return m.set(Minimal, fmt.Sprintf("E>=%v for %d reports", DemoteToMinimal, DemoteSamples))
		}
		if m.promote >= PromoteSamples {
			return m.set(Full, fmt.Sprintf("E<%v for %d reports", PromoteToFull, PromoteSamples))
		}
	case Minimal:
		m.demote = 0
		if e < PromoteToDegraded {
			m.promote++
		} else {
			m.promote = 0
		}
		if m.promote >= PromoteSamples {
			return m.set(Degraded, fmt.Sprintf("E<%v for %d reports", PromoteToDegraded, PromoteSamples))
		}
	}
	return ""
}

// CheckTimeout applies the missing-report policy. Silence is treated as bad
// network: after ReportDegradeAfter the client is capped at DEGRADED, after
// ReportMinimalAfter it drops to MINIMAL. Normal reports promote it back with hysteresis.
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
