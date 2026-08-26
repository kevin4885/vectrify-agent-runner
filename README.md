# Vectrify Agent Runner

A lightweight daemon that connects your machine to Vectrify Cloud, allowing AI agents to read and write files, run shell commands, and perform git operations on your real local projects — without any cloud sandbox or remote VM.

---

## How it works

The runner makes a **single outbound WebSocket connection** to the Vectrify API. No ports are opened, no inbound connections are accepted. When an AI agent needs to edit a file or run a command, the API sends a command over that socket and the runner executes it locally and streams back the result.

---

## Installation

### Step 1 — Register a runner in the Vectrify UI

1. Go to **Settings → Runners → New Runner**
2. Give it a name (e.g. `dev-laptop`, `staging-server`)
3. Copy the `vrun_...` key — it is shown **exactly once and never again**

### Step 2 — Run the installer

**macOS / Linux — download the script, then run it with `sudo` as a separate step:**
```bash
curl -fsSLO https://github.com/kevin4885/vectrify-agent-runner/releases/latest/download/install.sh
sudo bash install.sh
```

> ⚠️ **Do not** run this as `sudo bash <(curl -fsSL ...)` or `curl ... | sudo bash`.
> - Process substitution (`<(...)`) breaks under `sudo` on macOS/Linux with `bash: /dev/fd/NN: Bad file descriptor` — `sudo` closes inherited file descriptors before the subshell can read from them.
> - Piping directly into `sudo bash` can hang or misbehave on the interactive prompts because `sudo`'s stdin is the pipe, not your terminal.
>
> Downloading first and running the local file with `sudo bash install.sh` avoids both problems and is the only supported method.

**Windows — open PowerShell as Administrator, then:**
```powershell
iwr -useb https://github.com/kevin4885/vectrify-agent-runner/releases/latest/download/install.ps1 | iex
```

The installer will ask for:
- **Workspace root folder** — the directory agents are allowed to work in (all file operations are confined here)
- **Runner key** — the `vrun_...` key from Step 1
- **Allow shell commands** — whether to permit `runner_shell` commands (default: no)

It then installs the binary, writes the config, and registers and starts a system service automatically.

### What gets installed

| | Windows | Linux | macOS |
|---|---|---|---|
| Binary | `C:\Program Files\VectrifyRunner\vectrify-runner.exe` | `/usr/local/bin/vectrify-runner` | `/usr/local/bin/vectrify-runner` |
| Config | `C:\ProgramData\VectrifyRunner\config.yaml` | `/etc/vectrify-runner/config.yaml` | `/etc/vectrify-runner/config.yaml` |
| Logs | `C:\ProgramData\VectrifyRunner\vectrify-runner.log` | `journalctl -u vectrify-runner` | `/Library/Logs/VectrifyRunner/vectrify-runner.log` |
| Service | Windows Service (`VectrifyRunner`) | systemd (`vectrify-runner`), runs as invoking user | launchd (`ai.vectrify.runner`), runs as invoking user |

On macOS and Linux the daemon runs as **the user who ran the installer** (not root), matching least-privilege — even with `allow_shell: true`, shell commands only ever run with that user's own permissions.

---

## Updating

Re-run the same installer commands from Step 2. If a config file already exists the installer detects it, skips all prompts, and just swaps the binary and restarts the service (on macOS this also repairs the launchd plist/log directory if a previous install left them in a broken state).

The runner also **updates itself automatically** — on startup and every 24 hours it checks GitHub for a newer release and applies it in the background with no user interaction needed.

---

## Managing the service

### Windows
```powershell
Start-Service   VectrifyRunner
Stop-Service    VectrifyRunner
Restart-Service VectrifyRunner
Get-Service     VectrifyRunner

# View logs
Get-Content "C:\ProgramData\VectrifyRunner\vectrify-runner.log" -Wait
```

### Linux
```bash
sudo systemctl start   vectrify-runner
sudo systemctl stop    vectrify-runner
sudo systemctl restart vectrify-runner
sudo systemctl status  vectrify-runner

# View logs
journalctl -u vectrify-runner -f
```

### macOS
```bash
sudo launchctl start  ai.vectrify.runner
sudo launchctl stop   ai.vectrify.runner

# View logs
tail -f /Library/Logs/VectrifyRunner/vectrify-runner.log
```

---

## Configuration reference

Config is written by the installer. To change a setting, edit the file and restart the service.

