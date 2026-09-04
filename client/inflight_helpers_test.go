package client

// This file contains test-only helpers for inflightRegistry. It has no
// production callers — client/ws_client.go's dispatch hand-off path uses
// acquire() exclusively, which is capacity-gated. add() predates acquire()
// (it was the original, uncapped registration primitive from the previous
// phase) and is kept only because a large slice of pre-existing tests in
// inflight_test.go exercise registry bookkeeping (snapshot ordering,
// staleness, ref-counting) directly through it without needing to reason
// about capacity limits at all. Isolating it in a _test.go file (rather
// than shipping it in inflight.go) makes it impossible for a future
// production call site to accidentally bypass the bounded-wait admission
// path acquire() exists to enforce.

// add registers a dispatch for cmdID/cmdType and returns true if this is a
// duplicate — i.e. cmdID was already in-flight. Every add() call, duplicate
// or not, must be paired with exactly one remove(cmdID) call (typically via
// an unconditional defer at the call site) to keep refCount balanced.
//
// add() performs no capacity check — unlike acquire(), it always succeeds.
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
	r.entries[cmdID] = &inflightEntry{cmdType: cmdType, class: classifyCommand(cmdType), start: r.now(), refCount: 1}
	return false
}
