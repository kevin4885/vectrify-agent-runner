// Package client manages the persistent WebSocket connection to the Vectrify API.
// It handles authentication, the registration handshake, reconnection with
// exponential backoff, and the bidirectional message loop.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"vectrify/agent-runner/config"
	"vectrify/agent-runner/protocol"
	"vectrify/agent-runner/runner"
)

const (
	// pingInterval is how often the runner sends a WebSocket ping to the server.
	// Keeps the connection alive through NAT gateways and load-balancer idle timeouts.
	pingInterval = 30 * time.Second

	// readDeadline is the maximum time to wait for any inbound message (including
	// pong replies).  Must be greater than pingInterval to allow time for the
	// round-trip.
	readDeadline = 90 * time.Second

	// writeDeadline is the maximum time allowed for a single WebSocket write.
	// Without this, a half-open TCP connection (reads fail but the kernel send
	// buffer still accepts bytes) causes WriteJSON/WriteMessage to block forever,
	// which hangs <-writerDone in connect() and prevents RunForever from retrying.
	writeDeadline = 10 * time.Second

	// healthyDuration is the minimum uptime for a connection to be considered
	// healthy.  If connect() ran at least this long before returning, the attempt
	// counter is reset so the next reconnect is fast regardless of how many prior
	// failures occurred.
	healthyDuration = 30 * time.Second

	// staleCommandThreshold is how long a command may stay in-flight before the
	// staleness monitor warns about it. inflightEntry.start is set at
	// successful acquire() (i.e. execution start), not at receipt from the
	// WebSocket — time spent waiting in acquire() for a free slot does NOT
	// count toward this age. So the invariant this must preserve is simply:
	// stay comfortably above maxShellTimeout (runner/runner.go, 600s = 10m),
	// the longest any single command's own execution is allowed to run. 20
	// minutes keeps a wide margin over that 10m execution ceiling, so a
	// healthy long-running shell command never trips this — it exists to
	// catch commands wedged past their own timeout (e.g. the
	// descendant-process pipe-inheritance hang this dispatch layer was
	// previously vulnerable to), not to police normal durations. If
	// maxShellTimeout is ever raised, raise this too so the margin holds.
	staleCommandThreshold = 20 * time.Minute

	// staleCheckInterval is how often the staleness monitor scans the registry.
	staleCheckInterval = 30 * time.Second

	// rejectionLogTopN is how many of the oldest in-flight commands to include
	// in the concurrency-limit rejection log line.
	rejectionLogTopN = 5

	// maxPendingSlotWaiters bounds how many dispatch hand-off goroutines
	// (see recv loop below) may be simultaneously blocked inside
	// inflightRegistry.acquire() waiting for a free slot. Without this cap,
	// a sustained burst of incoming commands beyond capacity would grow one
	// goroutine per command for up to SlotAcquireTimeoutSeconds each, which
	// is itself an unbounded-growth risk during a bad burst — exactly the
	// class of problem this whole change is trying to eliminate. Once the
	// cap is hit, new commands are rejected immediately (no wait at all)
	// rather than queuing a waiter that has to be tracked.
	maxPendingSlotWaiters = 64
)

// Client manages one persistent WebSocket connection to the Vectrify API.
type Client struct {
	cfg      *config.Config
	runner   *runner.Runner
	log      *slog.Logger
	inflight *inflightRegistry // single source of truth for active dispatch count + detail

	// dispatchFunc is the function dispatchOne calls once a slot has been
	// acquired. Defaults to c.runner.Dispatch (set in New()); tests override
	// it to inject panics/delays without needing a live Runner, so the
	// admission-control logic in dispatchOne can be exercised in isolation
	// from the executor package.
	dispatchFunc func(raw protocol.RawCommand, send func(interface{}), triggerReconnect func())

	// acquireTimeout returns the bounded-wait duration for one dispatch's
	// slot acquisition. Defaults (in New()) to
	// time.Duration(cfg.SlotAcquireTimeoutSeconds) * time.Second, i.e. the
	// configured whole-second value. Pulled out as a func field (rather than
	// reading cfg directly in dispatchOne) purely so tests can inject a
	// sub-second timeout — SlotAcquireTimeoutSeconds is deliberately
	// whole-second-only in the YAML config (this is an operator-facing
	// tuning knob, not something that needs sub-second precision), but a
	// test asserting "the reject path actually waited out the timeout"
	// would otherwise cost a full real second per case.
	acquireTimeout func() time.Duration

	// pendingWaiters counts hand-off goroutines currently blocked in
	// inflightRegistry.acquire() (see recv loop in connect()), bounded by
	// maxPendingSlotWaiters. Plain int guarded by pendingMu rather than an
	// atomic: it is always read-then-conditionally-incremented as one
	// step, which a bare atomic.Int64 cannot express without a CAS loop —
	// a mutex is simpler and this is not a hot path (one lock/unlock per
	// inbound command, not per byte).
	pendingMu      sync.Mutex
	pendingWaiters int
}

