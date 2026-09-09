package client

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// inflightEntry describes one distinct cmd_id currently being dispatched.
//
// refCount exists solely to handle the (anomalous) case of the API sending a
// duplicate cmd_id while the first dispatch for that id is still running: two
// dispatch goroutines end up paired with the same map key, so a plain
// "present/absent" entry cannot be removed twice without either leaking one
// decrement or double-deleting the entry out from under the still-running
// original. Tracking a ref count per key makes add/remove for the same
// cmd_id commutative and order-independent, which is what makes it safe to
// pair every add() with an unconditional defer remove() regardless of
// duplicates.
type inflightEntry struct {
	cmdType     string
	class       commandClass // derived once from cmdType at insertion time (see classifyCommand)
	start       time.Time    // time of the *first* dispatch for this cmd_id
	refCount    int
	warnedStale bool // see inflightRegistry.checkStale
}

// inflightSnapshot is a read-only, point-in-time view of one in-flight
// command, safe to log or pass around outside the registry's lock.
type inflightSnapshot struct {
	CmdID   string
	CmdType string
	Age     time.Duration
}

// inflightRegistry is a concurrency-safe registry of currently-dispatching
// commands. It is the single source of truth for "how many commands are
// running right now" (in total, and per commandClass) and "which ones" — it
// replaces the old bare atomic.Int64 counter, which could only ever answer
// the first question for the total. count(), countsLocked(), and acquire()
// all derive their numbers from r.entries rather than tracking a second,
// separately-maintained counter, so admission control and the numbers
// reported in logs/Drain/staleness warnings can never drift apart.
//
// acquire() is the capacity-gated entry point used by the dispatch hand-off
// path (client/ws_client.go): it waits, bounded by a caller-supplied
// context, for a slot to free rather than either rejecting instantly or
// polling in a sleep loop. Waiting is implemented with a channel that is
// closed and replaced every time remove() frees up any capacity (see
// waitCh below) — every current waiter wakes on a select, rechecks capacity
// under the lock, and either wins the slot or loses the race and waits
// again. This means waiters are woken in a broadcast, NOT strict FIFO order
// — whichever goroutine's recheck happens to run first under the mutex
// wins, regardless of how long it has been waiting. That is an accepted
// trade-off for this use case (bounded 2-3s waits absorbing normal command
// bursts, not a fairness-critical scheduler) in exchange for a much simpler
// implementation than a proper FIFO wait queue.
type inflightRegistry struct {
	mu      sync.Mutex
	entries map[string]*inflightEntry

	// waitCh is closed and replaced (never just closed-and-left) every time
	// remove() frees capacity, so every acquire() call currently blocked in
	// `select { case <-wake: ... }` observes the close and re-checks
	// capacity under the lock. Replacing (not reusing) the channel means a
	// waiter that already captured the old value under the lock cannot miss
	// a subsequent close: it either already grabbed the freed slot when it
	// last held the lock, or it is holding a reference to the exact channel
	// that will be closed on the next remove().
	waitCh chan struct{}

	// now supplies the current time. Overridable in tests so staleness
	// checks are deterministic instead of relying on time.Sleep.
	now func() time.Time
}

func newInflightRegistry() *inflightRegistry {
	return &inflightRegistry{
		entries: make(map[string]*inflightEntry),
		waitCh:  make(chan struct{}),
		now:     time.Now,
	}
}

// countsLocked returns (global, heavy) active counts derived from the
// current registry contents. Callers must hold r.mu.
func (r *inflightRegistry) countsLocked() (global, heavy int) {
	for _, e := range r.entries {
		global += e.refCount
		if e.class == classHeavy {
			heavy += e.refCount
		}
	}
	return global, heavy
}

// acquireOutcome is the result of an acquire() call. blockedBy, global, and
// heavy are only meaningful when OK is false — they capture which
// constraint was binding (and the counts at that moment) purely for the
// caller's rejection log/error message; the registry itself has already
// made its decision by the time this is returned.
type acquireOutcome struct {
	OK        bool
	Duplicate bool
	BlockedBy string // "global" | "heavy" — set only when OK is false
	Global    int
	Heavy     int
}

