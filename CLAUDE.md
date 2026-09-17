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
        runner_shell                          ├─ git       (structured git ops)
        runner_git                            ├─ file_transfer (S3 ↔ runner filesystem)
                                               └─ browser   (Playwright automation, opt-in)
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
│   ├── file_transfer.go   File transfer via presigned S3 URLs (download runner←S3, upload runner→S3)
│   ├── shell.go           Shell execution (bash/PowerShell) + structured git operations
│   └── browser.go         Playwright-driven browser automation (opt-in, see "Browser automation" below)
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
| Browser automation | github.com/mxschmitt/playwright-go (opt-in, see below) |
| Stealth evasions | github.com/jonfriesen/playwright-go-stealth (embedded JS only, see below) |

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
{ "cmd_id": "uuid", "type": "file_transfer", "direction": "download", "url": "<presigned-GET>",
  "path": "/absolute/path/on/runner", "max_bytes": 104857600, "overwrite": false }
{ "cmd_id": "uuid", "type": "file_transfer", "direction": "upload",   "url": "<presigned-PUT>",
  "path": "/absolute/path/on/runner", "max_bytes": 104857600 }
{ "cmd_id": "uuid", "type": "update_key", "new_key": "vrun_..." }
{ "cmd_id": "uuid", "type": "browser", "action": "launch",  "session_id": "s1" }
{ "cmd_id": "uuid", "type": "browser", "action": "goto",    "session_id": "s1", "url": "https://example.com", "timeout_seconds": 30 }
{ "cmd_id": "uuid", "type": "browser", "action": "click",   "session_id": "s1", "selector": "#submit" }
{ "cmd_id": "uuid", "type": "browser", "action": "fill",    "session_id": "s1", "selector": "#search", "value": "hello" }
{ "cmd_id": "uuid", "type": "browser", "action": "wait_for_selector", "session_id": "s1", "selector": "#result", "state": "visible" }
{ "cmd_id": "uuid", "type": "browser", "action": "screenshot", "session_id": "s1", "path": "/absolute/path/shot.png", "full_page": true }
{ "cmd_id": "uuid", "type": "browser", "action": "get_text", "session_id": "s1", "selector": "#result" }
{ "cmd_id": "uuid", "type": "browser", "action": "content",  "session_id": "s1" }
{ "cmd_id": "uuid", "type": "browser", "action": "evaluate", "session_id": "s1", "expression": "document.title" }
{ "cmd_id": "uuid", "type": "browser", "action": "close",    "session_id": "s1" }
```

**update_key notes:**
- Rewrites `runner_key` in the runner's own `config.yaml` on disk (no root/elevated
  access needed — the file is already owned by the user this process runs as)
  and swaps the in-memory key, then closes the current WebSocket connection so
  the client's existing reconnect loop immediately dials back in with the new
  key. Applied live with zero downtime, no restart command needed.
- API side: `POST /runners/{id}/rotate-key` calls this automatically when
  `registry.is_connected(runner_id)` is true, and reports back `pushedLive` in
  the response. If the runner is offline, or on a version that predates this
  command (unknown cmd type → runner replies with an `error` message, or the
  send simply times out), `pushedLive=false` and the caller must fall back to
  `install.sh --set-key <key>` / `install.ps1 -SetKey <key>`.

**file_transfer notes:**
- `direction`: `"download"` = S3 → runner filesystem; `"upload"` = runner filesystem → S3.
- `url`: HTTPS presigned URL only. The runner never logs this value.
- `path`: absolute path on the runner machine; must be inside `workspace_root`.
- `max_bytes`: maximum file size in bytes (default/max 104857600 = 100 MiB).
- `overwrite`: download only — if the destination already exists and `overwrite=false` the command fails with a descriptive error.

**browser notes:**
- Gated by `allow_shell` in config.yaml — browser automation shares the shell
  permission rather than having its own flag. There is deliberately no
  `allow_browser` setting: any runner that trusts the agent with a real shell
  already trusts it with equivalent (or greater) local-machine capability, so
  a separate toggle would add config surface without a real security
  boundary. Unlike shell, there is no separate API-side check; the runner is
  the sole enforcement point.
- `session_id` is caller-supplied and identifies a stateful browser session (one
  Chromium `BrowserContext` + `Page`) that persists across multiple `browser` commands
  until explicitly closed (`action: "close"`) or reaped after `browser_idle_timeout_seconds`
  of inactivity. The first action referencing a new `session_id` implicitly creates it.
- Actions: `launch`, `goto`, `click`, `fill`, `wait_for_selector` (`state`: attached |
  detached | hidden | visible), `screenshot` (`path` validated against `workspace_root`,
  same containment rule as `file_op`), `get_text` (page body text, or a single selector's
  text when `selector` is set), `content` (full page HTML), `evaluate` (arbitrary JS,
  result JSON-encoded into `data`), `close` (idempotent — closing an unknown/already-closed
  `session_id` is not an error).
- Requires the Playwright driver + Chromium binaries to be installed once per machine —
  see "Browser automation" below.
- Classified `heavy` in `client/classify.go` (shares the heavy concurrency sub-limit with
  `shell` and `file_transfer`).

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
max_concurrency:       32               # max simultaneous dispatched commands, all classes
max_heavy_concurrency: 24               # sub-limit for "heavy" commands (shell, file_transfer);
                                         # must be strictly less than max_concurrency (clamped to
                                         # max_concurrency-1 with a warning if not), so light
                                         # commands (file_op, git, update_key, unknown types)
                                         # always keep at least one slot free even when every
                                         # heavy slot is occupied
slot_acquire_timeout_seconds: 3         # how long a command waits for a free slot before being
                                         # rejected as "runner busy", instead of rejecting instantly
max_browser_sessions:  3                # max concurrent browser sessions (each = one Chromium
                                         # BrowserContext + Page kept alive across commands)
browser_idle_timeout_seconds: 300       # auto-close a browser session after this many seconds of
                                         # inactivity (no command referencing its session_id)
browser_headless:      true             # false only for local debugging on a machine with a display
```

