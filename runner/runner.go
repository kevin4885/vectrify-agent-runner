// Package runner implements the main command dispatch loop.
// It reads RawCommand messages from the WebSocket client, routes them to the
// appropriate executor, and writes back result/stream/done/error responses.
package runner

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"vectrify/agent-runner/config"
	"vectrify/agent-runner/executor"
	"vectrify/agent-runner/protocol"
)

// maxShellTimeout is the hard upper limit on any shell command timeout requested
// by the API.  Prevents a rogue or buggy server payload from holding a dispatch
// goroutine for an unbounded duration.
const maxShellTimeout = 10 * time.Minute

// Runner dispatches commands received from the API to local executors.
type Runner struct {
	fileOps       *executor.FileOps
	shell         *executor.Shell
	workspaceRoot string
	cfg           *config.Config
	log           *slog.Logger
}

// New creates a Runner with executors scoped to workspaceRoot.
func New(cfg *config.Config, log *slog.Logger) *Runner {
	return &Runner{
		fileOps:       executor.NewFileOps(cfg.WorkspaceRoot),
		shell:         executor.NewShell(cfg.WorkspaceRoot, log),
		workspaceRoot: cfg.WorkspaceRoot,
		cfg:           cfg,
		log:           log,
	}
}

// Dispatch processes one inbound command and calls send for each outbound message.
// send is called synchronously — callers should queue or channel the results as
// needed. triggerReconnect, if non-nil, is called by handlers that need the
// current WebSocket connection closed so the client's reconnect loop picks up
// changed state (currently only update_key uses this, to reconnect with the
// new key immediately instead of waiting for the next natural disconnect).
func (r *Runner) Dispatch(raw protocol.RawCommand, send func(interface{}), triggerReconnect func()) {
	cmdID := raw.CmdID()
	cmdType := raw.Type()

	r.log.Info("dispatch", "cmd_id", cmdID, "type", cmdType)

	switch cmdType {
	case "file_op":
		r.handleFileOp(cmdID, raw, send)
	case "shell":
		r.handleShell(cmdID, raw, send)
	case "git":
		r.handleGit(cmdID, raw, send)
	case "file_transfer":
		r.handleFileTransfer(cmdID, raw, send)
	case "update_key":
		r.handleUpdateKey(cmdID, raw, send, triggerReconnect)
	default:
		send(protocol.ErrorMsg{
			CmdID:   cmdID,
			Type:    "error",
			Message: fmt.Sprintf("unknown command type: %q", cmdType),
		})
	}
}

// ── File operations ────────────────────────────────────────────────────────────

func (r *Runner) handleFileOp(cmdID string, raw protocol.RawCommand, send func(interface{})) {
	command, _ := raw["command"].(string)
	path, _ := raw["path"].(string)

	var data string
	var err error

	switch command {
	case "view":
		var viewRange []int
		if vr, ok := raw["view_range"].([]interface{}); ok && len(vr) == 2 {
			viewRange = []int{protocol.Int(vr[0]), protocol.Int(vr[1])}
		}
		data, err = r.fileOps.ReadFile(path, viewRange)

	case "create":
		content, _ := raw["file_text"].(string)
		err = r.fileOps.WriteFile(path, content)
		if err == nil {
			data = fmt.Sprintf("File created successfully: %s", path)
		}

	case "str_replace":
		oldStr, _ := raw["old_str"].(string)
		newStr, _ := raw["new_str"].(string)
		data, err = r.fileOps.StrReplace(path, oldStr, newStr)

	case "insert":
		lineNum := protocol.Int(raw["insert_line"])
		newStr, _ := raw["new_str"].(string)
		data, err = r.fileOps.Insert(path, lineNum, newStr)

	case "delete":
		err = r.fileOps.DeleteFile(path)
		if err == nil {
			data = fmt.Sprintf("File deleted: %s", path)
		}

	default:
		send(protocol.ResultMsg{
			CmdID: cmdID, Type: "result", OK: false,
			Error: fmt.Sprintf("unknown file_op command: %q", command),
		})
		return
	}

	if err != nil {
		send(protocol.ResultMsg{CmdID: cmdID, Type: "result", OK: false, Error: err.Error()})
		return
	}
	send(protocol.ResultMsg{CmdID: cmdID, Type: "result", OK: true, Data: data})
}

// ── Shell ──────────────────────────────────────────────────────────────────────

