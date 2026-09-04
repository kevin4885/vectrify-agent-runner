package client

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vectrify/agent-runner/config"
	"vectrify/agent-runner/protocol"
)

// newTestClient builds a Client with the given global/heavy limits and
// acquire timeout, and a no-op logger, without starting the background
// staleness monitor (not needed by these tests and it would otherwise leak
// a goroutine per test). dispatchFunc defaults to an instant no-op;
// individual tests override it. acquireTimeout is injected directly as a
// time.Duration (not whole seconds like the real YAML config knob) so
// bounded-wait tests can use short sub-second timeouts and stay fast.
func newTestClient(maxConcurrency, maxHeavy int, acquireTimeout time.Duration) *Client {
	cfg := &config.Config{
		MaxConcurrency:      maxConcurrency,
		MaxHeavyConcurrency: maxHeavy,
	}
	c := &Client{
		cfg:      cfg,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		inflight: newInflightRegistry(),
	}
	c.dispatchFunc = func(raw protocol.RawCommand, send func(interface{}), triggerReconnect func()) {}
	c.acquireTimeout = func() time.Duration { return acquireTimeout }
	return c
}

func rawCmd(cmdID, cmdType string) protocol.RawCommand {
	return protocol.RawCommand{"cmd_id": cmdID, "type": cmdType}
}

func discardSend(interface{}) {}

// 2. Heavy sub-limit: heavy commands are refused once the heavy cap is
// reached even when the global limit still has room; light commands still
// succeed in that state. This is the anti-starvation guarantee.
func TestDispatchOne_HeavySubLimit_LightStillSucceeds(t *testing.T) {
	// global=10, heavy=2: fill the heavy sub-limit with 2 long-running shells,
	// leaving 8 global slots free. A 3rd heavy command must be rejected; a
	// light command must still be admitted.
	c := newTestClient(10, 2, 200*time.Millisecond)

	release := make(chan struct{})
	started := make(chan struct{}, 2)
	c.dispatchFunc = func(raw protocol.RawCommand, send func(interface{}), triggerReconnect func()) {
		started <- struct{}{}
		<-release
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.dispatchOne(rawCmd(fmt.Sprintf("heavy-%d", i), "shell"), fmt.Sprintf("heavy-%d", i), "shell", discardSend, nil)
		}(i)
	}
	for i := 0; i < 2; i++ {
		<-started
	}

	// Heavy sub-limit is now full (2/2); global is only 2/10.
	var gotMsg string
	var mu sync.Mutex
	blockedSend := func(msg interface{}) {
		if em, ok := msg.(protocol.ErrorMsg); ok {
			mu.Lock()
			gotMsg = em.Message
			mu.Unlock()
		}
	}
	c.dispatchOne(rawCmd("heavy-3", "shell"), "heavy-3", "shell", blockedSend, nil)
	mu.Lock()
	msg := gotMsg
	mu.Unlock()
	if msg == "" {
		t.Fatal("3rd heavy command was admitted; want rejection (heavy sub-limit reached)")
	}
	if !contains(msg, "runner busy") {
		t.Fatalf("rejection message = %q, want it to contain %q", msg, "runner busy")
	}
	if !contains(msg, "heavy-command concurrency limit") {
		t.Fatalf("rejection message = %q, want it to identify the heavy sub-limit specifically (not just the generic global-limit text)", msg)
	}

	// A light command must still succeed — this is the anti-starvation
	// guarantee: heavy commands saturating their sub-limit must never block
	// light commands while global capacity remains.
	lightRan := make(chan struct{})
	c.dispatchFunc = func(raw protocol.RawCommand, send func(interface{}), triggerReconnect func()) {
		close(lightRan)
	}
	c.dispatchOne(rawCmd("light-1", "file_op"), "light-1", "file_op", discardSend, nil)
	select {
	case <-lightRan:
	default:
		t.Fatal("light command was not dispatched while heavy sub-limit was full but global capacity remained")
	}

	close(release)
	wg.Wait()
}

