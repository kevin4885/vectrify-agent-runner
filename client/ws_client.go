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
	"time"

	"github.com/gorilla/websocket"

	"vectrify/agent-runner/config"
	"vectrify/agent-runner/protocol"
	"vectrify/agent-runner/runner"
)

// Client manages one persistent WebSocket connection to the Vectrify API.
type Client struct {
	cfg     *config.Config
	runner  *runner.Runner
	log     *slog.Logger
}

// New creates a Client.
func New(cfg *config.Config, r *runner.Runner, log *slog.Logger) *Client {
	return &Client{cfg: cfg, runner: r, log: log}
}

// RunForever connects and reconnects indefinitely until the process is stopped.
// It uses exponential backoff capped at cfg.ReconnectMaxBackoff seconds.
func (c *Client) RunForever() {
	attempt := 0
	for {
		c.log.Info("connecting", "url", c.cfg.APIURL, "attempt", attempt+1)
		err := c.connect()
		if err != nil {
			attempt++
			wait := backoff(attempt, c.cfg.ReconnectMaxBackoff)
			c.log.Warn("connection lost", "err", err, "retry_in", wait)
			time.Sleep(wait)
		} else {
			// Clean disconnect (should not happen in normal operation).
			attempt = 0
			time.Sleep(2 * time.Second)
		}
	}
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
	if err := conn.WriteJSON(reg); err != nil {
		return fmt.Errorf("sending register: %w", err)
	}

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

	// ── Message loop ──────────────────────────────────────────────────────────
	send := func(msg interface{}) {
		if err := conn.WriteJSON(msg); err != nil {
			c.log.Warn("send error", "err", err)
		}
	}

	for {
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		raw, err := runner.DecodeRaw(msgBytes)
		if err != nil {
			c.log.Warn("decode error", "err", err)
			continue
		}

		// Dispatch each command in its own goroutine so the recv loop is never
		// blocked by a long-running shell command.
		go c.runner.Dispatch(raw, send)
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
