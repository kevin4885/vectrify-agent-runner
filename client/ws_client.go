// Package client manages the persistent WebSocket connection to the Vectrify API.
// It handles authentication, the registration handshake, reconnection with
// exponential backoff, and the bidirectional message loop.
package client

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"runtime/debug"
	"sync/atomic"
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

	// maxDispatchConcurrency is the maximum number of command goroutines that may
	// run simultaneously.  Prevents unbounded goroutine growth if the API sends a
	// burst of commands (e.g. due to a server-side bug or replay).
	maxDispatchConcurrency = 32
)

// Client manages one persistent WebSocket connection to the Vectrify API.
type Client struct {
	cfg            *config.Config
	runner         *runner.Runner
	log            *slog.Logger
	activeCommands atomic.Int64 // count of in-flight dispatch goroutines
}

// New creates a Client.
func New(cfg *config.Config, r *runner.Runner, log *slog.Logger) *Client {
	return &Client{cfg: cfg, runner: r, log: log}
}

// Drain blocks until all in-flight dispatch goroutines have finished or
// timeout elapses.  Called by the auto-updater before os.Exit so that
// commands already dispatched can complete and send their results back.
func (c *Client) Drain(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.activeCommands.Load() == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.log.Warn("drain timeout: exiting with in-flight commands",
		"active", c.activeCommands.Load(),
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
	writeCh    := make(chan interface{}, 64)
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

		// Dispatch each command in its own goroutine so the recv loop is never
		// blocked by a long-running shell command.  A deferred recover ensures a
		// panicking handler never crashes the process.  The semaphore limits
		// concurrent goroutines; if full the command is rejected with an error so
		// the API caller gets a clear response rather than a silent queue build-up.
		cmdID := raw.CmdID()
		if c.activeCommands.Load() >= maxDispatchConcurrency {
			c.log.Warn("dispatch: concurrency limit reached, rejecting command", "cmd_id", cmdID)
			send(protocol.ErrorMsg{
				CmdID:   cmdID,
				Type:    "error",
				Message: fmt.Sprintf("runner busy: concurrency limit of %d reached", maxDispatchConcurrency),
			})
			continue
		}
		c.activeCommands.Add(1)
		go func(raw protocol.RawCommand) {
			defer c.activeCommands.Add(-1)
			defer func() {
				if p := recover(); p != nil {
					c.log.Error("dispatch panic recovered",
						"panic", p,
						"stack", string(debug.Stack()),
					)
				}
			}()
			c.runner.Dispatch(raw, send, func() {
				// Force this connection closed so the outer ReadMessage loop
				// errors out and RunForever immediately reconnects, picking up
				// the (just-updated) runner key from cfg. Safe to call multiple
				// times / concurrently — Close() on an already-closed conn is a
				// no-op error we don't care about.
				_ = conn.Close()
			})
		}(raw)
	}

	// Signal the writer to stop, then wait for it to exit.
	close(stopWriter)
	<-writerDone

	return readErr
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