// 3. Global limit still enforced for light commands: once the global limit
// is reached (even with zero heavy commands running), a light command must
// be rejected too.
func TestDispatchOne_GlobalLimit_EnforcedForLightCommands(t *testing.T) {
	c := newTestClient(2, 2, 200*time.Millisecond) // global=2, heavy=2 (irrelevant here — all light)

	release := make(chan struct{})
	started := make(chan struct{}, 2)
	c.dispatchFunc = func(raw protocol.RawCommand, send func(interface{}), triggerReconnect func()) {
		started <- struct{}{}
		<-release
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("light-%d", i)
			c.dispatchOne(rawCmd(id, "file_op"), id, "file_op", discardSend, nil)
		}(i)
	}
	for i := 0; i < 2; i++ {
		<-started
	}

	var gotMsg string
	blockedSend := func(msg interface{}) {
		if em, ok := msg.(protocol.ErrorMsg); ok {
			gotMsg = em.Message
		}
	}
	c.dispatchOne(rawCmd("light-2", "file_op"), "light-2", "file_op", blockedSend, nil)
	if gotMsg == "" {
		t.Fatal("3rd light command was admitted while global limit was full; want rejection")
	}
	if !contains(gotMsg, "runner busy") {
		t.Fatalf("rejection message = %q, want it to contain %q", gotMsg, "runner busy")
	}

	close(release)
	wg.Wait()
}

// 4. Bounded acquire: succeeds when a slot frees before the timeout; fails
// (rejects) when none frees. Uses a short injected timeout to keep the test
// fast and deterministic (no reliance on real elapsed wall-clock waiting
// beyond the short timeout itself).
func TestDispatchOne_BoundedAcquire_SucceedsWhenSlotFreesInTime(t *testing.T) {
	c := newTestClient(1, 1, 500*time.Millisecond) // acquire timeout comfortably above the below delay

	release := make(chan struct{})
	holderDone := make(chan struct{})
	waiterAdmitted := make(chan struct{})
	// A single stable dispatchFunc dispatches on cmd_id rather than being
	// reassigned mid-test from another goroutine — reassigning a shared
	// field concurrently would itself be a data race independent of the
	// mechanism under test.
	c.dispatchFunc = func(raw protocol.RawCommand, send func(interface{}), triggerReconnect func()) {
		switch raw.CmdID() {
		case "holder":
			<-release
		case "waiter":
			close(waiterAdmitted)
		}
	}

	go func() {
		c.dispatchOne(rawCmd("holder", "file_op"), "holder", "file_op", discardSend, nil)
		close(holderDone)
	}()
	// Give the holder a moment to actually acquire before the waiter tries.
	waitUntilCount(t, c, 1)

	go c.dispatchOne(rawCmd("waiter", "file_op"), "waiter", "file_op", discardSend, nil)

	// Free the slot shortly after the waiter starts blocking — well within
	// the acquire timeout above.
	time.Sleep(20 * time.Millisecond)
	close(release)
	<-holderDone

	select {
	case <-waiterAdmitted:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was never admitted after the slot freed within the timeout")
	}
}

func TestDispatchOne_BoundedAcquire_RejectsWhenNoSlotFreesInTime(t *testing.T) {
	const timeout = 80 * time.Millisecond
	c := newTestClient(1, 1, timeout) // never freed below

	release := make(chan struct{}) // never closed in this test
	defer close(release)           // avoid leaking the holder goroutine after the test ends
	c.dispatchFunc = func(raw protocol.RawCommand, send func(interface{}), triggerReconnect func()) {
		<-release
	}
	go c.dispatchOne(rawCmd("holder", "file_op"), "holder", "file_op", discardSend, nil)
	waitUntilCount(t, c, 1)

	var gotMsg string
	blockedSend := func(msg interface{}) {
		if em, ok := msg.(protocol.ErrorMsg); ok {
			gotMsg = em.Message
		}
	}
	start := time.Now()
	c.dispatchOne(rawCmd("waiter", "file_op"), "waiter", "file_op", blockedSend, nil)
	elapsed := time.Since(start)

	if gotMsg == "" {
		t.Fatal("waiter was admitted despite the only slot never freeing; want rejection")
	}
	if elapsed < timeout*8/10 {
		t.Fatalf("rejection returned after only %v; want it to have actually waited out the ~%v timeout", elapsed, timeout)
	}
}