| Key | Required | Default | Description |
|---|---|---|---|
| `api_url` | ✓ | — | WebSocket URL, e.g. `wss://api.vectrify.ai/api/v1/runner/ws` |
| `runner_key` | ✓ | — | The `vrun_...` key from the Vectrify UI |
| `workspace_root` | ✓ | — | Absolute path — all file operations must stay inside this directory |
| `allow_shell` | | `false` | Set `true` to enable shell commands (bash on Linux/macOS, PowerShell on Windows) |
| `log_level` | | `info` | Verbosity: `debug` \| `info` \| `warn` \| `error` |
| `reconnect_max_backoff` | | `60` | Maximum seconds between reconnect attempts (exponential backoff) |
| `log_file` | | *(auto on Windows service)* | Path to write logs. Set automatically on Windows; on Linux/macOS the service manager captures stdout. |

### Command-line flags

| Flag | Default | Description |
|---|---|---|
| `--config` | platform default | Path to the YAML config file |

---

## Development

### Run against a local API

```powershell
# Windows
.\vectrify-runner.exe --config dev-config.yaml
```

```bash
# Linux / macOS
./vectrify-runner --config dev-config.yaml
```

With `dev-config.yaml`:
```yaml
api_url:        ws://localhost:11083/api/v1/runner/ws
runner_key:     vrun_YOUR_KEY_HERE
workspace_root: /home/yourname/projects
allow_shell:    true
log_level:      debug
```

### Build all platform binaries

Requires Go 1.22+. From Windows:
```powershell
.\build.ps1                  # builds to dist/, version 1.0.0
.\build.ps1 -Version 1.2.3   # stamp a specific version
```

Outputs:
```
dist/vectrify-runner-windows-amd64.exe
dist/vectrify-runner-linux-amd64
dist/vectrify-runner-linux-arm64
dist/vectrify-runner-darwin-amd64
dist/vectrify-runner-darwin-arm64
```

### Release

```bash
git tag v1.0.0
git push origin v1.0.0
```

GitHub Actions builds all 5 binaries and publishes them as a GitHub Release automatically. The auto-updater in every installed runner will pick up the new version within 24 hours.

---

## Troubleshooting

**`bash: /dev/fd/NN: Bad file descriptor` when installing**
You ran the installer as `sudo bash <(curl -fsSL ...)`. Process substitution doesn't survive `sudo`'s fd handling. Download the script first, then run it: `curl -fsSLO .../install.sh && sudo bash install.sh` (see Step 2 above).

**Install hangs or seems to do nothing after `curl ... | sudo bash`**
Piping directly into `sudo bash` puts the pipe (not your terminal) on `sudo`'s stdin, which can hang the interactive prompts. Use the two-step download-then-run pattern instead.

**macOS: `launchctl print system/ai.vectrify.runner` shows `last exit code = 78: EX_CONFIG` and no log file appears**
This means launchd could not start the process at all — typically because the configured log path wasn't writable by the daemon's user (this affected installs from versions of `install.sh` predating the `/Library/Logs/VectrifyRunner` fix, which pointed at root-owned `/var/log`). Re-run the installer — the update path now repairs the plist and log directory automatically on every run:
```bash
curl -fsSLO https://github.com/kevin4885/vectrify-agent-runner/releases/latest/download/install.sh
sudo bash install.sh
```

**`runner_key must start with 'vrun_'`**
Re-copy the key from Settings → Runners in the Vectrify UI.

**`Unauthorized` on connect**
The key was deleted or the runner was deactivated. Create a new runner and update `runner_key` in the config file.

**`path is outside the workspace root`**
The agent tried to access a file outside `workspace_root`. Widen `workspace_root` in the config to cover all the directories you want the agent to reach.

**Shell commands fail on Windows**
Verify PowerShell is available: `powershell -Command "Get-Host"`. The runner uses `powershell -NoProfile -NonInteractive -Command "..."` internally.

**Connection keeps dropping**
Set `log_level: debug` and check the logs. Verify outbound port 443 to `api.vectrify.ai` is not blocked by a firewall or proxy.

**No log file on Windows**
The log file is created on first run. If it never appears, the service failed to start — check `Get-Service VectrifyRunner` and look for errors in the Windows Event Viewer under Application logs.

---

## Security

| Guarantee | How |
|---|---|
| **Path containment** | Every file path is resolved to absolute, then checked against `workspace_root` before any I/O. Rejects `../` traversal. |
| **Shell gating** | Shell execution is blocked unless `allow_shell: true` in config. The API also enforces this independently. |
| **Outbound only** | The runner makes one outbound WebSocket connection. No ports are listened on. |
| **Run as regular user** | The service runs as `LocalSystem` on Windows and as the installing user on Linux/macOS — never as a privileged superuser beyond what the installer requires. |
| **Key never logged** | The `runner_key` is used only in the WebSocket URL query string and is never written to log output. |
