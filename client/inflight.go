package client

import (
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
	start       time.Time // time of the *first* dispatch for this cmd_id
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
// running right now" and "which ones" — it replaces the old bare
// atomic.Int64 counter, which could only ever answer the first question.
// count() is derived from the registry contents rather than tracked
// separately, so the two can never drift out of sync.
type inflightRegistry struct {
	mu      sync.Mutex
	entries map[string]*inflightEntry

	// now supplies the current time. Overridable in tests so staleness
	// checks are deterministic instead of relying on time.Sleep.
	now func() time.Time
}

func newInflightRegistry() *inflightRegistry {
	return &inflightRegistry{
		entries: make(map[string]*inflightEntry),
		now:     time.Now,
	}
}

// add registers a dispatch for cmdID/cmdType and returns true if this is a
// duplicate — i.e. cmdID was already in-flight. Every add() call, duplicate
// or not, must be paired with exactly one remove(cmdID) call (typically via
// an unconditional defer at the call site) to keep refCount balanced.
//
// A duplicate add() keeps the *original* entry's cmdType, start time, and
// warnedStale flag — it does not create a second entry or reset the clock.
// This is deliberate: the entry's age must reflect how long the original
// (still-running) dispatch has been in flight, which is exactly the signal
// this registry exists to preserve. The practical effect is that once a
// wedged original has been warned as stale, an incoming duplicate for the
// same id will not trigger a second stale warning of its own — it shares
// the original's warned state, consistent with "one WARN per stuck cmd_id".
func (r *inflightRegistry) add(cmdID, cmdType string) (duplicate bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, exists := r.entries[cmdID]; exists {
		e.refCount++
		return true
	}
	r.entries[cmdID] = &inflightEntry{cmdType: cmdType, start: r.now(), refCount: 1}
	return false
}

// remove decrements the ref count for cmdID and deletes the entry once it
// reaches zero. It is a deliberate no-op if cmdID is not tracked (already
// removed, or never added) so that defer-based cleanup can never panic or
// under/over-count regardless of which code path reached it.
func (r *inflightRegistry) remove(cmdID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, exists := r.entries[cmdID]
	if !exists {
		return
	}
	e.refCount--
	if e.refCount <= 0 {
		delete(r.entries, cmdID)
	}
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
