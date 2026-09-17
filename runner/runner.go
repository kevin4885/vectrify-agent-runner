// Package runner implements the main command dispatch loop.
// It reads RawCommand messages from the WebSocket client, routes them to the
// appropriate executor, and writes back result/stream/done/error responses.
package runner

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
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
	browser       *executor.BrowserManager
	workspaceRoot string
	cfg           *config.Config
	log           *slog.Logger
}

// New creates a Runner with executors scoped to workspaceRoot.
func New(cfg *config.Config, log *slog.Logger) *Runner {
	return &Runner{
		fileOps: executor.NewFileOps(cfg.WorkspaceRoot),
		shell:   executor.NewShell(cfg.WorkspaceRoot, log),
		browser: executor.NewBrowserManager(
			cfg.WorkspaceRoot,
			cfg.MaxBrowserSessions,
			time.Duration(cfg.BrowserIdleTimeoutSeconds)*time.Second,
			cfg.IsBrowserHeadless(),
			log,
		),
		workspaceRoot: cfg.WorkspaceRoot,
		cfg:           cfg,
		log:           log,
	}
}

// Shutdown releases resources held by the Runner's executors — currently
// just the browser manager's Chromium process + driver, if it was ever
// started. Safe to call even if no browser command was ever dispatched
// (BrowserManager.Shutdown no-ops in that case). Called from main.go on
// graceful shutdown.
func (r *Runner) Shutdown() {
	r.browser.Shutdown()
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
	case "browser":
		r.handleBrowser(cmdID, raw, send)
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

// ── Browser ────────────────────────────────────────────────────────────────

// handleBrowser dispatches one "browser" command to the shared
// BrowserManager. Gated by cfg.AllowShell — browser automation is treated
// as an extension of shell-level trust rather than its own permission:
// there is deliberately no separate allow_browser setting.
//
// Before dispatching to any action, it calls BrowserManager.EnsureInstalled,
// which auto-downloads the Playwright driver + Chromium browser the first
// time browser automation is ever used on this machine (streaming progress
// back as the same StreamMsg chunks a shell command uses for stdout), so
// the customer never has to run an install step by hand. Every subsequent
// call is a near-instant no-op once installed.
func (r *Runner) handleBrowser(cmdID string, raw protocol.RawCommand, send func(interface{})) {
	if !r.cfg.AllowShell {
		send(protocol.ResultMsg{
			CmdID: cmdID, Type: "result", OK: false,
			Error: "browser commands require allow_shell=true in config.yaml (browser automation shares the shell permission — there is no separate allow_browser setting)",
		})
		return
	}

	progressCh := make(chan executor.InstallProgress, 16)
	installDone := make(chan error, 1)
	go func() {
		installDone <- r.browser.EnsureInstalled(progressCh)
		close(progressCh)
	}()
	var installLog strings.Builder
	for p := range progressCh {
		installLog.WriteString(p.Data)
		send(protocol.StreamMsg{CmdID: cmdID, Type: "stream", Stream: "stdout", Data: p.Data})
	}
	if err := <-installDone; err != nil {
		errMsg := fmt.Sprintf("browser driver unavailable: %s", err.Error())
		if installLog.Len() > 0 {
			// Prepend the partial install log so the caller sees what was
			// attempted, not just the final failure. Needed because the API's
			// send_command() helper only accumulates stream chunks for a
			// "done"-terminated command (shell's convention); a "result"
			// terminal message like this one would otherwise silently drop
			// everything sent via StreamMsg above.
			errMsg = installLog.String() + "\n" + errMsg
		}
		send(protocol.ResultMsg{CmdID: cmdID, Type: "result", OK: false, Error: errMsg})
		return
	}

	action, _ := raw["action"].(string)
	sessionID, _ := raw["session_id"].(string)
	if sessionID == "" {
		send(protocol.ResultMsg{CmdID: cmdID, Type: "result", OK: false, Error: "session_id is required"})
		return
	}

	var data string
	var err error

	switch action {
	case "launch":
		err = r.browser.Launch(sessionID)
		if err == nil {
			data = fmt.Sprintf("browser session %q launched", sessionID)
		}

	case "goto":
		url, _ := raw["url"].(string)
		if url == "" {
			err = fmt.Errorf("url is required for goto")
			break
		}
		timeout := protocol.Int(raw["timeout_seconds"])
		err = r.browser.Goto(sessionID, url, timeout)
		if err == nil {
			data = fmt.Sprintf("navigated to %s", url)
		}

	case "click":
		selector, _ := raw["selector"].(string)
		if selector == "" {
			err = fmt.Errorf("selector is required for click")
			break
		}
		timeout := protocol.Int(raw["timeout_seconds"])
		err = r.browser.Click(sessionID, selector, timeout)
		if err == nil {
			data = fmt.Sprintf("clicked %s", selector)
		}

	case "fill":
		selector, _ := raw["selector"].(string)
		value, _ := raw["value"].(string)
		if selector == "" {
			err = fmt.Errorf("selector is required for fill")
			break
		}
		timeout := protocol.Int(raw["timeout_seconds"])
		err = r.browser.Fill(sessionID, selector, value, timeout)
		if err == nil {
			data = fmt.Sprintf("filled %s", selector)
		}

	case "wait_for_selector":
		selector, _ := raw["selector"].(string)
		if selector == "" {
			err = fmt.Errorf("selector is required for wait_for_selector")
			break
		}
		state, _ := raw["state"].(string)
		timeout := protocol.Int(raw["timeout_seconds"])
		err = r.browser.WaitForSelector(sessionID, selector, state, timeout)
		if err == nil {
			data = fmt.Sprintf("selector %s reached state", selector)
		}

	case "screenshot":
		path, _ := raw["path"].(string)
		fullPage := protocol.Bool(raw["full_page"])
		err = r.browser.Screenshot(sessionID, path, fullPage)
		if err == nil {
			data = fmt.Sprintf("screenshot saved: %s", path)
		}

	case "get_text":
		selector, _ := raw["selector"].(string)
		data, err = r.browser.GetText(sessionID, selector)

	case "content":
		data, err = r.browser.GetContent(sessionID)

	case "evaluate":
		expression, _ := raw["expression"].(string)
		if expression == "" {
			err = fmt.Errorf("expression is required for evaluate")
			break
		}
		var result interface{}
		result, err = r.browser.Evaluate(sessionID, expression)
		if err == nil {
			if b, marshalErr := json.Marshal(result); marshalErr == nil {
				data = string(b)
			} else {
				data = fmt.Sprintf("%v", result)
			}
		}

	case "close":
		r.browser.Close(sessionID)
		data = fmt.Sprintf("browser session %q closed", sessionID)

	default:
		send(protocol.ResultMsg{
			CmdID: cmdID, Type: "result", OK: false,
			Error: fmt.Sprintf("unknown browser action: %q", action),
		})
		return
	}

	if err != nil {
		send(protocol.ResultMsg{CmdID: cmdID, Type: "result", OK: false, Error: err.Error()})
		return
	}
	if installLog.Len() > 0 {
		// Same rationale as the failure path above: a "result"-terminated
		// command's preceding StreamMsg chunks are otherwise invisible to
		// the caller, so fold the one-time install log into the success
		// data instead of losing it silently.
		data = installLog.String() + "\n" + data
	}
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
