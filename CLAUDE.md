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
        runner_browser                        ├─ browser   (Playwright automation, opt-in)
        runner_process                        └─ process   (detached background processes, opt-in)
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
├── updater/
│   ├── updater.go         Background auto-update loop: checks GitHub releases hourly, downloads +
│   │                      verifies the new binary, then swaps it (Windows: in-process; Linux/macOS: script; see
│   │                      "Auto-update" below — has a subtle cross-platform gotcha, read before touching)
│   ├── lock.go            Cross-process update lock (prevents two runner processes on the same
│   │                      machine from swapping the binary at the same time)
│   ├── apply_windows.go   Windows: in-process rename swap, no helper script (build tag: windows)
│   ├── swap.go            Windows swap: rename running exe -> exe.old, move new exe in, roll back on failure
│   └── apply_other.go     Linux/macOS: systemd/launchd-aware swap (build tag: !windows)
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
| Stealth evasions | vendored `executor/stealth.min.js` (sourced from jonfriesen/playwright-go-stealth, not a Go dependency — see below) |

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
{ "cmd_id": "uuid", "type": "process", "action": "start", "process_id": "p1", "command": "npm run dev", "working_dir": "..." }
{ "cmd_id": "uuid", "type": "process", "action": "stop",  "process_id": "p1" }
{ "cmd_id": "uuid", "type": "process", "action": "list" }
{ "cmd_id": "uuid", "type": "process", "action": "logs",  "process_id": "p1", "tail_lines": 100 }
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

**process notes:**
- Gated by `allow_shell` — same reasoning as `browser`: there is deliberately
  no separate `allow_process` setting.
