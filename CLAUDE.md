# vectrify-agent-runner

A lightweight Go daemon that customers install on their own machines.  It connects to the Vectrify Cloud API over a persistent WebSocket and executes commands issued by the LLM orchestrator — file CRUD, shell commands (optional), and git operations.

The runner is the customer's machine's execution environment.  The Vectrify API (running in AWS) is purely orchestration — it never sees the customer's local files directly.

---

## Architecture

```
Vectrify Cloud (AWS)                    Customer Machine
────────────────────                    ─────────────────────────────
 LLM (Bedrock)                          vectrify-runner (this app)
     │                                        │
     ▼                                        │  persistent WebSocket
 API (FastAPI)  ◄──────────────────────────►  │  wss://api.vectrify.ai/api/v1/runner/ws
     │                                        │
     └─ runner tools                          ├─ file_op   (read/write/list/delete)
        runner_file_editor                    ├─ shell     (bash or PowerShell)
        runner_shell                          └─ git       (structured git ops)
        runner_git
```

---

## Repository layout

```
vectrify-agent-runner/
├── main.go                Entry point — loads config, sets up logger, starts RunForever loop
├── go.mod                 Go module definition (vectrify/agent-runner)
├── CLAUDE.md              This file
├── README.md              User-facing installation guide
├── config/
│   └── config.go          Config loading from YAML + validation + defaults
├── protocol/
│   └── messages.go        All JSON message structs (RegisterMsg, CommandMsg, ResultMsg, etc.)
├── client/
│   └── ws_client.go       WebSocket connection, registration handshake, reconnect with backoff
├── executor/
│   ├── file_ops.go        File CRUD — read (with line numbers), write, str_replace, insert, delete
│   └── shell.go           Shell execution (bash/PowerShell) + structured git operations
└── runner/
    └── runner.go          Command dispatch loop — routes cmd_type to executor, formats responses
```

---

## Technology stack

| Layer | Technology |
|---|---|
| Language | Go 1.22+ |
| WebSocket | gorilla/websocket v1.5.3 |
| Config | gopkg.in/yaml.v3 |
| Logging | log/slog (stdlib, structured JSON/text) |
| Shell (Linux/macOS) | bash -c "..." |
| Shell (Windows) | powershell -NoProfile -NonInteractive -Command "..." |

---

## Command protocol

All messages are JSON over the WebSocket.

### Runner → API (on connect)
```json
{ "type": "register", "platform": "linux", "workspace_root": "/home/user/projects",
  "allow_shell": true, "version": "1.0.0" }
```

### API → Runner (ack)
```json
{ "type": "registered", "runner_id": 42 }
```

### API → Runner (commands)
```json
{ "cmd_id": "uuid", "type": "file_op",  "command": "view",  "path": "/absolute/path" }
{ "cmd_id": "uuid", "type": "file_op",  "command": "create", "path": "...", "file_text": "..." }
{ "cmd_id": "uuid", "type": "file_op",  "command": "str_replace", "path": "...", "old_str": "...", "new_str": "..." }
{ "cmd_id": "uuid", "type": "file_op",  "command": "insert", "path": "...", "insert_line": 5, "new_str": "..." }
{ "cmd_id": "uuid", "type": "shell",    "command": "npm test", "working_dir": "...", "timeout_seconds": 60 }
{ "cmd_id": "uuid", "type": "git",      "operation": "commit", "working_dir": "...", "message": "..." }
```

### Runner → API (responses)
```json
{ "cmd_id": "uuid", "type": "result", "ok": true,  "data": "file content or output" }
{ "cmd_id": "uuid", "type": "result", "ok": false, "error": "description" }
{ "cmd_id": "uuid", "type": "stream", "stream": "stdout", "data": "chunk..." }
{ "cmd_id": "uuid", "type": "done",   "ok": true, "exit_code": 0 }
{ "cmd_id": "uuid", "type": "error",  "message": "...", "fatal": false }
```

---

## Config file

Default location: `~/.vectrify-runner/config.yaml`

```yaml
api_url:              wss://api.vectrify.ai/api/v1/runner/ws
runner_key:           vrun_...          # from the Vectrify UI (shown once at creation)
workspace_root:       /home/user/projects  # all file ops must be inside this path
allow_shell:          false             # set true to enable runner_shell commands
log_level:            info              # debug | info | warn | error
reconnect_max_backoff: 60               # seconds
```

---

## Building

```bash
# Local build
go build -o vectrify-runner ./...

# Cross-compile for all platforms
GOOS=linux   GOARCH=amd64   go build -ldflags "-X vectrify/agent-runner/config.Version=1.0.0" -o dist/vectrify-runner-linux-amd64 ./...
GOOS=darwin  GOARCH=arm64   go build -ldflags "-X vectrify/agent-runner/config.Version=1.0.0" -o dist/vectrify-runner-darwin-arm64 ./...
GOOS=windows GOARCH=amd64   go build -ldflags "-X vectrify/agent-runner/config.Version=1.0.0" -o dist/vectrify-runner-windows-amd64.exe ./...
```

---

## Security invariants

1. **Path containment** — `executor/file_ops.go` rejects any path outside `workspace_root` before reading or writing. No exceptions.
2. **Shell gating** — `runner_shell` commands are blocked at the API level if `allow_shell=false`; the runner also checks before executing.
3. **Outbound-only networking** — the runner makes no inbound connections; only the one outbound WebSocket to the API.
4. **No privilege escalation** — run as a regular user, never root/admin.
5. **Key never logged** — `runner_key` is used only in the WebSocket URL; it is never written to log files.

---

## Running as a system service

### Linux (systemd)
```ini
[Unit]
Description=Vectrify Agent Runner
After=network.target

[Service]
ExecStart=/usr/local/bin/vectrify-runner
Restart=always
RestartSec=5
User=youruser

[Install]
WantedBy=multi-user.target
```

### macOS (launchd) — coming soon
### Windows (service) — coming soon