// 5. Release-on-panic: a panicking dispatch still releases its slot and
// registry entry.
func TestDispatchOne_ReleaseOnPanic(t *testing.T) {
	c := newTestClient(1, 1, 200*time.Millisecond)
	c.dispatchFunc = func(raw protocol.RawCommand, send func(interface{}), triggerReconnect func()) {
		panic("boom")
	}

	c.dispatchOne(rawCmd("panics", "file_op"), "panics", "file_op", discardSend, nil)

	if got := c.inflight.count(); got != 0 {
		t.Fatalf("inflight count after panicking dispatch = %d, want 0 (slot must be released)", got)
	}

	// The slot must be genuinely usable again, not just reporting count==0
	// while still internally reserved.
	ran := make(chan struct{})
	c.dispatchFunc = func(raw protocol.RawCommand, send func(interface{}), triggerReconnect func()) {
		close(ran)
	}
	c.dispatchOne(rawCmd("after", "file_op"), "after", "file_op", discardSend, nil)
	select {
	case <-ran:
	default:
		t.Fatal("dispatch after a panic was not admitted; slot appears leaked")
	}
}

// Regression test for the pendingWaiters accounting fix: releaseWaiter()
// must fire the instant acquire() returns (success OR rejection), NOT be
// deferred to the end of dispatchOne — otherwise pendingWaiters would keep
// counting a goroutine that already won its slot and is now running the
// (potentially long) dispatchFunc, silently turning maxPendingSlotWaiters
// into a second, lower concurrency ceiling than MaxConcurrency. This
// mirrors the real hand-off in connect(): tryReserveWaiter() before
// spawning the goroutine, dispatchOne() as its body.
func TestDispatchOne_PendingWaiterReleased_AsSoonAsAcquireReturns_NotAfterDispatchFunc(t *testing.T) {
	c := newTestClient(1, 1, 500*time.Millisecond)

	dispatchFuncRunning := make(chan struct{})
	releaseDispatchFunc := make(chan struct{})
	c.dispatchFunc = func(raw protocol.RawCommand, send func(interface{}), triggerReconnect func()) {
		close(dispatchFuncRunning)
		<-releaseDispatchFunc
	}

	if !c.tryReserveWaiter() {
		t.Fatal("tryReserveWaiter() unexpectedly failed at the start of the test")
	}
	go c.dispatchOne(rawCmd("holder", "file_op"), "holder", "file_op", discardSend, nil)

	// Wait for dispatchFunc to actually start running (i.e. acquire()
	// already succeeded and returned).
	select {
	case <-dispatchFuncRunning:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatchFunc never started running")
	}

	// The defining assertion: while dispatchFunc is still running (holding
	// its slot, NOT waiting for one), pendingWaiterCount() must already be
	// back to 0. If releaseWaiter() were still deferred to the end of
	// dispatchOne (the bug this test guards against), this would read 1
	// here instead.
	if got := c.pendingWaiterCount(); got != 0 {
		t.Fatalf("pendingWaiterCount() while dispatchFunc is running (not waiting) = %d, want 0 — releaseWaiter() is not firing as soon as acquire() returns", got)
	}

	close(releaseDispatchFunc)
	waitUntilCount(t, c, 0)
	if got := c.pendingWaiterCount(); got != 0 {
		t.Fatalf("pendingWaiterCount() after dispatchOne fully completed = %d, want 0", got)
	}
}

// 6. Pending-wait cap: exceeding it rejects immediately rather than growing
// goroutines without bound. This test exercises tryReserveWaiter/releaseWaiter
// directly (the mechanism gating hand-off goroutine creation in connect()),
// since dispatchOne itself is the body of an already-spawned goroutine.
func TestPendingWaiterCap_RejectsImmediatelyWhenExceeded(t *testing.T) {
	c := newTestClient(100, 100, 200*time.Millisecond)

	for i := 0; i < maxPendingSlotWaiters; i++ {
		if !c.tryReserveWaiter() {
			t.Fatalf("tryReserveWaiter() returned false before reaching the cap (i=%d)", i)
		}
	}
	if c.tryReserveWaiter() {
		t.Fatal("tryReserveWaiter() succeeded beyond maxPendingSlotWaiters; cap not enforced")
	}

	c.releaseWaiter()
	if !c.tryReserveWaiter() {
		t.Fatal("tryReserveWaiter() failed immediately after releaseWaiter() freed a slot")
	}
}