// acquire reserves one dispatch slot for cmdID/cmdType, waiting (bounded by
// ctx) for a slot to free if none is available right now. It never polls:
// the wait is a select on ctx.Done() and the shared waitCh broadcast
// described on inflightRegistry. Every call that returns OK == true —
// including duplicates — reserves exactly one unit of capacity that MUST be
// released with exactly one matching remove(cmdID) call, typically via an
// unconditional defer at the call site (same contract the old add()/remove()
// pair had).
//
// class is derived from cmdType via classifyCommand internally (not passed
// in as a separate parameter) so the two can never disagree — a caller
// cannot accidentally pass a cmdType/class pair where the class doesn't
// actually correspond to classifyCommand(cmdType).
//
// Duplicate cmd_id handling: a duplicate is subjected to EXACTLY the same
// capacity check as a brand new command — it is NOT admitted
// unconditionally regardless of capacity. This mirrors the pre-acquire()
// dispatch path (client/ws_client.go, prior commit), where the capacity
// check ran before add() was ever called, so a duplicate arriving while the
// registry was already full was rejected exactly like any other command.
// It matters because a duplicate still increments refCount, which
// countsLocked() includes in the global/heavy totals — i.e. a duplicate
// really does consume one additional unit of tracked capacity, not a free
// ride on the original's slot. Treating duplicates as capacity-exempt would
// let a buggy or replaying API resend the same cmd_id N times while the
// original is still running and get N executions admitted regardless of
// MaxConcurrency — precisely the unbounded-growth failure this mechanism
// exists to prevent. The only special-casing is that once capacity IS
// available, a duplicate joins the existing entry (increment refCount)
// instead of creating a second map entry, per inflightEntry's doc comment.
func (r *inflightRegistry) acquire(ctx context.Context, cmdID, cmdType string, maxGlobal, maxHeavy int) acquireOutcome {
	for {
		r.mu.Lock()

		// effectiveClass is the class the capacity check below is measured
		// against. For a duplicate, this MUST be the original entry's own
		// class (e.class), NOT classifyCommand(cmdType) of the incoming
		// duplicate — a cmd_id reused with a different type (however
		// anomalous) must still be accounted against whichever class it is
		// actually going to occupy (the existing entry it joins), or the
		// heavy/light counts silently drift from reality: a "shell"
		// duplicate of an existing light entry would run heavy work without
		// ever consuming a heavy slot, and a light duplicate of a heavy
		// entry would inflate the heavy count past maxHeavy.
		existing, isDuplicate := r.entries[cmdID]
		effectiveClass := classifyCommand(cmdType)
		if isDuplicate {
			effectiveClass = existing.class
		}

		global, heavy := r.countsLocked()
		globalFull := global >= maxGlobal
		heavyFull := effectiveClass == classHeavy && heavy >= maxHeavy
		if !globalFull && !heavyFull {
			if isDuplicate {
				existing.refCount++
				r.mu.Unlock()
				return acquireOutcome{OK: true, Duplicate: true}
			}
			r.entries[cmdID] = &inflightEntry{cmdType: cmdType, class: effectiveClass, start: r.now(), refCount: 1}
			r.mu.Unlock()
			return acquireOutcome{OK: true}
		}

		wake := r.waitCh
		r.mu.Unlock()

		select {
		case <-wake:
			continue // capacity may have freed; loop back and recheck under the lock
		case <-ctx.Done():
			// blockedBy reflects the constraint that was binding on THIS
			// (the last) check, captured above before releasing the lock —
			// not recomputed from fresh counts after ctx.Done() fires, which
			// could report "global" for a command that was actually blocked
			// on the heavy sub-limit the whole time if global count happened
			// to dip in the interim.
			blocked := "global"
			if heavyFull {
				blocked = "heavy"
			}
			return acquireOutcome{OK: false, BlockedBy: blocked, Global: global, Heavy: heavy}
		}
	}
}

// remove decrements the ref count for cmdID and deletes the entry once it
// reaches zero. It is a deliberate no-op if cmdID is not tracked (already
// removed, or never added) so that defer-based cleanup can never panic or
// under/over-count regardless of which code path reached it.
//
// Every successful decrement (i.e. cmdID was tracked) wakes every goroutine
// currently blocked in acquire()'s select, because a decrement — whether or
// not it deletes the entry outright — always reduces the total reserved
// capacity by one unit, and that unit might be exactly what an acquire()
// waiter needs.
func (r *inflightRegistry) remove(cmdID string) {
	r.mu.Lock()
	e, exists := r.entries[cmdID]
	if !exists {
		r.mu.Unlock()
		return
	}
	e.refCount--
	if e.refCount <= 0 {
		delete(r.entries, cmdID)
	}
	oldWake := r.waitCh
	r.waitCh = make(chan struct{})
	r.mu.Unlock()
	close(oldWake)
}

