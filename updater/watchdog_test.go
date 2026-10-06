package updater

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// fakeClock lets tests run minutes of watchdog time instantly.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time        { return c.t }
func (c *fakeClock) Sleep(d time.Duration) { c.t = c.t.Add(d) }

// step describes the service at a point in (fake) time.
type step struct {
	at    time.Duration // offset from start at which this state begins
	state svcState
	pid   uint32
}

// fakeService replays a timeline of states and records Start/Stop calls.
// Start() can optionally "bring the service up" by appending a new state.
type fakeService struct {
	clk       *fakeClock
	begin     time.Time
	timeline  []step
	starts    []time.Duration
	stops     []time.Duration
	startErr  error
	onStart   func(f *fakeService, at time.Duration) // may mutate timeline
	stopKills bool                                   // Stop() moves the service to stopped
}

func (f *fakeService) now() time.Duration { return f.clk.Now().Sub(f.begin) }

func (f *fakeService) Status() (svcState, uint32, error) {
	cur := f.timeline[0]
	for _, s := range f.timeline {
		if s.at <= f.now() {
			cur = s
		}
	}
	return cur.state, cur.pid, nil
}
func (f *fakeService) Start() error {
	f.starts = append(f.starts, f.now())
	if f.startErr != nil {
		return f.startErr
	}
	if f.onStart != nil {
		f.onStart(f, f.now())
	}
	return nil
}
func (f *fakeService) Stop() error {
	f.stops = append(f.stops, f.now())
	if f.stopKills {
		f.timeline = append(f.timeline, step{at: f.now(), state: svcStopped, pid: 0})
	}
	return nil
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newHarness(timeline []step) (*fakeService, watchdogParams) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	f := &fakeService{clk: clk, begin: clk.t, timeline: timeline, stopKills: true}
	p := defaultWatchdogParams(100, "9.9.9")
	p.Now, p.Sleep = clk.Now, clk.Sleep
	return f, p
}

const oldPID = 100

func TestWatchdog_SCMRestartsPromptly_Healthy(t *testing.T) {
	// Old process dies; SCM restarts the service as pid 200 after 5s.
	f, p := newHarness([]step{
		{0, svcRunning, oldPID},
		{1 * time.Second, svcStopped, 0},
		{6 * time.Second, svcRunning, 200},
	})
	swapped, marked := false, false
	out := runWatchdog(f, p, func() error { swapped = true; return nil }, func() error { marked = true; return nil }, quietLog())
	if out != OutcomeHealthy {
		t.Fatalf("outcome = %v, want healthy", out)
	}
	if len(f.starts) != 0 {
		t.Errorf("watchdog started the service %d times, but the SCM had already restarted it", len(f.starts))
	}
	if swapped || marked {
		t.Error("a healthy update must not roll back or mark the version bad")
	}
}

// THE regression: SCM recovery policy is "take no action", service stays down.
func TestWatchdog_SCMNeverRestarts_WatchdogStartsIt(t *testing.T) {
	f, p := newHarness([]step{
		{0, svcRunning, oldPID},
		{1 * time.Second, svcStopped, 0},
	})
	f.onStart = func(f *fakeService, at time.Duration) {
		f.timeline = append(f.timeline, step{at: at + 3*time.Second, state: svcRunning, pid: 300})
	}
	swapped := false
	out := runWatchdog(f, p, func() error { swapped = true; return nil }, func() error { return nil }, quietLog())
	if out != OutcomeHealthy {
		t.Fatalf("outcome = %v, want healthy", out)
	}
	if len(f.starts) != 1 {
		t.Fatalf("watchdog Start calls = %d, want exactly 1", len(f.starts))
	}
	if f.starts[0] < p.RestartGrace {
		t.Errorf("watchdog started the service at %v, before the %v grace period for the SCM", f.starts[0], p.RestartGrace)
	}
	if swapped {
		t.Error("must not roll back when the only problem was a missing SCM restart")
	}
}