// New creates a Client and starts its background staleness monitor.
// The monitor runs for the lifetime of the process (New is called exactly
// once from main.go) and is intentionally not tied to any one WebSocket
// connection: in-flight dispatch goroutines (and the registry entries they
// hold) survive reconnects, so a stale-command warning must too.
func New(cfg *config.Config, r *runner.Runner, log *slog.Logger) *Client {
	c := &Client{cfg: cfg, runner: r, log: log, inflight: newInflightRegistry()}
	c.dispatchFunc = r.Dispatch
	c.acquireTimeout = func() time.Duration {
		return time.Duration(c.cfg.SlotAcquireTimeoutSeconds) * time.Second
	}
	go c.monitorStale()
	return c
}

// monitorStale periodically scans the in-flight registry for commands that
// have been running longer than staleCommandThreshold and warns about them.
// This is the early-warning signal for a wedged dispatch goroutine: it fires
// long before enough slots are wedged to start rejecting new commands.
//
// Anti-spam: inflightRegistry.checkStale marks each entry as "warned" the
// first time it crosses the threshold, so a command stuck for hours produces
// exactly one WARN, not one every 30s. Runs for the life of the process —
// there is nothing to stop, so no lifecycle management is needed beyond the
// one goroutine started in New().
func (c *Client) monitorStale() {
	ticker := time.NewTicker(staleCheckInterval)
	defer ticker.Stop()
	for range ticker.C {
		stale := c.inflight.checkStale(staleCommandThreshold)
		for _, s := range stale {
			c.log.Warn("dispatch: command has been in-flight longer than expected, possible wedged slot",
				"cmd_id", s.CmdID,
				"type", s.CmdType,
				"age", s.Age.Round(time.Second).String(),
				"threshold", staleCommandThreshold.String(),
			)
		}
	}
}

// Drain blocks until all in-flight dispatch goroutines have finished or
// timeout elapses.  Called by the auto-updater before os.Exit so that
// commands already dispatched can complete and send their results back.
//
// Must check pendingWaiterCount() in addition to inflight.count(): a
// command that is still waiting in acquire() for a free slot (see
// dispatchOne) has not yet been added to the registry, so count() alone
// could report 0 while a waiter is about to win a slot and start a brand
// new command — which os.Exit (called right after Drain returns) would
// then kill mid-flight with no result ever sent back, the exact loss Drain
// exists to prevent.
func (c *Client) Drain(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.inflight.count() == 0 && c.pendingWaiterCount() == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	oldest := c.inflight.snapshot()
	c.log.Warn("drain timeout: exiting with in-flight commands",
		"active", c.inflight.count(),
		"pending_waiters", c.pendingWaiterCount(),
		"oldest", summarizeOldest(oldest, rejectionLogTopN),
	)
}

// RunForever connects and reconnects indefinitely until the process is stopped.
// It uses exponential backoff capped at cfg.ReconnectMaxBackoff seconds.
// The attempt counter resets whenever a connection was healthy for at least
// healthyDuration, so a brief blip after hours of uptime reconnects quickly.
// Any unexpected panic inside connect() is caught here so the loop always
// continues rather than crashing the process.
func (c *Client) RunForever() {
	attempt := 0
	for {
		c.log.Info("connecting", "url", c.cfg.APIURL, "attempt", attempt+1)
		start := time.Now()
		err := c.safeConnect()
		uptime := time.Since(start)

		if err != nil {
			// Reset backoff if the connection was healthy before it dropped so a
			// brief network hiccup after a long-running session reconnects quickly.
			if uptime >= healthyDuration {
				attempt = 0
			} else {
				attempt++
			}
			wait := backoff(attempt, c.cfg.ReconnectMaxBackoff)
			c.log.Warn("connection lost", "err", err, "uptime", uptime.Round(time.Second), "retry_in", wait)
			time.Sleep(wait)
		} else {
			// Clean disconnect (should not happen in normal operation).
			attempt = 0
			time.Sleep(2 * time.Second)
		}
	}
}