- Exists specifically because a `shell` command CANNOT support "start a
  long-lived process, then interact with it from a later, separate call" —
  `executor/shell.go` kills its entire process tree unconditionally the
  moment the starting command returns, even on a clean exit (see that
  file's `ROOT-CAUSE NOTE` and `PRODUCT DECISION` comments — this is
  deliberate for `shell`'s own request/response contract, not a bug). A
  `process` command's whole reason to exist is to NOT do that.
- `process_id` is caller-supplied and identifies one tracked process (like
  `browser`'s `session_id`), capped at `max_background_processes`,
  automatically reaped after `background_process_max_age_seconds` if never
  explicitly stopped (safety net for a forgotten `stop`, not a normal code
  path — see "Background processes" below for the intended lifecycle).
- Actions: `start` (`command` interpreted the same way `shell`'s `command`
  is — `bash -c` / `powershell -Command`; `working_dir` defaults to
  `workspace_root`; fails fast if the process exits with a non-zero code
  within ~300ms of starting, e.g. a bad executable or syntax error — a
  process that starts fine and exits later, even seconds later, is not a
  `start`-time error), `stop` (kills the whole tracked process tree if the
  tracker attached successfully, otherwise just the direct child; idempotent
  — stopping an unknown/already-stopped `process_id` is not an error), `list`
  (every tracked process, running or recently exited, with PID/command/
  working_dir/started_at and, once exited, exit code), `logs` (tail of
  combined stdout+stderr from the process's log file; `tail_lines` <= 0
  returns everything, capped at 200000 bytes from the end).
- Classified `heavy` in `client/classify.go` (same reasoning as `shell` — a
  process `start` is exactly the kind of external-process-spawn action that
  sub-limit exists to bound).

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
allow_shell:          true              # default true; set false to disable runner_shell commands
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
max_background_processes: 5             # max concurrent detached background processes (see
                                         # "Background processes" below)
background_process_max_age_seconds: 3600 # auto-stop/clean-up a background process (running or
                                         # already exited) after this many seconds, if never
                                         # explicitly stopped or retrieved
```

All three concurrency knobs are optional; the defaults shown above match the
hardcoded behavior from before they became configurable, so an existing
config.yaml with none of these keys set behaves identically. The
`max_browser_sessions`/`browser_idle_timeout_seconds`/`browser_headless` and
`max_background_processes`/`background_process_max_age_seconds` keys are
likewise all optional — an existing config.yaml with none of them set uses
the defaults shown above and behaves identically. **`allow_shell` defaults to `true`** when the key is absent (`Load` seeds `Config{AllowShell: true}` before unmarshalling); an explicit `allow_shell: false` opts out. The installers' prompt also defaults to yes.
There is no
`allow_browser` or `allow_process` key: both command types are gated by
`allow_shell` (see "Browser automation" and "Background processes" below).

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
- injects the evasion script vendored at `executor/stealth.min.js` (unmodified,
  sourced from
  [`github.com/jonfriesen/playwright-go-stealth`](https://github.com/jonfriesen/playwright-go-stealth)
  v0.0.3 — the extracted `puppeteer-extra-plugin-stealth` evasions; see
  `executor/stealth.min.js.LICENSE`) into every new browser context via
  `ctx.AddInitScript(...)`, so it also covers popups and `target=_blank` pages
  opened later in that context

**This raises the bar against basic/medium bot detection — it is NOT a guarantee
against advanced systems** (Cloudflare Turnstile with behavioral scoring, Akamai,
PerimeterX/DataDome). Those fingerprint TLS/JA3, canvas/audio noise, and mouse/timing
behavior in ways a generic stealth layer cannot fully spoof, and it is a permanent
cat-and-mouse game with no guarantee either way.

**Implementation note (vendored, not imported):** `stealth.min.js` is vendored
directly via `//go:embed` rather than importing `github.com/jonfriesen/playwright-go-stealth`
as a Go dependency. That package itself depends on the older
`github.com/playwright-community/playwright-go` module path, which Go treats as a
completely different type identity than `github.com/mxschmitt/playwright-go` (same
upstream project, renamed on GitHub over time — the code lineage is identical but the
two module paths are NOT interchangeable to the Go compiler; the newer module path is
required here because the driver version pinned by the old path's latest tagged release,
`v0.4201.1`, points at Playwright driver binaries Microsoft no longer hosts —
`playwright.Install()` against it 404s). Vendoring the JS file directly and calling
`AddInitScript` ourselves reaches the exact same runtime behavior as that package's own
`Inject()` helper, with one fewer dependency (and its transitive sub-dependencies —
`go-jose`, `go.uber.org/multierr`, `golang.org/x/exp` — previously compiled into the
binary purely to reach one embedded string) and no module-path caveat to carry forward.
To pick up an upstream update to the evasion script, re-copy `stealth.min.js` from a
newer `playwright-go-stealth` release — there is no dependency to bump.

---

## Background processes

Gated by `allow_shell` — there is deliberately no separate `allow_process`
setting (same reasoning as browser automation above). The `process` command
type lets the API start a long-lived process on the runner machine that
survives PAST the single command that started it — e.g. a dev server —
so it can be driven or verified by later, separate commands (most usefully
`browser` ones) issued minutes apart, in a completely different tool call.

**Why this needed its own command type instead of a `shell` flag:**
`executor/shell.go` is built around one hard invariant — a shell command
must always return, which it guarantees by killing the ENTIRE process tree
the instant the command's own process exits or times out (see `shell.go`'s
`ROOT-CAUSE NOTE`). That is not a limitation to work around; it is what
makes `shell.go` safe to use as a blocking request/response primitive at
all. A "leave it running" flag on `shell` would need a fundamentally
different output-handling strategy (no live pipe to stream — the whole
point is the descendant survives, so the pipe never reaches EOF) and would
give `runner_shell` two contradictory contracts instead of one clear one.
`process.go` reuses the exact same process-tree tracker (`procTreeIface` /
`newProcTreeFn` — see `proc_windows.go` / `proc_other.go`) `shell.go` uses,
but only ever calls `kill()` from an explicit `stop`, the max-age reaper, or
runner `Shutdown()` — never automatically just because the *starting* call
returned.

**Typical workflow:** `process` `start` a dev server → `browser` `goto`/
`click`/`screenshot` against it, any number of times, across any number of
separate tool calls → `process` `stop` when done. See
`app/engine/agent_engine/runner.py`'s system-prompt block in `vectrify-api`
for how this is described to the LLM.

**Lifecycle:**
- A background process's lifetime is bounded by the RUNNER's own lifetime,
  not left to survive it: `Runner.Shutdown()` kills every tracked process
  (see `ProcessManager.Shutdown`) — including on the routine auto-update
  restart cycle (see "Auto-update" above). There is no "survive a runner
  restart" mode; a caller must re-`start` after one.
- A forgotten `start` (no matching `stop`) is not permanently leaked: the
  reaper force-stops (if still running) or cleans up (if already exited on
  its own) any tracked process past `background_process_max_age_seconds`.
  Defaults to a long window (1 hour) since this is meant for multi-step
  workflows spread over real time, not a tight idle timeout like browser
  sessions'.
- Output is captured to a log file (stdout+stderr combined, in the order
  written) under a runner-internal temp directory — NOT under
  `workspace_root`, and not exposed as a path the caller operates on
  directly (unlike a browser screenshot's `path`). Retrieve it only via the
  `logs` action.

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

**Windows build flags matter for antivirus (do not "optimize" them away).** The Windows exe is
built WITHOUT `-s -w` (stripped symbols make an unsigned Go binary look opaque to Microsoft
Defender's ML classifier 

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
5 platform binaries, generates `checksums.txt` for them, and publishes them all —
plus `install.sh` and `install.ps1` — as assets on the GitHub Release. The one-liner
install commands always pull from `releases/latest/download/` so users get the
newest version automatically. `checksums.txt` is not optional: `updater/apply_*.go`
refuses to install any release that doesn't have one (see "Auto-update" below).

---

## Auto-update

Every installed runner (`updater.Start`, called from `main.go`/`service_windows.go`)
checks GitHub for a newer release on startup and hourly thereafter, and self-updates
in the background with no user interaction. High level: download the new binary,
verify its SHA256 against `checksums.txt`, drain in-flight commands, then swap the binary
and restart the service. **Windows does the swap in-process** (`updater/swap.go`: rename
the running exe to `exe.old`, move the verified new file to `exe`, exit, let the SCM
failure/restart policy relaunch it) with **no helper script**. Linux/macOS still hand
off to a detached bash script that swaps the binary and restarts the service.

**Windows swap 

**The subtle, previously-production-breaking gotcha — read this before touching
`updater/*.go`:** `install.ps1` configures Windows SCM restart-on-failure
(`sc.exe failure ... actions=restart/...`); `install.sh` configures
`Restart=always` (systemd) / `KeepAlive=true` (launchd). All three mean *"if this
process ever exits without the supervisor itself having caused it, treat it as a
crash and restart the (still-old) binary."* If `apply()` just calls `os.Exit(0)`
directly to hand off to its swap script — which is exactly what it used to do —
every one of those supervisors treats that as a crash, restarts the old binary
within seconds, and that freshly-restarted process's own startup update check
immediately detects the same new release and starts a **second, fully independent
update flow** — racing the first flow's still-running swap script over the exact
same file paths. This happened in production (`v1.0.17`) and corrupted the
installed Windows binary (`... is not a valid Win32 application` on next launch),
because both flows' `os.Create()` (truncates) and `Move-Item`/`mv` calls interleaved
with zero coordination.

Two independent, complementary fixes, both required — removing either one
reopens this bug:

1. **`updater/lock.go`** — a cross-process, atomic (`O_CREATE|O_EXCL`) lock file
   next to the binary. A second `checkAndApply()` — however it got started — that
   finds the lock already held (and not stale — see `staleLockAge`) skips its
   update cycle entirely instead of racing the first. This is the guarantee that
   holds *even if* the platform-specific mitigation below is ever imperfect on some
   OS/supervisor version.
2. **`updater/apply_other.go` / `apply_windows.go`** — the platform-specific exit sequence. On Linux/macOS,
   before the terminal `os.Exit(0)`, `apply()` makes the *supervisor itself* the cause of this
   process's exit, instead of just exiting and hoping the supervisor doesn't notice:
   - **Windows** (different mechanism, no helper script): the binary is already swapped
     in-process (`swap.go`) *before* the exit, so `apply()` exits non-cleanly (`os.Exit(1)`,
     no `SERVICE_STOPPED` reported) and the SCM's `sc.exe failure ... restart` policy
     relaunches the service from `exePath` = the new binary. The old `sc.exe stop` trick
     is gone: it existed to make the old binary's exit look requested so it would not be
     restarted *before* the script swapped it; with the swap done first, a restart is exactly
     what we want. `Runner.Shutdown` (browser cleanup) is passed in as `beforeExit`
     because the SCM Stop handler that used to run it no longer fires.
   - **Linux (systemd)**: runs `systemctl stop vectrify-runner`. Per
     `systemd.service(5)`: *"When the death of the process is a result of systemd
     operation (e.g. service stop or restart), the service will not be
     restarted"* — regardless of `Restart=always`. `systemctl stop` sends SIGTERM
     to this process, the existing signal handler in `main.go`'s `runInteractive`
     already handles that correctly (calls `Runner.Shutdown()`, then exits), and
     because systemd itself initiated it, no restart follows.
   - **macOS (launchd)**: **different rule from systemd** — a boolean
     `KeepAlive=true` job restarts unconditionally on *any* exit, including one
     caused by `launchctl stop`; there is no "this was requested" exception for
     the boolean form. The only way to prevent the restart is `launchctl unload`
     (removes the job from supervision entirely) instead of `stop`. `apply_other.go`
     branches on `runtime.GOOS` for exactly this reason — don't unify the Linux and
     macOS paths, they need genuinely different supervisor calls.
+— `apply()`
     must never hang indefinitely either.

Also note: the temp download path is unique per attempt
(`exePath + ".new." + pid + "." + timestamp"`, not a fixed `.new` suffix) as
defense-in-depth — even with the lock, a fixed shared path is one less thing that
has to go right for two attempts to never collide on the same file.

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
6. **Browser gating** — `browser` commands share `allow_shell`'s gating (no separate `allow_browser` setting exists) — blocked at the runner level if `allow_shell=false`. Screenshot *writes* are subject to the same path-containment rule as `file_op` (invariant #1). Browser *reads* are not similarly contained: `goto` only accepts `http`/`https` URLs (rejecting `file:`, `data:`, `chrome:`, etc. outright — see executor/browser.go's `validateGotoURL`), but an allowed `http(s)` URL can still reach loopback/link-local addresses (e.g. cloud metadata endpoints) the same way `curl` could under `allow_shell: true` — this is bounded by the same shell-level trust as everything else here, not by `workspace_root`. No separate API-side check exists for browser commands (unlike shell) — the runner is the sole enforcement point.
7. **Process gating** — `process` commands share `allow_shell`'s gating (no separate `allow_process` setting exists), same reasoning as invariant #6. A `process start` command is, in effect, an unattended `shell` command whose lifetime outlives the call that issued it — not a broader capability than `shell` already grants, just a longer-lived instance of it. `working_dir` is NOT path-contained (matches `shell`'s own `working_dir`, which isn't either) — both are already gated by the same full local-machine trust `allow_shell` implies.

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