// Confirm no goroutine leak from the hand-off path: waiters actually exit
// after the acquire timeout rather than blocking forever.
func TestDispatchOne_WaiterGoroutineExitsAfterTimeout(t *testing.T) {
	const timeout = 100 * time.Millisecond
	c := newTestClient(1, 1, timeout)

	release := make(chan struct{})
	defer close(release)
	c.dispatchFunc = func(raw protocol.RawCommand, send func(interface{}), triggerReconnect func()) {
		<-release
	}
	go c.dispatchOne(rawCmd("holder", "file_op"), "holder", "file_op", discardSend, nil)
	waitUntilCount(t, c, 1)

	waiterDone := make(chan struct{})
	go func() {
		c.dispatchOne(rawCmd("waiter", "file_op"), "waiter", "file_op", discardSend, nil)
		close(waiterDone)
	}()

	select {
	case <-waiterDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("waiter goroutine did not exit within 2s of a %v acquire timeout — possible goroutine leak", timeout)
	}
}

// Regression test for the Drain() fix: Drain must treat a nonzero
// pendingWaiterCount() as "still busy" exactly like a nonzero
// inflight.count() — a command parked in acquire() waiting for a free slot
// has not yet been added to the inflight registry, so checking
// inflight.count() alone would let Drain return while that waiter is about
// to win a slot and start a brand-new command, which the caller's
// immediately-following os.Exit would then kill mid-flight with no result
// ever sent.
//
// Directly manipulates tryReserveWaiter()/releaseWaiter() (the same
// primitives the real hand-off in connect() uses) rather than racing the
// real acquire() timing, so the assertion "Drain has not returned" is
// unconditionally true for as long as the reserved waiter slot is held,
// with no dependency on scheduling.
func TestDrain_WaitsForPendingWaiters_NotJustInflightRegistry(t *testing.T) {
	c := newTestClient(1, 1, 200*time.Millisecond)

	// inflight.count() is 0 throughout this test — the only thing keeping
	// Drain from returning immediately must be pendingWaiterCount().
	if got := c.inflight.count(); got != 0 {
		t.Fatalf("inflight count = %d, want 0 (test precondition)", got)
	}
	if !c.tryReserveWaiter() {
		t.Fatal("tryReserveWaiter() unexpectedly failed")
	}

	drainDone := make(chan struct{})
	go func() {
		c.Drain(5 * time.Second)
		close(drainDone)
	}()

	select {
	case <-drainDone:
		t.Fatal("Drain returned while pendingWaiterCount() > 0 and inflight.count() == 0 — a parked waiter is invisible to Drain")
	case <-time.After(300 * time.Millisecond):
		// Expected: Drain is still polling, since pendingWaiterCount() == 1.
	}

	c.releaseWaiter()

	select {
	case <-drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return promptly after pendingWaiterCount() dropped to 0")
	}
}