func (r *Runner) handleShell(cmdID string, raw protocol.RawCommand, send func(interface{})) {
	cmd, _ := raw["command"].(string)
	workingDir, _ := raw["working_dir"].(string)
	timeout := protocol.Int(raw["timeout_seconds"])
	if timeout <= 0 {
		timeout = 60
	}
	// Hard cap: prevent a runaway command from holding a dispatch goroutine
	// indefinitely regardless of what the API sends.
	if time.Duration(timeout)*time.Second > maxShellTimeout {
		timeout = int(maxShellTimeout.Seconds())
	}

	chunks := make(chan executor.ShellChunk, 64)
	result := make(chan executor.ShellResult, 1)

	go r.shell.Run(cmd, workingDir, timeout, chunks, result)

	for chunk := range chunks {
		send(protocol.StreamMsg{
			CmdID:  cmdID,
			Type:   "stream",
			Stream: chunk.Stream,
			Data:   chunk.Data,
		})
	}

	res := <-result
	send(protocol.DoneMsg{
		CmdID:    cmdID,
		Type:     "done",
		OK:       res.OK,
		ExitCode: res.ExitCode,
	})
}

// ── Git ────────────────────────────────────────────────────────────────────────

func (r *Runner) handleGit(cmdID string, raw protocol.RawCommand, send func(interface{})) {
	op, _ := raw["operation"].(string)
	workingDir, _ := raw["working_dir"].(string)

	output, err := r.shell.RunGit(op, workingDir, raw)
	if err != nil {
		send(protocol.ResultMsg{CmdID: cmdID, Type: "result", OK: false, Error: err.Error()})
		return
	}
	send(protocol.ResultMsg{CmdID: cmdID, Type: "result", OK: true, Data: output})
}

// ── File transfer ──────────────────────────────────────────────────────────────

func (r *Runner) handleFileTransfer(cmdID string, raw protocol.RawCommand, send func(interface{})) {
	direction, _ := raw["direction"].(string)
	url, _ := raw["url"].(string)
	path, _ := raw["path"].(string)
	maxBytes := protocol.Int64(raw["max_bytes"])
	if maxBytes <= 0 {
		maxBytes = 100 * 1024 * 1024 // default 100 MiB
	}
	overwrite := protocol.Bool(raw["overwrite"])

	// Never log the URL — it contains presigned bearer credentials.
	r.log.Info("file_transfer", "cmd_id", cmdID, "direction", direction, "path", path)

	result, err := executor.TransferFile(r.workspaceRoot, direction, url, path, maxBytes, overwrite)
	if err != nil {
		send(protocol.ResultMsg{CmdID: cmdID, Type: "result", OK: false, Error: err.Error()})
		return
	}

	// Encode result as a small JSON string (matches the ResultMsg.Data convention).
	data := fmt.Sprintf(`{"bytes":%d,"sha256":%q}`, result.Bytes, result.SHA256)
	send(protocol.ResultMsg{CmdID: cmdID, Type: "result", OK: true, Data: data})
}

// DecodeRaw decodes a raw JSON WebSocket message into a RawCommand.
func DecodeRaw(data []byte) (protocol.RawCommand, error) {
	var m protocol.RawCommand
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decoding command: %w", err)
	}
	return m, nil
}

// ── Update key ─────────────────────────────────────────────────────────────────

// handleUpdateKey rewrites runner_key in the local config file (owned by this
// process's user — no elevated privileges needed) and, on success, closes the
// current WebSocket connection so the client immediately reconnects using the
// new key. This lets a key rotation apply live with zero downtime and no
// manual restart, as long as the runner is currently connected.
func (r *Runner) handleUpdateKey(cmdID string, raw protocol.RawCommand, send func(interface{}), triggerReconnect func()) {
	newKey, _ := raw["new_key"].(string)
	if newKey == "" {
		send(protocol.ResultMsg{CmdID: cmdID, Type: "result", OK: false, Error: "new_key is required"})
		return
	}
	if err := r.cfg.UpdateRunnerKey(newKey); err != nil {
		r.log.Error("update_key: failed to write config", "err", err)
		send(protocol.ResultMsg{CmdID: cmdID, Type: "result", OK: false, Error: err.Error()})
		return
	}
	r.log.Info("update_key: config updated, reconnecting with new key", "cmd_id", cmdID)
	send(protocol.ResultMsg{CmdID: cmdID, Type: "result", OK: true, Data: "runner_key updated"})
	if triggerReconnect != nil {
		// Give the result message a moment to actually flush over the wire
		// before we tear down the connection.
		go func() {
			time.Sleep(500 * time.Millisecond)
			triggerReconnect()
		}()
	}
}