// safeConnect wraps connect() with a panic recovery so an unexpected panic
// inside the connection loop (e.g. nil pointer, bad server response) is
// treated as a retriable error rather than crashing the process.
func (c *Client) safeConnect() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic in connect: %v\n%s", p, string(debug.Stack()))
			c.log.Error("connect panic recovered", "panic", p)
		}
	}()
	return c.connect()
}

// connect establishes the WebSocket, completes the registration handshake,
// and runs the message loop until the connection closes.
func (c *Client) connect() error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
	}
	url := c.cfg.APIURL + "?key=" + c.cfg.RunnerKey

	conn, _, err := dialer.Dial(url, http.Header{})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// ── Registration handshake ────────────────────────────────────────────────
	reg := protocol.RegisterMsg{
		Type:          "register",
		Platform:      config.Platform(),
		WorkspaceRoot: c.cfg.WorkspaceRoot,
		AllowShell:    c.cfg.AllowShell,
		Version:       config.Version,
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeDeadline))
	if err := conn.WriteJSON(reg); err != nil {
		return fmt.Errorf("sending register: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{}) // clear; writer goroutine sets its own per-write

	// Read ack
	_, ackBytes, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("reading registered ack: %w", err)
	}
	var ack map[string]interface{}
	if err := json.Unmarshal(ackBytes, &ack); err != nil {
		return fmt.Errorf("decoding ack: %w", err)
	}
	if ack["type"] == "error" {
		return fmt.Errorf("server rejected registration: %v", ack["message"])
	}
	if ack["type"] != "registered" {
		return fmt.Errorf("unexpected ack type: %v", ack["type"])
	}

	c.log.Info("registered", "runner_id", ack["runner_id"])

	// ── Keepalive: pong handler resets the read deadline ─────────────────────
	// The writer goroutine sends a ping every pingInterval.  The server must
	// reply with a pong (or send any message) within readDeadline.  If it does
	// not, ReadMessage returns a timeout error and we reconnect cleanly instead
	// of hanging on a silently dead connection.
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(readDeadline))
	})
	if err := conn.SetReadDeadline(time.Now().Add(readDeadline)); err != nil {
		return fmt.Errorf("setting initial read deadline: %w", err)
	}

	// ── Dedicated writer goroutine ─────────────────────────────────────────────
	// gorilla/websocket requires all writes to be serialized.  A single writer
	// goroutine owns conn.WriteJSON / conn.WriteMessage; everything else enqueues
	// onto writeCh.  The same goroutine fires periodic pings via a ticker so the
	// ping and regular writes are never concurrent.
	// writeCh is intentionally NEVER closed — closing it would cause a panic if
	// a long-running dispatch goroutine calls send() after the connection dies.
	// Instead, stopWriter is closed to signal the writer to exit cleanly.
	// SetWriteDeadline is set before every write so a half-open TCP connection
	// (reads fail but kernel send buffer still accepts bytes) cannot block the
	// writer indefinitely, which would hang <-writerDone and freeze RunForever.
	writeCh := make(chan interface{}, 64)
	stopWriter := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer func() {
			if p := recover(); p != nil {
				c.log.Error("writer panic recovered",
					"panic", p,
					"stack", string(debug.Stack()),
				)
			}
		}()
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopWriter:
				return
			case msg := <-writeCh:
				_ = conn.SetWriteDeadline(time.Now().Add(writeDeadline))
				if err := conn.WriteJSON(msg); err != nil {
					c.log.Warn("writer: send error", "err", err)
					return
				}
			case <-ticker.C:
				_ = conn.SetWriteDeadline(time.Now().Add(writeDeadline))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					c.log.Warn("writer: ping error", "err", err)
					return
				}
			}
		}
	}()

	// send enqueues a message for the writer. Safe to call from any goroutine
	// at any time — writeCh is never closed so this never panics.
	send := func(msg interface{}) {
		select {
		case writeCh <- msg:
		case <-writerDone:
			// Writer already exited (write error or connection closed); drop the message.
		}
	}

	// ── Message loop ──────────────────────────────────────────────────────────
	var readErr error
	for {
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			readErr = fmt.Errorf("read: %w", err)
			break
		}
		// Reset the read deadline on every inbound message, not just pongs,
		// so an active command stream also keeps the deadline rolling.
		_ = conn.SetReadDeadline(time.Now().Add(readDeadline))

		raw, err := runner.DecodeRaw(msgBytes)
		if err != nil {
			c.log.Warn("decode error", "err", err)
			continue
		}

		// Dispatch each command via a bounded hand-off goroutine so the recv
		// loop is NEVER blocked waiting for a free slot: blocking here would
		// stall pings and all other inbound traffic (including other
		// commands' results streaming back), which is worse than the
		// rejection this replaces. See tryReserveWaiter for the bound on how
		// many of these hand-off goroutines may exist at once, and
		// inflightRegistry.acquire for the bounded wait itself.
		cmdID := raw.CmdID()
		cmdType := raw.Type()
		class := classifyCommand(cmdType)

		if !c.tryReserveWaiter() {
			c.log.Warn("dispatch: concurrency limit reached, rejecting command",
				"cmd_id", cmdID,
				"type", cmdType,
				"class", class.String(),
				"reason", "pending_wait_cap",
				"pending_waiters", c.pendingWaiterCount(),
				"pending_wait_cap", maxPendingSlotWaiters,
			)
			send(protocol.ErrorMsg{
				CmdID:   cmdID,
				Type:    "error",
				Message: fmt.Sprintf("runner busy: too many commands already waiting for a free slot (cap=%d)", maxPendingSlotWaiters),
			})
			continue
		}

		go func(raw protocol.RawCommand, cmdID, cmdType string) {
			c.dispatchOne(raw, cmdID, cmdType, send, func() { _ = conn.Close() })
		}(raw, cmdID, cmdType)
	}

	// Signal the writer to stop, then wait for it to exit.
	close(stopWriter)
	<-writerDone

	return readErr
}

