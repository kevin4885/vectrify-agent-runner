// Package runner implements the main command dispatch loop.
// It reads RawCommand messages from the WebSocket client, routes them to the
// appropriate executor, and writes back result/stream/done/error responses.
package runner

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"vectrify/agent-runner/executor"
	"vectrify/agent-runner/protocol"
)

// Runner dispatches commands received from the API to local executors.
type Runner struct {
	fileOps *executor.FileOps
	shell   *executor.Shell
	log     *slog.Logger
}

// New creates a Runner with executors scoped to workspaceRoot.
func New(workspaceRoot string, log *slog.Logger) *Runner {
	return &Runner{
		fileOps: executor.NewFileOps(workspaceRoot),
		shell:   executor.NewShell(workspaceRoot, log),
		log:     log,
	}
}

// Dispatch processes one inbound command and calls send for each outbound message.
// send is called synchronously — callers should queue or channel the results as
// needed.
func (r *Runner) Dispatch(raw protocol.RawCommand, send func(interface{})) {
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

// DecodeRaw decodes a raw JSON WebSocket message into a RawCommand.
func DecodeRaw(data []byte) (protocol.RawCommand, error) {
	var m protocol.RawCommand
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decoding command: %w", err)
	}
	return m, nil
}