// count returns the total number of in-flight dispatches (counting
// duplicates), matching what the old activeCommands atomic tracked.
func (r *inflightRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.entries {
		n += e.refCount
	}
	return n
}

// snapshot returns one entry per distinct cmd_id, sorted oldest-first (i.e.
// largest age first), so callers can cheaply take "the N oldest" for a log
// summary.
func (r *inflightRegistry) snapshot() []inflightSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

// snapshotLocked builds the sorted snapshot; callers must hold r.mu.
func (r *inflightRegistry) snapshotLocked() []inflightSnapshot {
	now := r.now()
	out := make([]inflightSnapshot, 0, len(r.entries))
	for id, e := range r.entries {
		out = append(out, inflightSnapshot{CmdID: id, CmdType: e.cmdType, Age: now.Sub(e.start)})
	}
	sortSnapshotsOldestFirst(out)
	return out
}

// checkStale returns every in-flight command older than threshold that has
// not already been reported stale, and marks those entries as warned so
// subsequent calls do not re-report them. This is the entire anti-spam
// mechanism for the staleness monitor: each stuck command produces exactly
// one WARN for the remainder of its (stuck) lifetime, no matter how many
// monitor ticks pass while it stays wedged, instead of one WARN per tick.
func (r *inflightRegistry) checkStale(threshold time.Duration) []inflightSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	var out []inflightSnapshot
	for id, e := range r.entries {
		if e.warnedStale {
			continue
		}
		age := now.Sub(e.start)
		if age >= threshold {
			e.warnedStale = true
			out = append(out, inflightSnapshot{CmdID: id, CmdType: e.cmdType, Age: age})
		}
	}
	sortSnapshotsOldestFirst(out)
	return out
}

// sortSnapshotsOldestFirst sorts by descending age (oldest first) with
// cmd_id as a tiebreak. Map iteration order is random and multiple entries
// can share the same rounded age, so without a tiebreak the "top N oldest"
// log summary would vary nondeterministically between calls for no reason —
// a minor but needless source of log noise when comparing two rejection
// lines by eye.
func sortSnapshotsOldestFirst(s []inflightSnapshot) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Age != s[j].Age {
			return s[i].Age > s[j].Age
		}
		return s[i].CmdID < s[j].CmdID
	})
}

// summarizeOldest formats up to n entries (assumed already sorted
// oldest-first) into a single comma-separated string such as
// "ab12(shell,4m32s), cd34(git,1m03s)" for one log field.
//
// Returned as a single string rather than a []string: slog's text/JSON
// handlers escape/quote plain string values, but a []string is rendered via
// the generic Any/reflect path with no escaping. cmd_id and type originate
// from the (untrusted) server payload, so without escaping a value
// containing e.g. a newline could forge extra log lines. Each component is
// also length-capped so one oversized field cannot bloat every rejection
// log line.
func summarizeOldest(entries []inflightSnapshot, n int) string {
	if n < 0 {
		n = 0
	}
	if n > len(entries) {
		n = len(entries)
	}
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		e := entries[i]
		parts = append(parts,
			fmt.Sprintf("%s(%s,%s)", truncateForLog(e.CmdID), truncateForLog(e.CmdType), e.Age.Round(time.Second)),
		)
	}
	return strings.Join(parts, ", ")
}

// maxLogFieldLen caps individual untrusted values (cmd_id, type) embedded in
// the oldest-in-flight log summary so a single oversized or adversarial
// value cannot bloat the log line.
const maxLogFieldLen = 64

// truncateForLog clamps s to maxLogFieldLen, appending "…" when truncated,
// so callers get an obvious visual cue rather than a silently cut value.
func truncateForLog(s string) string {
	if len(s) <= maxLogFieldLen {
		return s
	}
	return s[:maxLogFieldLen] + "…"
}
