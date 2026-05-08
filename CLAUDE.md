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
├── main.go                Entry point — loads config, sets up logger, calls runService()
├── service_windows.go     Windows Service handler (build tag: windows) — svc.Handler impl,
│                          detects SCM vs interactive mode via svc.IsWindowsService()
├── service_other.go       Linux/macOS stub (build tag: !windows) — delegates to runInteractive()
├── go.mod                 Go module definition (vectrify/agent-runner)
├── build.ps1              Cross-compile all 5 platform binaries into dist/ (run from Windows)
├── install.ps1            Interactive Windows installer — prompts for config, installs as
│                          Windows Service via sc.exe (C:\ProgramData\VectrifyRunner\config.yaml)
├── install.sh             Interactive Linux/macOS installer — prompts for config, installs as
│                          systemd service (Linux) or launchd daemon (macOS)
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
| Windows Service | golang.org/x/sys/windows/svc |
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

```powershell
# Build all 5 platform binaries into dist/ (from Windows)
.\build.ps1

# Build with a specific version
.\build.ps1 -Version 1.2.3

# Local build only (current platform)
go build -o vectrify-runner.exe .
```

## Installing

### One-liner (recommended — downloads binary automatically from latest release)

```bash
# macOS / Linux
bash <(curl -fsSL https://github.com/vectrify/vectrify-agent-runner/releases/latest/download/install.sh)
```

```powershell
# Windows — works from any PowerShell window (prompts for UAC automatically)
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; $f = "$env:TEMP\vectrify-install.ps1"; iwr -useb https://github.com/kevin4885/vectrify-agent-runner/releases/latest/download/install.ps1 -OutFile $f; Start-Process powershell -Verb RunAs -ArgumentList "-ExecutionPolicy Bypass -File `"$f`"" -Wait; Remove-Item $f -EA 0
```

### Local install (after running build.ps1)

```powershell
# Windows
.\install.ps1
```

```bash
# Linux / macOS
sudo ./install.sh
```

Both installers prompt for: workspace root path, runner key, allow_shell, log_level,
and reconnect_max_backoff. Config is written to:
- Windows : C:\ProgramData\VectrifyRunner\config.yaml
- Linux   : /etc/vectrify-runner/config.yaml
- macOS   : /etc/vectrify-runner/config.yaml

## Releasing

```bash
git tag v1.0.0
git push origin v1.0.0
```

GitHub Actions (`.github/workflows/release.yml`) triggers automatically, builds all
5 platform binaries, and publishes them along with `install.sh` and `install.ps1`
as assets on the GitHub Release. The one-liner install commands always pull from
`releases/latest/download/` so users get the newest version automatically.

---

## Security invariants

1. **Path containment** — `executor/file_ops.go` rejects any path outside `workspace_root` before reading or writing. No exceptions.
2. **Shell gating** — `runner_shell` commands are blocked at the API level if `allow_shell=false`; the runner also checks before executing.
3. **Outbound-only networking** — the runner makes no inbound connections; only the one outbound WebSocket to the API.
4. **No privilege escalation** — run as a regular user, never root/admin.
5. **Key never logged** — `runner_key` is used only in the WebSocket URL; it is never written to log files.

---

## Running as a system service

Use `install.ps1` (Windows) or `install.sh` (Linux/macOS) — they handle everything.

### Service lifecycle (Windows)

The binary uses `golang.org/x/sys/windows/svc` to detect whether it was launched
by the Windows SCM. When running as a service, `service_windows.go` implements
`svc.Handler` and handles `SERVICE_CONTROL_STOP` / `SHUTDOWN`. When running
interactively in a terminal, it falls back to SIGTERM/SIGINT handling as before.

### Service lifecycle (Linux / macOS)

`service_other.go` is a no-op stub — systemd and launchd both stop services by
sending SIGTERM, which `runInteractive()` in `main.go` already handles correctly.
No extra service-awareness is needed in the binary on these platforms.
