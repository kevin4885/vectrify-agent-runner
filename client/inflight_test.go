package client

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a simple mutable time source used to make staleness tests
// deterministic — no time.Sleep, no real elapsed time.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{t: start}
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func newTestRegistry() (*inflightRegistry, *fakeClock) {
	clock := newFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	r := newInflightRegistry()
	r.now = clock.now
	return r, clock
}

// 1. add/remove/count correctness, including remove of an unknown cmd_id.
func TestRegistry_AddRemoveCount(t *testing.T) {
	r, _ := newTestRegistry()

	if got := r.count(); got != 0 {
		t.Fatalf("count on empty registry = %d, want 0", got)
	}

	if dup := r.add("a", "shell"); dup {
		t.Fatalf("add(a) reported duplicate on first insert")
	}
	if dup := r.add("b", "file_op"); dup {
		t.Fatalf("add(b) reported duplicate on first insert")
	}
	if got := r.count(); got != 2 {
		t.Fatalf("count after 2 adds = %d, want 2", got)
	}

	// Removing an unknown cmd_id must be a safe no-op, not a panic or
	// negative count.
	r.remove("does-not-exist")
	if got := r.count(); got != 2 {
		t.Fatalf("count after removing unknown id = %d, want 2 (unchanged)", got)
	}

	r.remove("a")
	if got := r.count(); got != 1 {
		t.Fatalf("count after removing a = %d, want 1", got)
	}

	r.remove("b")
	if got := r.count(); got != 0 {
		t.Fatalf("count after removing b = %d, want 0", got)
	}

	// Removing again after the entry is already gone must still be a no-op.
	r.remove("b")
	if got := r.count(); got != 0 {
		t.Fatalf("count after redundant remove = %d, want 0", got)
	}

	if len(r.entries) != 0 {
		t.Fatalf("registry map not empty after all removes: %v", r.entries)
	}
}

// 2. Concurrent add/remove from many goroutines; run with -race.
func TestRegistry_ConcurrentAddRemove(t *testing.T) {
	r, _ := newTestRegistry()

	const workers = 50
	const opsPerWorker = 200

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				id := fmt.Sprintf("w%d-op%d", worker, i)
				r.add(id, "shell")
				// Interleave a snapshot/count read to exercise the lock from
				// concurrent readers too.
				_ = r.count()
				_ = r.snapshot()
				r.remove(id)
			}
		}(w)
	}
	wg.Wait()

	if got := r.count(); got != 0 {
		t.Fatalf("count after concurrent add/remove = %d, want 0", got)
	}
	if len(r.entries) != 0 {
		t.Fatalf("registry map not empty after concurrent add/remove: %d entries", len(r.entries))
	}
}

// TestRegistry_ConcurrentDuplicatesAndStaleReads exercises the parts the
// basic concurrency test above deliberately doesn't: many goroutines racing
// add/remove on the SAME small set of cmd_ids (so refCount actually goes
// above 1 under contention), plus concurrent checkStale/snapshot/count
// readers hammering the registry throughout. Run with -race; this is the
// test that would catch a missing lock or an unsynchronized refCount
// mutation in add/remove/checkStale.
func TestRegistry_ConcurrentDuplicatesAndStaleReads(t *testing.T) {
	r, _ := newTestRegistry()

	const sharedIDs = 5
	const writers = 40
	const opsPerWriter = 100

	var writerWG sync.WaitGroup
	writerWG.Add(writers)
	for w := 0; w < writers; w++ {
		go func(worker int) {
			defer writerWG.Done()
			for i := 0; i < opsPerWriter; i++ {
				id := fmt.Sprintf("shared-%d", i%sharedIDs)
				r.add(id, "shell")
				r.remove(id)
			}
		}(w)
	}

	// Readers: hammer checkStale/snapshot/count concurrently with the
	// writers above until told to stop. threshold=0 makes every checkStale
	// call pass mark-and-report entries, exercising its mutation path under
	// race alongside add/remove.
	stop := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(3)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				r.checkStale(0)
			}
		}
	}()
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = r.snapshot()
			}
		}
	}()
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = r.count()
			}
		}
	}()

	writerWG.Wait()
	close(stop)
	readerWG.Wait()

	if got := r.count(); got != 0 {
		t.Fatalf("count after concurrent duplicate add/remove + readers = %d, want 0", got)
	}
	if len(r.entries) != 0 {
		t.Fatalf("registry map not empty after concurrent duplicate add/remove + readers: %d entries", len(r.entries))
	}
}