func TestWatchdog_NewVersionCrashLoops_RollsBack(t *testing.T) {
	// New binary starts (pid 201, 202, 203) but dies each time within seconds.
	f, p := newHarness([]step{
		{0, svcRunning, oldPID},
		{1 * time.Second, svcStopped, 0},
		{6 * time.Second, svcRunning, 201},
		{8 * time.Second, svcStopped, 0},
		{13 * time.Second, svcRunning, 202},
		{15 * time.Second, svcStopped, 0},
		{20 * time.Second, svcRunning, 203},
		{22 * time.Second, svcStopped, 0},
	})
	var order []string
	f.onStart = func(f *fakeService, at time.Duration) {
		// After rollback the old binary starts and stays up.
		f.timeline = append(f.timeline, step{at: at + time.Second, state: svcRunning, pid: 400})
	}
	out := runWatchdog(f, p,
		func() error { order = append(order, "swapBack"); return nil },
		func() error { order = append(order, "markBad"); return nil },
		quietLog())
	if out != OutcomeRolledBack {
		t.Fatalf("outcome = %v, want rolled-back", out)
	}
	if len(order) != 2 || order[0] != "markBad" || order[1] != "swapBack" {
		t.Errorf("rollback order = %v, want [markBad swapBack] (mark first so a half-failed rollback can't reinstall the bad release)", order)
	}
	if len(f.starts) == 0 {
		t.Error("after rollback the service must be started again")
	}
}

func TestWatchdog_NewVersionHangsNeverStable_RollsBackAtDeadline(t *testing.T) {
	// Process stays "starting" forever (never reaches Running).
	f, p := newHarness([]step{
		{0, svcRunning, oldPID},
		{1 * time.Second, svcStopped, 0},
		{6 * time.Second, svcPending, 500},
	})
	f.onStart = func(f *fakeService, at time.Duration) {
		f.timeline = append(f.timeline, step{at: at + time.Second, state: svcRunning, pid: 600})
	}
	rolled := false
	out := runWatchdog(f, p, func() error { rolled = true; return nil }, func() error { return nil }, quietLog())
	if out != OutcomeRolledBack || !rolled {
		t.Fatalf("outcome = %v rolled=%v, want rolled-back", out, rolled)
	}
}

func TestWatchdog_ServiceCannotBeStartedAtAll_DoesNotBlameBinary(t *testing.T) {
	// e.g. the service account's password changed: every Start is refused.
	f, p := newHarness([]step{
		{0, svcRunning, oldPID},
		{1 * time.Second, svcStopped, 0},
	})
	f.startErr = errors.New("logon failure")
	p.IsEnvError = func(error) bool { return true }
	rolled, marked := false, false
	out := runWatchdog(f, p, func() error { rolled = true; return nil }, func() error { marked = true; return nil }, quietLog())
	if out != OutcomeGaveUp {
		t.Fatalf("outcome = %v, want gave-up", out)
	}
	if rolled || marked {
		t.Error("an environment failure must not roll back or blacklist a good release")
	}
	if len(f.starts) < 2 {
		t.Errorf("watchdog should keep retrying Start; got %d attempts", len(f.starts))
	}
}

// A corrupt/unrunnable new binary makes Start itself fail ("not a valid Win32
// application", or a timeout because the process died before reporting). That
// implicates the binary, so it must roll back - not give up.
func TestWatchdog_StartFailsBecauseBinaryIsBroken_RollsBack(t *testing.T) {
	f, p := newHarness([]step{
		{0, svcRunning, oldPID},
		{1 * time.Second, svcStopped, 0},
	})
	f.startErr = errors.New("%1 is not a valid Win32 application")
	p.IsEnvError = func(error) bool { return false }
	rolled, marked := false, false
	f.stopKills = true
	out := runWatchdog(f, p, func() error { rolled = true; f.startErr = nil; return nil }, func() error { marked = true; return nil }, quietLog())
	if !rolled || !marked {
		t.Fatalf("outcome = %v rolled=%v marked=%v, want a rollback", out, rolled, marked)
	}
	if f.starts[len(f.starts)-1] > p.Deadline/2 {
		t.Errorf("rollback should trigger after %d failed starts, not wait for the %v deadline (last start at %v)", p.MaxBinErrors, p.Deadline, f.starts[len(f.starts)-1])
	}
}