// dispatchOne runs the bounded-wait admission check for one command and, if
// admitted, calls c.dispatchFunc (normally c.runner.Dispatch, see New()). It
// is the entire body of the hand-off goroutine spawned from the recv loop in
// connect(), pulled out into its own method so it can be unit-tested
// directly — tests override dispatchFunc to inject panics/delays without
// needing a live Runner or WebSocket connection. send is called for the
// busy-rejection error and is otherwise passed straight through;
// triggerReconnect is passed straight through too (see runner.Dispatch's
// doc comment — currently only update_key uses it).
//
// releaseWaiter is called as soon as acquire() returns (success or
// failure) — see the comment at that call site for why it must NOT be
// deferred to the end of this method.
func (c *Client) dispatchOne(raw protocol.RawCommand, cmdID, cmdType string, send func(interface{}), triggerReconnect func()) {
	class := classifyCommand(cmdType)

	// releaseWaiterOnce guards against ever double-releasing (releaseWaiter
	// is called explicitly right after acquire() returns, AND unconditionally
	// deferred below as a safety net for a panic occurring before that point
	// — e.g. inside acquire() itself, or context.WithTimeout). Without the
	// once-guard, the normal path would call releaseWaiter() twice (once
	// explicitly, once via the deferred safety net), permanently leaking a
	// pending-waiter slot in the OTHER direction. Without the deferred safety
	// net at all, a panic before the explicit call would leak one waiter
	// slot per panic — after maxPendingSlotWaiters (64) such panics, EVERY
	// subsequent command would be rejected at the pending-wait cap forever,
	// with no way to recover except a restart.
	var waiterReleased bool
	releaseWaiterOnce := func() {
		if !waiterReleased {
			waiterReleased = true
			c.releaseWaiter()
		}
	}
	defer releaseWaiterOnce()

	// Recovers a panic from anywhere in this method, including a
	// (theoretical) panic inside acquire() itself, not just from Dispatch.
	defer func() {
		if p := recover(); p != nil {
			c.log.Error("dispatch panic recovered",
				"panic", p,
				"stack", string(debug.Stack()),
			)
		}
	}()

	// Bounded wait: acquire() blocks at most acquireTimeout() for a free
	// slot before giving up, instead of rejecting the instant the limit is
	// hit. This absorbs the normal brief oversubscription from sub-agents
	// fanning out parallel tool calls. Waiters are woken in broadcast (not
	// strict FIFO) order — see inflightRegistry for why that trade-off is
	// acceptable here.
	ctx, cancel := context.WithTimeout(context.Background(), c.acquireTimeout())
	defer cancel()

	outcome := c.inflight.acquire(ctx, cmdID, cmdType, c.cfg.MaxConcurrency, c.cfg.MaxHeavyConcurrency)
	// pendingWaiters must count only goroutines actually *waiting* to
	// acquire a slot, mirroring maxPendingSlotWaiters' purpose of bounding
	// hand-off-goroutine growth during a burst — it must NOT also count
	// goroutines that already won a slot and are now running the
	// (potentially long) dispatchFunc. Releasing here, the instant acquire()
	// returns (success or failure), rather than deferring to the end of
	// this method, keeps the pending-wait cap a purely burst-absorbing cap
	// instead of it silently becoming a second, lower concurrency ceiling
	// (min(maxPendingSlotWaiters, MaxConcurrency)).
	releaseWaiterOnce()
	if !outcome.OK {
		oldest := c.inflight.snapshot()
		c.log.Warn("dispatch: concurrency limit reached, rejecting command",
			"cmd_id", cmdID,
			"type", cmdType,
			"class", class.String(),
			"reason", outcome.BlockedBy,
			"active", outcome.Global,
			"heavy_active", outcome.Heavy,
			"limit", c.cfg.MaxConcurrency,
			"heavy_limit", c.cfg.MaxHeavyConcurrency,
			"oldest_inflight", summarizeOldest(oldest, rejectionLogTopN),
		)
		send(protocol.ErrorMsg{
			CmdID:   cmdID,
			Type:    "error",
			Message: busyMessage(outcome, c.cfg),
		})
		return
	}
	if outcome.Duplicate {
		c.log.Warn("dispatch: duplicate cmd_id received while original still in-flight",
			"cmd_id", cmdID, "type", cmdType,
		)
	}

	// Unconditional: pairs with the successful acquire() above regardless of
	// duplicates, panics, or which handler branch runs, so no registry entry
	// (and no active-count unit) can ever leak. Deferred after acquire()
	// succeeds — nothing to release if acquire() itself returned !OK above.
	defer c.inflight.remove(cmdID)

	c.dispatchFunc(raw, send, triggerReconnect)
}