// TestRegistry_DuplicateUnderConcurrency is a simpler, deterministic
// companion to the stress test above: N goroutines add() the SAME cmd_id
// concurrently, each pairs with exactly one remove(), and the final count
// and map state must be exactly zero/empty — this is the concrete assertion
// that duplicate ref-counting is race-free, not just "doesn't panic".
func TestRegistry_DuplicateUnderConcurrency(t *testing.T) {
	r, _ := newTestRegistry()

	const dupWorkers = 30
	var wg sync.WaitGroup
	wg.Add(dupWorkers)
	for i := 0; i < dupWorkers; i++ {
		go func() {
			defer wg.Done()
			r.add("hot-id", "shell")
		}()
	}
	wg.Wait()

	if got := r.count(); got != dupWorkers {
		t.Fatalf("count after %d concurrent duplicate adds = %d, want %d", dupWorkers, got, dupWorkers)
	}
	if len(r.entries) != 1 {
		t.Fatalf("expected exactly 1 map entry for the shared cmd_id, got %d", len(r.entries))
	}

	wg.Add(dupWorkers)
	for i := 0; i < dupWorkers; i++ {
		go func() {
			defer wg.Done()
			r.remove("hot-id")
		}()
	}
	wg.Wait()

	if got := r.count(); got != 0 {
		t.Fatalf("count after %d concurrent removes = %d, want 0", dupWorkers, got)
	}
	if len(r.entries) != 0 {
		t.Fatalf("map entry leaked after matching concurrent removes: %d entries", len(r.entries))
	}
}

// 3. Snapshot ordering is genuinely oldest-first.
func TestRegistry_SnapshotOrderingOldestFirst(t *testing.T) {
	r, clock := newTestRegistry()

	r.add("first", "shell") // oldest
	clock.advance(1 * time.Minute)
	r.add("second", "git")
	clock.advance(1 * time.Minute)
	r.add("third", "file_op") // newest

	snap := r.snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len = %d, want 3", len(snap))
	}

	wantOrder := []string{"first", "second", "third"}
	for i, id := range wantOrder {
		if snap[i].CmdID != id {
			t.Fatalf("snapshot[%d].CmdID = %q, want %q (full snapshot: %+v)", i, snap[i].CmdID, id, snap)
		}
	}
	// Ages must be monotonically non-increasing (oldest-first == largest age first).
	for i := 1; i < len(snap); i++ {
		if snap[i].Age > snap[i-1].Age {
			t.Fatalf("snapshot not sorted oldest-first: entry %d age %v > entry %d age %v", i, snap[i].Age, i-1, snap[i-1].Age)
		}
	}
	if snap[0].Age != 2*time.Minute {
		t.Fatalf("oldest entry age = %v, want 2m", snap[0].Age)
	}
	if snap[2].Age != 0 {
		t.Fatalf("newest entry age = %v, want 0", snap[2].Age)
	}
}

// 4. Staleness detection identifies only entries beyond the threshold, using
// the injected clock so this is deterministic rather than sleep-based.
func TestRegistry_CheckStale(t *testing.T) {
	r, clock := newTestRegistry()

	r.add("old", "shell") // will be 20m old
	clock.advance(10 * time.Minute)
	r.add("medium", "git") // will be 10m old
	clock.advance(10 * time.Minute)
	r.add("fresh", "file_op") // will be 0m old

	threshold := 15 * time.Minute
	stale := r.checkStale(threshold)

	if len(stale) != 1 {
		t.Fatalf("checkStale returned %d entries, want 1 (%+v)", len(stale), stale)
	}
	if stale[0].CmdID != "old" {
		t.Fatalf("checkStale returned %q, want %q", stale[0].CmdID, "old")
	}
	if stale[0].Age != 20*time.Minute {
		t.Fatalf("stale entry age = %v, want 20m", stale[0].Age)
	}

	// Anti-spam: calling again immediately (no time advance, no new
	// staleness) must not re-report "old" since it was already warned.
	stale2 := r.checkStale(threshold)
	if len(stale2) != 0 {
		t.Fatalf("checkStale second call returned %d entries, want 0 (already warned): %+v", len(stale2), stale2)
	}

	// Advance time so "medium" now also crosses the threshold; only the
	// newly-crossed entry should be reported, not "old" again.
	clock.advance(10 * time.Minute) // old=30m, medium=20m, fresh=10m
	stale3 := r.checkStale(threshold)
	if len(stale3) != 1 || stale3[0].CmdID != "medium" {
		t.Fatalf("checkStale third call = %+v, want exactly [medium]", stale3)
	}
}