All three concurrency knobs are optional; the defaults shown above match the
hardcoded behavior from before they became configurable, so an existing
config.yaml with none of these keys set behaves identically. The three
`max_browser_sessions`/`browser_idle_timeout_seconds`/`browser_headless` keys
are likewise all optional — an existing config.yaml with none of them set
uses the defaults shown above and behaves identically. There is no
`allow_browser` key: browser commands are gated by `allow_shell` (see
"Browser automation" below).

---

## Browser automation

Gated by `allow_shell` — there is deliberately no separate `allow_browser`
setting. The `browser` command type lets the API drive a real Chromium
browser on the runner machine — navigate, click, fill, screenshot, extract
text/HTML, evaluate JS — via
[`github.com/mxschmitt/playwright-go`](https://github.com/mxschmitt/playwright-go).
See the "browser notes" in the Command protocol section above for the full action list.

### Installation

Nothing manual is required to enable browser automation once `allow_shell: true`
is set — the first `browser` command on a given machine automatically downloads
the Playwright driver + Chromium binaries (~300 MB, Chromium only — Firefox and
WebKit are explicitly skipped) if they are not already present, streaming
progress back on the command's stdout the same way a long shell command would,
then proceeds with the original action once installed. Every command after
that first one is a near-instant no-op. See `BrowserManager.EnsureInstalled` in
`executor/browser.go` and `handleBrowser` in `runner/runner.go`.

To avoid paying that ~300 MB delay on whichever command happens to run first,
you can pre-warm the install instead — either interactively during
`install.ps1`/`install.sh` (prompted only when shell mode is enabled), or by
running the binary with `-install-browsers` directly, which exits immediately
after downloading:

```powershell
# Windows
.\vectrify-runner.exe -install-browsers
```
```bash
# Linux / macOS
./vectrify-runner -install-browsers
```

Both paths install into the *running user's* home directory
(`~/.cache/ms-playwright` on Linux/macOS, `%LOCALAPPDATA%\ms-playwright` on
Windows) — whoever the runner service actually runs as. This matters for the
interactive installers specifically: `install.sh` runs under `sudo` as root
but the systemd/launchd service runs as `ORIGINAL_USER`, and pre-1.x
`install.ps1` ran the Windows service as `LocalSystem`, a different profile
than whoever ran the installer interactively. Both installers now run the
pre-install step as the actual service account (see "Windows service
account" below) so a pre-install actually lands where the running service
will look for it, rather than silently going to waste.

### Stealth

Every launched browser context:
- disables the Blink automation-controlled flag (`--disable-blink-features=AutomationControlled`)
- uses a realistic desktop Chrome UA string, viewport, locale, and timezone (instead of
  Playwright's own defaults, which are themselves a bot-detection signal)
- injects the evasion script from
  [`github.com/jonfriesen/playwright-go-stealth`](https://github.com/jonfriesen/playwright-go-stealth)
  (the extracted `puppeteer-extra-plugin-stealth` evasions) into every new page

**This raises the bar against basic/medium bot detection — it is NOT a guarantee
against advanced systems** (Cloudflare Turnstile with behavioral scoring, Akamai,
PerimeterX/DataDome). Those fingerprint TLS/JA3, canvas/audio noise, and mouse/timing
behavior in ways a generic stealth layer cannot fully spoof, and it is a permanent
cat-and-mouse game with no guarantee either way.

**Implementation note (module-path gotcha):** `executor/browser.go` imports
`github.com/jonfriesen/playwright-go-stealth` only for its embedded `stealth.StealthJS`
string constant, injected via our own `page.AddInitScript(...)` call — NOT via that
package's own `stealth.Inject(page)` helper. That helper's signature is pinned to the
older `github.com/playwright-community/playwright-go` module path, which Go treats as a
completely different type identity than `github.com/mxschmitt/playwright-go` (same
upstream project, renamed on GitHub over time — the code lineage is identical but the
two module paths are NOT interchangeable to the Go compiler). The newer module path is
required here because the driver version pinned by the old path's latest tagged release
(`v0.4201.1`) points at Playwright driver binaries Microsoft no longer hosts —
`playwright.Install()` against it 404s. If `playwright-go-stealth` ever ships a release
pinned to the newer module path, switching to its `Inject()` helper directly would be a
safe simplification.

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
# macOS / Linux — download first, then run with sudo (do NOT use
# `sudo bash <(curl ...)` or `curl ... | sudo bash` — both break under sudo,
# see README.md Troubleshooting)
curl -fsSLO https://github.com/kevin4885/vectrify-agent-runner/releases/latest/download/install.sh
sudo bash install.sh
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

On macOS/Linux the service runs as the invoking (`sudo`) user, not root — see
Security invariant #4 below. Re-running `install.sh` on an existing install
also repairs the macOS launchd plist and log directory if a previous install
left them broken (e.g. a log path the daemon user couldn't write to).

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
4. **No privilege escalation** — run as a regular user, never root/admin. On macOS
   and Linux the daemon runs as the user who invoked the installer (via `sudo`),
   never as `root`, even though the installer itself must be run with `sudo` to
   write system-level config/service files. macOS logs live under
   `/Library/Logs/VectrifyRunner/` (owned by that user) rather than root-owned
   `/var/log/`, so the daemon can actually write its own log file. The config
   directory/file (`/etc/vectrify-runner/`) is likewise `chown`ed to that same
   user (mode 700/600) — otherwise the root-owned config from earlier installer
   versions is unreadable by the non-root service user and the daemon fails
   immediately with "permission denied" on startup.
5. **Key never logged** — `runner_key` is used only in the WebSocket URL; it is never written to log files.
6. **Browser gating** — `browser` commands share `allow_shell`'s gating (no separate `allow_browser` setting exists) — blocked at the runner level if `allow_shell=false`. Screenshot destinations are subject to the same path-containment rule as `file_op` (invariant #1). No separate API-side check exists for browser commands (unlike shell) — the runner is the sole enforcement point.

---

## Running as a system service

Use `install.ps1` (Windows) or `install.sh` (Linux/macOS) — they handle everything.

### Windows service account

The Windows service runs as the account that ran the installer by default (not
`LocalSystem`), matching Linux/macOS where the systemd/launchd service already
runs as `ORIGINAL_USER` rather than root. `install.ps1` prompts for which
account to run as (defaulting to the current user) and that account's
password, then:

1. Grants `SeServiceLogonRight` to the account via the LSA policy API
   (`Grant-ServiceLogonRight` in `install.ps1`) — regular user accounts do NOT
   have this by default; only interactive logon rights are implied by normal
   account creation, so `New-Service -Credential` alone is not sufficient —
   the service is created fine but fails to actually start until this right
   is granted.
2. Sets ACLs on `$InstallDir` (read+execute) and `$ConfigDir` (modify) for
   that account via `icacls`, mirroring what `install.sh` already does with
   `chown` for the Linux/macOS service user — both directories are created
   under `Program Files`/`ProgramData`, owned by Administrators by default.
3. Creates the service with `New-Service -Credential`.

**Password-rotation caveat (inherent to any Windows service run as a real user
account rather than a built-in one):** if that account's Windows password ever
changes later, the service will fail to start on next reboot until someone
re-enters the new password — either by re-running `install.ps1`, or manually
via `services.msc` → the service → Properties → Log On tab. This is documented
in the install summary output at the end of a successful install.

### Service lifecycle (Windows)

The binary uses `golang.org/x/sys/windows/svc` to detect whether it was launched
by the Windows SCM. When running as a service, `service_windows.go` implements
`svc.Handler` and handles `SERVICE_CONTROL_STOP` / `SHUTDOWN`. When running
interactively in a terminal, it falls back to SIGTERM/SIGINT handling as before.

### Service lifecycle (Linux / macOS)

`service_other.go` is a no-op stub — systemd and launchd both stop services by
sending SIGTERM, which `runInteractive()` in `main.go` already handles correctly.
No extra service-awareness is needed in the binary on these platforms.