// tryReserveWaiter reserves one of the maxPendingSlotWaiters hand-off-
// goroutine slots, returning false without blocking if the cap is already
// reached. This is a separate, tighter cap from the dispatch concurrency
// limits themselves: it bounds how many goroutines may exist AT ALL waiting
// on inflightRegistry.acquire(), so a sustained burst of inbound commands
// beyond capacity cannot grow one goroutine per command for up to
// SlotAcquireTimeoutSeconds each — that would just be a slower-motion
// version of the unbounded-growth problem this whole change exists to
// eliminate. Every true result MUST be paired with exactly one
// releaseWaiter() call (via defer at the call site).
func (c *Client) tryReserveWaiter() bool {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if c.pendingWaiters >= maxPendingSlotWaiters {
		return false
	}
	c.pendingWaiters++
	return true
}

// releaseWaiter releases one hand-off-goroutine slot reserved by a prior
// successful tryReserveWaiter() call. Floors at 0 rather than going
// negative on an unbalanced call — a defensive guard, not something normal
// operation should ever hit (see dispatchOne's releaseWaiterOnce, which
// exists specifically to make every call site call this at most once).
func (c *Client) releaseWaiter() {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if c.pendingWaiters > 0 {
		c.pendingWaiters--
	}
}

// pendingWaiterCount returns the current number of hand-off goroutines
// blocked waiting for a free dispatch slot. Used by Drain — see its doc
// comment for why counting inflight.count() alone is not sufficient.
func (c *Client) pendingWaiterCount() int {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return c.pendingWaiters
}

// busyMessage builds the protocol.ErrorMsg text for a rejected dispatch.
// Always keeps the literal substring "runner busy" (the API surfaces this
// verbatim to the LLM) but states which specific limit was hit so the agent
// can reason about what to do next (e.g. retry a light command immediately,
// but back off longer before retrying another heavy one).
func busyMessage(outcome acquireOutcome, cfg *config.Config) string {
	switch outcome.BlockedBy {
	case "heavy":
		return fmt.Sprintf(
			"runner busy: heavy-command concurrency limit of %d reached (active=%d); light commands are unaffected",
			cfg.MaxHeavyConcurrency, outcome.Heavy,
		)
	default: // "global"
		return fmt.Sprintf(
			"runner busy: concurrency limit of %d reached (active=%d)",
			cfg.MaxConcurrency, outcome.Global,
		)
	}
}

// backoff returns the wait duration for the given attempt number.
// Starts at 1s, doubles each attempt, capped at maxBackoffSec seconds.
func backoff(attempt, maxBackoffSec int) time.Duration {
	secs := math.Pow(2, float64(attempt-1))
	if secs > float64(maxBackoffSec) {
		secs = float64(maxBackoffSec)
	}
	if secs < 1 {
		secs = 1
	}
	return time.Duration(secs) * time.Second
}