// 5. Duplicate cmd_id handling behaves as documented and does not corrupt
// count: a duplicate add() increments the ref count and is reported as a
// duplicate; the entry is only fully removed once every add() has a
// matching remove().
func TestRegistry_DuplicateCmdID(t *testing.T) {
	r, clock := newTestRegistry()

	if dup := r.add("dup", "shell"); dup {
		t.Fatalf("first add reported duplicate")
	}
	clock.advance(5 * time.Minute)
	if dup := r.add("dup", "shell"); !dup {
		t.Fatalf("second add with same cmd_id did not report duplicate")
	}

	if got := r.count(); got != 2 {
		t.Fatalf("count after duplicate add = %d, want 2", got)
	}
	if len(r.entries) != 1 {
		t.Fatalf("registry should have exactly 1 map entry for a duplicate cmd_id, got %d", len(r.entries))
	}

	// Start time must be pinned to the *first* add, not reset by the
	// duplicate — otherwise a wedged original command's age would be
	// under-reported every time a duplicate arrived.
	snap := r.snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	if snap[0].Age != 5*time.Minute {
		t.Fatalf("snapshot age = %v, want 5m (from first add)", snap[0].Age)
	}

	// First remove (pairs with the second add) must not delete the entry.
	r.remove("dup")
	if got := r.count(); got != 1 {
		t.Fatalf("count after first remove of duplicate = %d, want 1", got)
	}
	if len(r.entries) != 1 {
		t.Fatalf("entry removed too early after only one of two removes")
	}

	// Second remove (pairs with the first add) must fully clear it.
	r.remove("dup")
	if got := r.count(); got != 0 {
		t.Fatalf("count after second remove of duplicate = %d, want 0", got)
	}
	if len(r.entries) != 0 {
		t.Fatalf("entry not removed after matching remove count")
	}
}

// 1. Per-class counting is correct: countsLocked derives (global, heavy)
// purely from registry contents, and stays correct under concurrent
// acquire()/remove() across both classes — this is the "single source of
// truth, no separate drifting counter" property extended to per-class
// counts.
func TestRegistry_PerClassCounting(t *testing.T) {
	r, _ := newTestRegistry()
	ctx := context.Background()

	if out := r.acquire(ctx, "h1", "shell", 10, 5); !out.OK {
		t.Fatalf("acquire(h1) = %+v, want OK", out)
	}
	if out := r.acquire(ctx, "h2", "file_transfer", 10, 5); !out.OK {
		t.Fatalf("acquire(h2) = %+v, want OK", out)
	}
	if out := r.acquire(ctx, "l1", "file_op", 10, 5); !out.OK {
		t.Fatalf("acquire(l1) = %+v, want OK", out)
	}

	r.mu.Lock()
	global, heavy := r.countsLocked()
	r.mu.Unlock()
	if global != 3 {
		t.Fatalf("global count = %d, want 3", global)
	}
	if heavy != 2 {
		t.Fatalf("heavy count = %d, want 2", heavy)
	}

	r.remove("h1")
	r.mu.Lock()
	global, heavy = r.countsLocked()
	r.mu.Unlock()
	if global != 2 || heavy != 1 {
		t.Fatalf("after removing one heavy entry: global=%d heavy=%d, want global=2 heavy=1", global, heavy)
	}

	r.remove("h2")
	r.remove("l1")
	r.mu.Lock()
	global, heavy = r.countsLocked()
	r.mu.Unlock()
	if global != 0 || heavy != 0 {
		t.Fatalf("after removing everything: global=%d heavy=%d, want 0/0", global, heavy)
	}
}

// Per-class counting under genuine concurrent mutation: many goroutines
// acquire/remove a mix of heavy and light commands simultaneously; the
// derived (global, heavy) counts must never be observed inconsistent with
// registry contents, and must land at exactly 0/0 once every goroutine has
// finished.
func TestRegistry_PerClassCounting_ConcurrentMutation(t *testing.T) {
	r, _ := newTestRegistry()
	ctx := context.Background()

	const workers = 40
	const opsPerWorker = 50

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				id := fmt.Sprintf("w%d-op%d", worker, i)
				cmdType := "file_op"
				if i%2 == 0 {
					cmdType = "shell"
				}
				// Generous limits: this test is about counting correctness
				// under concurrency, not admission-control blocking.
				out := r.acquire(ctx, id, cmdType, workers*opsPerWorker, workers*opsPerWorker)
				if !out.OK {
					t.Errorf("acquire(%s) unexpectedly blocked/rejected: %+v", id, out)
					return
				}
				// Read counts mid-flight from another goroutine's perspective
				// to exercise the lock under contention; no assertion on the
				// exact value here (it's a moving target), just that it
				// doesn't panic or corrupt state.
				r.mu.Lock()
				_, _ = r.countsLocked()
				r.mu.Unlock()
				r.remove(id)
			}
		}(w)
	}
	wg.Wait()

	r.mu.Lock()
	global, heavy := r.countsLocked()
	r.mu.Unlock()
	if global != 0 || heavy != 0 {
		t.Fatalf("after all concurrent acquire/remove pairs completed: global=%d heavy=%d, want 0/0", global, heavy)
	}
	if len(r.entries) != 0 {
		t.Fatalf("registry map not empty after concurrent per-class mutation: %d entries", len(r.entries))
	}
}

