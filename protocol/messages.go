// Package protocol defines the JSON message types exchanged between the
// Vectrify API and the agent runner over the persistent WebSocket connection.
//
// Flow summary:
//
//  1. Runner connects to ws://<api>/api/v1/runner/ws?key=vrun_...
//  2. Runner sends RegisterMsg (first frame after accept).
//  3. API sends RegisteredMsg ack.
//  4. API sends CommandMsg frames; runner sends back ResultMsg / StreamMsg / DoneMsg / ErrorMsg.
package protocol

// ── Outbound (Runner → API) ────────────────────────────────────────────────────

// RegisterMsg is the first message the runner sends after accepting the connection.
type RegisterMsg struct {
	Type          string `json:"type"`           // always "register"
	Platform      string `json:"platform"`       // "linux" | "darwin" | "windows"
	WorkspaceRoot string `json:"workspace_root"` // absolute path configured by the user
	AllowShell    bool   `json:"allow_shell"`    // mirrors config.AllowShell
	Version       string `json:"version"`        // runner app semver, e.g. "1.0.0"
}

// ResultMsg is the terminal response for file and git commands (single result).
type ResultMsg struct {
	CmdID string `json:"cmd_id"`
	Type  string `json:"type"` // "result"
	OK    bool   `json:"ok"`
	Data  string `json:"data,omitempty"`  // file content, directory listing, git output…
	Error string `json:"error,omitempty"` // present when ok=false
}

// StreamMsg carries a chunk of stdout or stderr from a running shell command.
type StreamMsg struct {
	CmdID  string `json:"cmd_id"`
	Type   string `json:"type"`   // "stream"
	Stream string `json:"stream"` // "stdout" | "stderr"
	Data   string `json:"data"`
}

// DoneMsg is the terminal response for shell commands.
type DoneMsg struct {
	CmdID    string `json:"cmd_id"`
	Type     string `json:"type"`      // "done"
	OK       bool   `json:"ok"`
	ExitCode int    `json:"exit_code"`
}

// ErrorMsg is sent when the runner encounters an error that cannot be returned
// as part of a normal result (e.g. unknown command type, internal panic).
type ErrorMsg struct {
	CmdID   string `json:"cmd_id,omitempty"`
	Type    string `json:"type"`    // "error"
	Message string `json:"message"` // human-readable description
	Fatal   bool   `json:"fatal"`   // true = runner will close connection
}

// ── Inbound (API → Runner) ─────────────────────────────────────────────────────

// RegisteredMsg is the ack the API sends after a successful RegisterMsg.
type RegisteredMsg struct {
	Type     string `json:"type"`      // "registered"
	RunnerID int    `json:"runner_id"`
}

// CommandMsg is the generic inbound command envelope.  The Type field
// determines which fields in Payload are populated.
type CommandMsg struct {
	CmdID string                 `json:"cmd_id"`
	Type  string                 `json:"type"`
	// Remaining fields decoded into a map for flexible dispatch.
	// Executor functions cast the values they need.
	Payload map[string]interface{} `json:"-"` // populated by the receiver after decoding
}

// RawCommand is the wire format decoded before dispatch.
type RawCommand map[string]interface{}

func (r RawCommand) CmdID() string  { return str(r["cmd_id"]) }
func (r RawCommand) Type() string   { return str(r["type"]) }

func str(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func Bool(v interface{}) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func Int(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case int64:
		return int(n)
	}
	return 0
}
