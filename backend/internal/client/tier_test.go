package client

import (
	"testing"
	"time"
)

var cfg = MachineConfig{WarmupSamples: 5, ReportDegradeAfter: 3 * time.Second, ReportMinimalAfter: 6 * time.Second}

const ms = time.Millisecond

type step struct {
	srtt, rttvar time.Duration
	want         Tier
	note         string
}

func run(t *testing.T, m *Machine, now *time.Time, steps []step) {
	t.Helper()
	for i, s := range steps {
		*now = now.Add(time.Second)
		m.Report(s.srtt, s.rttvar, *now)
		if got := m.Tier(); got != s.want {
			t.Fatalf("step %d (%s): E=%v tier=%v want %v", i, s.note, s.srtt+4*s.rttvar, got, s.want)
		}
	}
}

func TestHysteresis(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMachine(cfg, now)
	good := func(n string) step { return step{20 * ms, 5 * ms, Full, n} }             // E=40ms
	bad := func(want Tier, n string) step { return step{200 * ms, 50 * ms, want, n} } // E=400ms

	steps := []step{
		{20 * ms, 5 * ms, Degraded, "warmup 1: starts DEGRADED"},
		{20 * ms, 5 * ms, Degraded, "warmup 2"},
		{20 * ms, 5 * ms, Degraded, "warmup 3"},
		{20 * ms, 5 * ms, Degraded, "warmup 4"},
		good("warmup 5 classifies directly -> FULL"),
		bad(Full, "one bad sample stays FULL"),
		good("good sample resets demotion counter"),
		bad(Full, "bad 1"),
		bad(Full, "bad 2"),
		bad(Degraded, "bad 3 -> DEGRADED"),
		bad(Degraded, "bad 1 in DEGRADED"),
		bad(Degraded, "bad 2 in DEGRADED"),
		bad(Minimal, "bad 3 -> MINIMAL"),
		{20 * ms, 5 * ms, Minimal, "one good sample stays MINIMAL"},
		{20 * ms, 5 * ms, Minimal, "good 2"},
		{20 * ms, 5 * ms, Minimal, "good 3"},
		{20 * ms, 5 * ms, Minimal, "good 4"},
		{20 * ms, 5 * ms, Degraded, "good 5 -> DEGRADED"},
		{20 * ms, 5 * ms, Degraded, "good 1 in DEGRADED"},
		{20 * ms, 5 * ms, Degraded, "good 2"},
		{20 * ms, 5 * ms, Degraded, "good 3"},
		{20 * ms, 5 * ms, Degraded, "good 4"},
		{20 * ms, 5 * ms, Full, "good 5 -> FULL"},
	}
	run(t, m, &now, steps)
}

func TestHysteresisBandDoesNotFlap(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMachine(cfg, now)
	var steps []step
	for i := 0; i < 5; i++ {
		steps = append(steps, step{60 * ms, 10 * ms, Degraded, "warmup at E=100ms (boundary) -> DEGRADED"})
	}
	// E alternates 90ms / 110ms: between the promote (80) and demote (250)
	// boundaries of DEGRADED, so it must never move.
	for i := 0; i < 20; i++ {
		e := 90 * ms
		if i%2 == 1 {
			e = 110 * ms
		}
		steps = append(steps, step{e, 0, Degraded, "oscillating inside band"})
	}
	run(t, m, &now, steps)
}

func TestMissingReports(t *testing.T) {
	now := time.Unix(0, 0)
	m := NewMachine(cfg, now)
	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		m.Report(10*ms, 2*ms, now)
	}
	if m.Tier() != Full {
		t.Fatalf("want FULL after warmup, got %v", m.Tier())
	}
	if r := m.CheckTimeout(now.Add(2 * time.Second)); r != "" || m.Tier() != Full {
		t.Fatalf("2s of silence should not demote")
	}
	if r := m.CheckTimeout(now.Add(3 * time.Second)); r == "" || m.Tier() != Degraded {
		t.Fatalf("3s of silence should demote to DEGRADED, got %v", m.Tier())
	}
	if r := m.CheckTimeout(now.Add(6 * time.Second)); r == "" || m.Tier() != Minimal {
		t.Fatalf("6s of silence should demote to MINIMAL, got %v", m.Tier())
	}
	// Reports resume: recovery still goes through hysteresis one tier at a time.
	now = now.Add(7 * time.Second)
	for i := 0; i < 4; i++ {
		now = now.Add(time.Second)
		m.Report(10*ms, 2*ms, now)
	}
	if m.Tier() != Minimal {
		t.Fatalf("4 good reports must not promote yet")
	}
	now = now.Add(time.Second)
	m.Report(10*ms, 2*ms, now)
	if m.Tier() != Degraded {
		t.Fatalf("5th good report should promote to DEGRADED, got %v", m.Tier())
	}
}

func TestClassify(t *testing.T) {
	cases := map[time.Duration]Tier{0: Full, 99 * ms: Full, 100 * ms: Degraded, 249 * ms: Degraded, 250 * ms: Minimal}
	for e, want := range cases {
		if got := Classify(e); got != want {
			t.Errorf("Classify(%v)=%v want %v", e, got, want)
		}
	}
}
