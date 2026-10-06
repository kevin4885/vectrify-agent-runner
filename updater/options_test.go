package updater

import (
	"testing"
	"time"
)

func gateAt(clk *fakeClock, a *Activity) *gate {
	opts := Options{CheckInterval: 5 * time.Minute, IdleWindow: 5 * time.Minute,
		Activity: func() Activity { return *a }}
	return newGate(opts, clk.Now)
}

func TestGate_IdleRunner_Proceeds(t *testing.T) {
	clk := &fakeClock{t: time.Unix(2_000_000, 0)}
	a := &Activity{LastActive: clk.Now().Add(-10 * time.Minute)}
	g := gateAt(clk, a)
	if ok, forced := g.evaluate(quietLog(), "1.0.0", "1.0.1"); !ok || forced {
		t.Fatalf("idle runner: proceed=%v forced=%v, want true,false", ok, forced)
	}
}

func TestGate_InFlightCommand_Defers(t *testing.T) {
	clk := &fakeClock{t: time.Unix(2_000_000, 0)}
	a := &Activity{Busy: true, LastActive: clk.Now().Add(-time.Hour)} // old LastActive must not matter
	g := gateAt(clk, a)
	if ok, _ := g.evaluate(quietLog(), "1.0.0", "1.0.1"); ok {
		t.Fatal("a command in flight must defer the update")
	}
}

func TestGate_RecentActivity_DefersUntilWindowElapses(t *testing.T) {
	clk := &fakeClock{t: time.Unix(2_000_000, 0)}
	a := &Activity{LastActive: clk.Now().Add(-4*time.Minute - 59*time.Second)}
	g := gateAt(clk, a)
	if ok, _ := g.evaluate(quietLog(), "1.0.0", "1.0.1"); ok {
		t.Fatal("4m59s since last activity is inside the 5m window: must defer")
	}
	clk.Sleep(2 * time.Second) // now 5m01s
	if ok, _ := g.evaluate(quietLog(), "1.0.0", "1.0.1"); !ok {
		t.Fatal("5m01s since last activity: must proceed")
	}
}

// A freshly started runner has lastActive == start time, so it never updates
// during its first idle window (this also stops update->restart->update chains).
func TestGate_FreshStart_CountsAsActive(t *testing.T) {
	clk := &fakeClock{t: time.Unix(2_000_000, 0)}
	a := &Activity{LastActive: clk.Now()}
	g := gateAt(clk, a)
	if ok, _ := g.evaluate(quietLog(), "1.0.0", "1.0.1"); ok {
		t.Fatal("just-started runner must not update immediately")
	}
}

func TestGate_NoActivityEverRecorded_Proceeds(t *testing.T) {
	clk := &fakeClock{t: time.Unix(2_000_000, 0)}
	g := gateAt(clk, &Activity{})
	if ok, _ := g.evaluate(quietLog(), "1.0.0", "1.0.1"); !ok {
		t.Fatal("zero LastActive means no activity: proceed")
	}
}

func TestGate_NilActivitySource_AlwaysIdle(t *testing.T) {
	g := newGate(Options{}, time.Now)
	if ok, _ := g.evaluate(quietLog(), "1.0.0", "1.0.1"); !ok {
		t.Fatal("no activity source configured: proceed")
	}
}

// Decision 2b: continuous activity must not block an update forever.
func TestGate_DeferralCap_ForcesUpdateAfterSixHours(t *testing.T) {
	clk := &fakeClock{t: time.Unix(2_000_000, 0)}
	a := &Activity{Busy: true}
	g := gateAt(clk, a)

	if ok, _ := g.evaluate(quietLog(), "1.0.0", "1.0.1"); ok { // starts the deferral clock
		t.Fatal("first check while busy must defer")
	}
	clk.Sleep(maxDeferral - 5*time.Minute)
	if ok, _ := g.evaluate(quietLog(), "1.0.0", "1.0.1"); ok {
		t.Fatal("just under the cap: still deferred")
	}
	clk.Sleep(5*time.Minute + time.Second)
	ok, forced := g.evaluate(quietLog(), "1.0.0", "1.0.1")
	if !ok || !forced {
		t.Fatalf("at the cap: proceed=%v forced=%v, want true,true", ok, forced)
	}
}

// If the runner goes idle, then busy again, the 6h clock restarts - the cap
// measures CONTINUOUS deferral, not total.
func TestGate_DeferralClockResetsWhenIdleOrUpToDate(t *testing.T) {
	clk := &fakeClock{t: time.Unix(2_000_000, 0)}
	a := &Activity{Busy: true}
	g := gateAt(clk, a)
	g.evaluate(quietLog(), "1.0.0", "1.0.1")
	clk.Sleep(5 * time.Hour)

	a.Busy = false
	a.LastActive = clk.Now().Add(-time.Hour)
	if ok, _ := g.evaluate(quietLog(), "1.0.0", "1.0.1"); !ok { // idle -> resets
		t.Fatal("idle: should proceed")
	}

	a.Busy = true
	g.evaluate(quietLog(), "1.0.0", "1.0.1") // new deferral starts now
	clk.Sleep(2 * time.Hour)                 // 7h after the original start, but only 2h since the reset
	if ok, forced := g.evaluate(quietLog(), "1.0.0", "1.0.1"); ok || forced {
		t.Fatalf("deferral clock should have restarted: proceed=%v forced=%v", ok, forced)
	}
}

func TestOptions_Defaults(t *testing.T) {
	o := Options{}.withDefaults()
	if o.CheckInterval != 5*time.Minute || o.IdleWindow != 5*time.Minute {
		t.Fatalf("defaults = %v / %v, want 5m / 5m", o.CheckInterval, o.IdleWindow)
	}
	o = Options{CheckInterval: time.Minute, IdleWindow: 10 * time.Minute}.withDefaults()
	if o.CheckInterval != time.Minute || o.IdleWindow != 10*time.Minute {
		t.Fatalf("explicit values must be kept, got %v / %v", o.CheckInterval, o.IdleWindow)
	}
}
