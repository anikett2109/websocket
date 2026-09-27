package client

import (
	"testing"
	"time"
)

var cfg = MachineConfig{WarmupSamples: 3, ReportDegradeAfter: 3 * time.Second, ReportMinimalAfter: 6 * time.Second}

const ms = time.Millisecond

// healthy is the measured India -> Singapore profile (RTT 77..125 ms).
var healthy = []time.Duration{82 * ms, 89 * ms, 95 * ms, 86 * ms, 100 * ms, 91 * ms, 125 * ms, 84 * ms, 93 * ms, 88 * ms}

func healthyAt(i int) time.Duration { return healthy[i%len(healthy)] }

type driver struct {
	t       *testing.T
	m       *Machine
	now     time.Time
	i       int
	changes []string
}

func newDriver(t *testing.T) *driver {
	now := time.Unix(0, 0)
	return &driver{t: t, m: NewMachine(cfg, now), now: now}
}

// feed reports one sample per second and records tier changes.
func (d *driver) feed(rtts ...time.Duration) {
	for _, r := range rtts {
		d.now = d.now.Add(time.Second)
		if reason := d.m.Report(r, d.now); reason != "" {
			d.changes = append(d.changes, d.m.Tier().String())
		}
		d.i++
	}
}

func (d *driver) healthy(n int) {
	for k := 0; k < n; k++ {
		d.feed(healthyAt(d.i))
	}
}

func (d *driver) want(tier Tier, note string) {
	d.t.Helper()
	if d.m.Tier() != tier {
		d.t.Fatalf("%s: tier=%v want %v (L=%v, changes %v)", note, d.m.Tier(), tier, d.m.Score(), d.changes)
	}
}

func TestScore(t *testing.T) {
	l, med, mad := Score([]time.Duration{80 * ms, 90 * ms, 100 * ms, 900 * ms, 85 * ms})
	if med != 90*ms || mad != 10*ms || l != 130*ms {
		t.Fatalf("L=%v median=%v mad=%v", l, med, mad)
	}
}

func TestHealthyLinkReachesFullAndStays(t *testing.T) {
	d := newDriver(t)
	d.feed(healthyAt(0), healthyAt(1))
	d.want(Degraded, "during warmup")
	d.feed(healthyAt(2))
	d.want(Full, "warmup (3 probes) classifies directly")
	d.healthy(120)
	d.want(Full, "two minutes of the measured profile")
	if len(d.changes) != 1 {
		t.Fatalf("changes %v", d.changes)
	}
}

func TestSpikesDoNotSwitchTier(t *testing.T) {
	d := newDriver(t)
	d.healthy(20)
	d.feed(900 * ms)
	d.healthy(10)
	d.feed(900*ms, healthyAt(0), 900*ms) // two spikes 2 s apart
	d.healthy(20)
	d.want(Full, "spikes")
	if len(d.changes) != 1 {
		t.Fatalf("spikes caused tier changes: %v", d.changes)
	}
}

func TestCongestionDemotesAndRecovers(t *testing.T) {
	d := newDriver(t)
	d.healthy(20)
	congested := func(k int) time.Duration { return time.Duration(500+(k*37)%200) * ms } // +400..600 ms queue
	for k := 0; k < 4; k++ {
		d.feed(congested(k))
	}
	d.want(Full, "3 bad samples only move the median; not yet 3 bad scores")
	d.feed(congested(4))
	d.want(Degraded, "5th bad sample: 3 consecutive scores >= 200")
	for k := 5; k < 10; k++ {
		d.feed(congested(k))
	}
	d.want(Minimal, "within 10 bad samples: 3 consecutive scores >= 600")
	if d.i != 30 {
		t.Fatal("bookkeeping")
	}
	for k := 10; k < 40; k++ {
		d.feed(congested(k))
	}
	d.healthy(6)
	d.want(Minimal, "not before the median recovers and 5 good scores")
	d.healthy(1)
	d.want(Degraded, "7 s after recovery")
	d.healthy(5)
	d.want(Full, "12 s after recovery")
}