func TestWatchdog_RollbackFailure_StillTriesToStart(t *testing.T) {
	f, p := newHarness([]step{
		{0, svcRunning, oldPID},
		{1 * time.Second, svcStopped, 0},
		{6 * time.Second, svcRunning, 201}, {8 * time.Second, svcStopped, 0},
		{13 * time.Second, svcRunning, 202}, {15 * time.Second, svcStopped, 0},
		{20 * time.Second, svcRunning, 203}, {22 * time.Second, svcStopped, 0},
	})
	out := runWatchdog(f, p, func() error { return errors.New("file locked") }, func() error { return nil }, quietLog())
	if out != OutcomeGaveUp {
		t.Fatalf("outcome = %v, want gave-up", out)
	}
	if len(f.starts) == 0 {
		t.Error("even if swap-back fails the watchdog must try to start the service")
	}
}

func TestWatchdog_OldProcessSlowToExit_IsNotMistakenForNewVersion(t *testing.T) {
	// Old pid lingers as "running" for a while before exiting; must not count
	// as the new version being healthy.
	f, p := newHarness([]step{
		{0, svcRunning, oldPID},
		{60 * time.Second, svcStopped, 0}, // old exits very late
		{65 * time.Second, svcRunning, 200},
	})
	out := runWatchdog(f, p, func() error { return nil }, func() error { return nil }, quietLog())
	if out != OutcomeHealthy {
		t.Fatalf("outcome = %v, want healthy (old pid must never count as the new version)", out)
	}
	if len(f.starts) > 1 {
		t.Errorf("unexpected extra starts: %d", len(f.starts))
	}
}

func TestRestorePrevious_PutsOldBackAndKeepsBad(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "vectrify-runner.exe")
	writeFile(t, exe, "NEW-BAD")
	writeFile(t, exe+".old", "OLD-GOOD")

	if err := restorePrevious(exe, exe+".old"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, exe); got != "OLD-GOOD" {
		t.Errorf("exe = %q, want OLD-GOOD", got)
	}
	if exists(exe + ".old") {
		t.Error(".old should have been consumed")
	}
	bad, _ := filepath.Glob(exe + ".bad.*")
	if len(bad) != 1 || readFile(t, bad[0]) != "NEW-BAD" {
		t.Errorf("bad binary should be preserved for diagnosis, got %v", bad)
	}
}

func TestRestorePrevious_NoOld_LeavesCurrentUntouched(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "vectrify-runner.exe")
	writeFile(t, exe, "CURRENT")
	if err := restorePrevious(exe, exe+".old"); err == nil {
		t.Fatal("expected an error when there is no previous binary")
	}
	if readFile(t, exe) != "CURRENT" {
		t.Error("current binary must be left alone when rollback is impossible")
	}
}

func TestBadVersionMarker(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "vectrify-runner.exe")
	if isBadVersion(exe, "1.0.24") {
		t.Fatal("nothing marked yet")
	}
	if err := markBadVersion(exe, "1.0.24"); err != nil {
		t.Fatal(err)
	}
	if !isBadVersion(exe, "1.0.24") {
		t.Error("1.0.24 should be marked bad")
	}
	if isBadVersion(exe, "1.0.25") {
		t.Error("a newer release must still be eligible")
	}
}

func TestRemoveLeftovers_AlsoClearsBadBinaries(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "vectrify-runner.exe")
	writeFile(t, exe, "CURRENT")
	writeFile(t, exe+".bad.123", "x")
	removeLeftovers(exe)
	if exists(exe + ".bad.123") {
		t.Error(".bad.* should be cleaned up")
	}
	if !exists(exe) {
		t.Error("current binary must never be removed")
	}
}