// 8. Classification function: known types map to the right class; unknown
// types default to light.
// Stress the broadcast/channel-swap wakeup mechanism itself under genuine
// contention: many more acquirers than available slots, each holding
// briefly then releasing, across both classes simultaneously. This is the
// highest-risk code in this change (a missed wakeup here would manifest as
// a command hanging until ctx times out, or — if broken the other way — the
// global/heavy limits being exceeded), so assert BOTH that every acquirer
// eventually gets admitted (no missed wakeup) AND that the global/heavy
// caps are never exceeded at any point a goroutine is holding its slot.
func TestDispatchOne_ManyWaiters_AllEventuallyAdmitted_LimitsNeverExceeded(t *testing.T) {
	const maxGlobal = 2
	const maxHeavy = 1
	const acquirers = 20
	const perAcquireHold = 5 * time.Millisecond
	const acquireTimeout = 3 * time.Second // generous: this test is about correctness, not speed

	c := newTestClient(maxGlobal, maxHeavy, acquireTimeout)

	var mu sync.Mutex
	activeGlobal, activeHeavy := 0, 0
	violated := false
	c.dispatchFunc = func(raw protocol.RawCommand, send func(interface{}), triggerReconnect func()) {
		mu.Lock()
		activeGlobal++
		if raw.Type() == "shell" {
			activeHeavy++
		}
		if activeGlobal > maxGlobal || activeHeavy > maxHeavy {
			violated = true
		}
		mu.Unlock()

		time.Sleep(perAcquireHold)

		mu.Lock()
		activeGlobal--
		if raw.Type() == "shell" {
			activeHeavy--
		}
		mu.Unlock()
	}

	var wg sync.WaitGroup
	var rejectedCount int32
	wg.Add(acquirers)
	for i := 0; i < acquirers; i++ {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("acq-%d", i)
			cmdType := "file_op"
			if i%2 == 0 {
				cmdType = "shell"
			}
			var rejected bool
			send := func(msg interface{}) {
				if _, ok := msg.(protocol.ErrorMsg); ok {
					rejected = true
				}
			}
			c.dispatchOne(rawCmd(id, cmdType), id, cmdType, send, nil)
			if rejected {
				atomic.AddInt32(&rejectedCount, 1)
			}
		}(i)
	}
	wg.Wait()

	if violated {
		t.Fatal("global or heavy concurrency limit was exceeded at some point under contention")
	}
	// With a 3s timeout and only 5ms holds, every one of the 20 acquirers
	// should comfortably be admitted eventually — a missed wakeup would
	// instead show up as spurious timeout-rejections here.
	if rejectedCount != 0 {
		t.Fatalf("%d/%d acquirers were rejected despite a generous acquire timeout; possible missed wakeup", rejectedCount, acquirers)
	}
	if got := c.inflight.count(); got != 0 {
		t.Fatalf("inflight count after all acquirers finished = %d, want 0", got)
	}
}

// Regression coverage for the once-guard itself: releaseWaiter() now floors
// at 0, which would silently hide a double-release bug from a naive
// "ends at 0" assertion. This test starts with TWO reserved waiter slots
// (via two tryReserveWaiter() calls) and runs exactly one dispatchOne(),
// asserting pendingWaiterCount() == 1 afterward — if dispatchOne's
// once-guard ever regressed to releasing twice, this would read 0 instead
// of 1, whereas a single-reservation test (starting from 1) could not tell
// the difference from a correct single release.
func TestDispatchOne_ReleasesExactlyOneWaiterSlot_NotTwo(t *testing.T) {
	c := newTestClient(1, 1, 200*time.Millisecond)

	if !c.tryReserveWaiter() {
		t.Fatal("first tryReserveWaiter() unexpectedly failed")
	}
	if !c.tryReserveWaiter() {
		t.Fatal("second tryReserveWaiter() unexpectedly failed")
	}
	if got := c.pendingWaiterCount(); got != 2 {
		t.Fatalf("pendingWaiterCount() after two reservations = %d, want 2", got)
	}

	c.dispatchOne(rawCmd("only-one", "file_op"), "only-one", "file_op", discardSend, nil)

	if got := c.pendingWaiterCount(); got != 1 {
		t.Fatalf("pendingWaiterCount() after exactly one dispatchOne() call = %d, want 1 (dispatchOne must release exactly one reservation, not two)", got)
	}
	c.releaseWaiter() // clean up the second, test-held reservation
}

func TestClassifyCommand(t *testing.T) {
	cases := []struct {
		cmdType string
		want    commandClass
	}{
		{"shell", classHeavy},
		{"file_transfer", classHeavy},
		{"file_op", classLight},
		{"git", classLight},
		{"update_key", classLight},
		{"", classLight},
		{"some_future_type_nobody_has_heard_of", classLight},
	}
	for _, tc := range cases {
		if got := classifyCommand(tc.cmdType); got != tc.want {
			t.Errorf("classifyCommand(%q) = %v, want %v", tc.cmdType, got, tc.want)
		}
	}
}

// waitUntilCount polls (test-only; not part of production code) until the
// registry reports the expected count or fails the test after a short
// bound. Used purely to synchronize "the holder goroutine has actually
// acquired its slot" before the test proceeds, since dispatchOne's
// admission happens inside a goroutine the test does not otherwise
// observe completing.
func waitUntilCount(t *testing.T, c *Client, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.inflight.count() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("inflight count never reached %d within 2s (last=%d)", want, c.inflight.count())
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