// Moderate congestion (+250..450 ms) lands in DEGRADED, not MINIMAL: L ≈ 430 + 4·MAD < 600 most of the time.
func TestModerateCongestionIsDegraded(t *testing.T) {
	d := newDriver(t)
	d.healthy(20)
	for k := 0; k < 30; k++ {
		d.feed(time.Duration(335+(k*37)%200) * ms)
	}
	if d.m.Tier() == Full {
		t.Fatalf("moderate congestion must leave FULL, changes %v", d.changes)
	}
}

func TestFlappingIsDamped(t *testing.T) {
	d := newDriver(t)
	d.healthy(20)
	for cycle := 0; cycle < 10; cycle++ {
		d.feed(420*ms, 450*ms, 480*ms)
		d.healthy(3)
	}
	d.healthy(30)
	if len(d.changes) > 5 {
		t.Fatalf("flapping network caused %d changes: %v", len(d.changes), d.changes)
	}
	d.want(Full, "settles back once stable")
}

func TestDeadBandHoldsTier(t *testing.T) {
	d := newDriver(t)
	d.feed(145*ms, 150*ms, 155*ms) // median 150 + 4*MAD 5 = L 170: inside 160..200
	d.want(Full, "classified FULL")
	for k := 0; k < 30; k++ {
		d.feed(time.Duration(145+k%3*5) * ms) // L stays 170
	}
	d.want(Full, "inside dead band stays FULL")

	d2 := newDriver(t)
	d2.feed(240*ms, 245*ms, 250*ms)
	d2.want(Degraded, "classified DEGRADED")
	for k := 0; k < 30; k++ {
		d2.feed(170 * ms) // below 200 but not below 160: no promotion
	}
	d2.want(Degraded, "inside dead band stays DEGRADED")
}

// Server location sets the ceiling: RTT ≈ 2·d·r/200 km/ms + ~10 ms.
func TestLocationProfiles(t *testing.T) {
	cases := []struct {
		name string
		rtt  time.Duration
		want Tier
	}{
		{"same region (~20 ms)", 20 * ms, Full},
		{"India->Singapore (~90 ms)", 90 * ms, Full},
		{"India->US west (~250 ms)", 250 * ms, Degraded},
		{"antipode (~410 ms)", 410 * ms, Degraded},
		{"GEO satellite (~650 ms)", 650 * ms, Minimal},
	}
	for _, c := range cases {
		d := newDriver(t)
		for k := 0; k < 60; k++ {
			d.feed(c.rtt + time.Duration(k%3)*ms)
		}
		if d.m.Tier() != c.want {
			t.Errorf("%s: %v want %v", c.name, d.m.Tier(), c.want)
		}
	}
}

func TestMissingReports(t *testing.T) {
	d := newDriver(t)
	d.healthy(10)
	d.want(Full, "baseline")
	if r := d.m.CheckTimeout(d.now.Add(2 * time.Second)); r != "" || d.m.Tier() != Full {
		t.Fatal("2 s of silence should not demote")
	}
	if r := d.m.CheckTimeout(d.now.Add(3 * time.Second)); r == "" || d.m.Tier() != Degraded {
		t.Fatalf("3 s of silence should demote to DEGRADED, got %v", d.m.Tier())
	}
	if r := d.m.CheckTimeout(d.now.Add(6 * time.Second)); r == "" || d.m.Tier() != Minimal {
		t.Fatalf("6 s of silence should demote to MINIMAL, got %v", d.m.Tier())
	}
	d.now = d.now.Add(7 * time.Second)
	d.healthy(4)
	d.want(Minimal, "4 good reports must not promote yet")
	d.healthy(1)
	d.want(Degraded, "5th good report promotes one step")
}

func TestThresholdsDerivedFromChartIntervals(t *testing.T) {
	if DemoteToDegraded != 200*ms || DemoteToMinimal != 600*ms || PromoteToFull != 160*ms || PromoteToDegraded != 480*ms {
		t.Fatal("thresholds must be 2x chart interval with a 20% promotion band")
	}
	for l, want := range map[time.Duration]Tier{0: Full, 199 * ms: Full, 200 * ms: Degraded, 599 * ms: Degraded, 600 * ms: Minimal} {
		if got := Classify(l); got != want {
			t.Errorf("Classify(%v)=%v want %v", l, got, want)
		}
	}
}