// acquire()'s own duplicate-cmd_id path (distinct from add()'s, which is
// tested separately): a second acquire() for a cmd_id already reserved by
// a still-in-flight dispatch is subjected to EXACTLY the same capacity
// check as any other command — the duplicate is a real additional unit of
// tracked capacity (refCount++), not a free ride on the original's slot —
// but if capacity IS available, joins the existing entry (refCount++)
// rather than creating a second map entry.
func TestRegistry_Acquire_DuplicateCmdID_SubjectToCapacityCheck(t *testing.T) {
	r, _ := newTestRegistry()
	ctx := context.Background()

	// maxGlobal=2 gives room for the original PLUS one duplicate.
	if out := r.acquire(ctx, "dup", "shell", 2, 2); !out.OK {
		t.Fatalf("first acquire = %+v, want OK", out)
	}

	out := r.acquire(ctx, "dup", "shell", 2, 2)
	if !out.OK || !out.Duplicate {
		t.Fatalf("second acquire(dup) with capacity available = %+v, want OK=true Duplicate=true", out)
	}
	if got := r.count(); got != 2 {
		t.Fatalf("count after duplicate acquire = %d, want 2 (duplicate consumes a real capacity unit)", got)
	}
	if len(r.entries) != 1 {
		t.Fatalf("registry should have exactly 1 map entry for a duplicate cmd_id, got %d", len(r.entries))
	}

	r.remove("dup")
	r.remove("dup")
	if got := r.count(); got != 0 {
		t.Fatalf("count after both removes = %d, want 0", got)
	}
}

// A duplicate must be REJECTED (not admitted for free) when the registry is
// already fully saturated — including when the original entry's own
// refCount is exactly what fills the limit. This is the anti-replay
// guarantee: a buggy or replaying API resending the same cmd_id must not be
// able to bypass MaxConcurrency by piggybacking on an already-admitted
// command.
func TestRegistry_Acquire_DuplicateCmdID_RejectedWhenSaturated(t *testing.T) {
	r, _ := newTestRegistry()

	// maxGlobal=1: the single original entry already fills the only slot.
	if out := r.acquire(context.Background(), "dup", "shell", 1, 1); !out.OK {
		t.Fatalf("first acquire = %+v, want OK", out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	out := r.acquire(ctx, "dup", "shell", 1, 1)
	if out.OK {
		t.Fatalf("duplicate acquire while saturated = %+v, want rejected", out)
	}
	if got := r.count(); got != 1 {
		t.Fatalf("count after rejected duplicate = %d, want 1 (unchanged)", got)
	}

	r.remove("dup")
	if got := r.count(); got != 0 {
		t.Fatalf("count after removing the original = %d, want 0", got)
	}
}

func TestSummarizeOldest(t *testing.T) {
	entries := []inflightSnapshot{
		{CmdID: "a", CmdType: "shell", Age: 90 * time.Second},
		{CmdID: "b", CmdType: "git", Age: 30 * time.Second},
	}

	got := summarizeOldest(entries, 5) // n > len(entries): must not panic or over-read
	want := "a(shell,1m30s), b(git,30s)"
	if got != want {
		t.Fatalf("summarizeOldest = %q, want %q", got, want)
	}

	got2 := summarizeOldest(entries, 1)
	if got2 != "a(shell,1m30s)" {
		t.Fatalf("summarizeOldest with n=1 = %q, want %q", got2, "a(shell,1m30s)")
	}

	if got3 := summarizeOldest(nil, 5); got3 != "" {
		t.Fatalf("summarizeOldest(nil) = %q, want empty string", got3)
	}

	if got4 := summarizeOldest(entries, -1); got4 != "" {
		t.Fatalf("summarizeOldest with negative n = %q, want empty string (clamped, no panic)", got4)
	}
}

func TestTruncateForLog(t *testing.T) {
	short := "abc123"
	if got := truncateForLog(short); got != short {
		t.Fatalf("truncateForLog(short) = %q, want unchanged %q", got, short)
	}

	long := strings.Repeat("x", maxLogFieldLen+10)
	got := truncateForLog(long)
	// runeLen check via len on ASCII input is fine here; the important
	// invariant is that it was actually shortened and marked as such.
	if len(got) >= len(long) {
		t.Fatalf("truncateForLog did not shorten an oversized value")
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncateForLog(long) = %q, want a truncation marker suffix", got)
	}
}

