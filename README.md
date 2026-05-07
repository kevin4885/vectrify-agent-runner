# Vectrify Agent Runner

A lightweight daemon that connects your machine to Vectrify Cloud, allowing AI agents to read and write files, run shell commands, and perform git operations on your real local projects — without any cloud sandbox or remote VM.

---

## How it works

The runner makes a **single outbound WebSocket connection** to the Vectrify API. No ports are opened, no inbound connections are accepted. When an AI agent needs to edit a file or run a command, the API sends a command over that socket and the runner executes it locally and streams back the result.

---

## Requirements

| Requirement | Notes |
|---|---|
| **Go 1.22+** | Only needed to build from source. Download from [go.dev/dl](https://go.dev/dl/) |
| **Git** | Must be on `PATH`. Required for `runner_git` operations. |
| **bash** (Linux/macOS) or **PowerShell** (Windows) | Required only if `allow_shell: true` |
| Network access to `api.vectrify.ai` | Outbound HTTPS/WSS on port 443 |

---

## Quickstart

### Step 1 — Install Go (if you don't have it)

**macOS / Linux:**
```bash
# macOS with Homebrew
brew install go

# Linux — download and extract
wget https://go.dev/dl/go1.22.5.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.22.5.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin   # add to ~/.bashrc or ~/.zshrc
```

**Windows (PowerShell):**
```powershell
# With winget
winget install GoLang.Go

# Or download the MSI from https://go.dev/dl/ and run the installer.
# Then open a new terminal — go should be on PATH automatically.
```

Verify: `go version`

---

### Step 2 — Clone and build

**Linux / macOS:**
```bash
git clone https://github.com/vectrify/vectrify-agent-runner.git
cd vectrify-agent-runner
go mod tidy
go build -o vectrify-runner .
```

**Windows (PowerShell):**
```powershell
git clone https://github.com/vectrify/vectrify-agent-runner.git
cd vectrify-agent-runner
go mod tidy
go build -o vectrify-runner.exe .
```

---

### Step 3 — Register a runner in the Vectrify UI

1. Go to **Settings → Runners → New Runner**
2. Give it a name (e.g. `dev-laptop`, `staging-server`)
3. Copy the `vrun_...` key — it is shown **exactly once and never again**

---

### Step 4 — Create the config file

**Linux / macOS:**
```bash
mkdir -p ~/.vectrify-runner
cat > ~/.vectrify-runner/config.yaml << 'EOF'
api_url:        wss://api.vectrify.ai/api/v1/runner/ws
runner_key:     vrun_YOUR_KEY_HERE
workspace_root: /home/yourname/projects
allow_shell:    false
EOF
```

**Windows (PowerShell):**
```powershell
New-Item -ItemType Directory -Force "$env:USERPROFILE\.vectrify-runner"
@"
api_url:        wss://api.vectrify.ai/api/v1/runner/ws
runner_key:     vrun_YOUR_KEY_HERE
workspace_root: C:\Users\yourname\projects
allow_shell:    false
"@ | Set-Content "$env:USERPROFILE\.vectrify-runner\config.yaml"
```

> **workspace_root** must be an absolute path. All file operations are confined to this directory — the runner rejects any path that tries to escape it.

---

### Step 5 — Run

**Linux / macOS:**
```bash
./vectrify-runner
```

**Windows:**
```powershell
.\vectrify-runner.exe
```

You should see output like:
```
time=2026-05-07T14:00:00Z level=INFO msg="vectrify agent runner starting" version=dev platform=linux workspace_root=/home/yourname/projects allow_shell=false
time=2026-05-07T14:00:01Z level=INFO msg=connecting url=wss://api.vectrify.ai/api/v1/runner/ws attempt=1
time=2026-05-07T14:00:01Z level=INFO msg=registered runner_id=42
```

The runner will reconnect automatically if the connection drops.

---

## Configuration reference

The config file defaults to `~/.vectrify-runner/config.yaml` on all platforms.  Use `--config /path/to/config.yaml` to point to a different file.

| Key | Required | Default | Description |
|---|---|---|---|
| `api_url` | ✓ | — | WebSocket URL, e.g. `wss://api.vectrify.ai/api/v1/runner/ws` |
| `runner_key` | ✓ | — | The `vrun_...` key from the Vectrify UI |
| `workspace_root` | ✓ | — | Absolute path — all file operations must stay inside this directory |
| `allow_shell` | | `false` | Set `true` to enable `runner_shell` commands (bash on Linux/macOS, PowerShell on Windows) |
| `log_level` | | `info` | Verbosity: `debug` \| `info` \| `warn` \| `error` |
| `reconnect_max_backoff` | | `60` | Maximum seconds between reconnect attempts (uses exponential backoff) |

---

## Command-line flags

```
vectrify-runner [--config /path/to/config.yaml]
```

| Flag | Default | Description |
|---|---|---|
| `--config` | `~/.vectrify-runner/config.yaml` | Path to the YAML config file |

---

## Building for distribution (cross-compilation)

Go makes it trivial to build a single binary for any platform from any machine.
Use `-ldflags` to stamp the version into the binary.

```bash
# From any OS — set GOOS and GOARCH to target platform

# Linux (64-bit Intel/AMD)
GOOS=linux   GOARCH=amd64   go build -ldflags "-X vectrify/agent-runner/config.Version=1.0.0" -o dist/vectrify-runner-linux-amd64 .

# Linux (ARM64 — e.g. AWS Graviton, Raspberry Pi 4)
GOOS=linux   GOARCH=arm64   go build -ldflags "-X vectrify/agent-runner/config.Version=1.0.0" -o dist/vectrify-runner-linux-arm64 .

# macOS (Apple Silicon M1/M2/M3)
GOOS=darwin  GOARCH=arm64   go build -ldflags "-X vectrify/agent-runner/config.Version=1.0.0" -o dist/vectrify-runner-darwin-arm64 .

# macOS (Intel)
GOOS=darwin  GOARCH=amd64   go build -ldflags "-X vectrify/agent-runner/config.Version=1.0.0" -o dist/vectrify-runner-darwin-amd64 .

# Windows (64-bit)
GOOS=windows GOARCH=amd64   go build -ldflags "-X vectrify/agent-runner/config.Version=1.0.0" -o dist/vectrify-runner-windows-amd64.exe .
```

**From Windows PowerShell:**
```powershell
$env:GOOS = "linux"; $env:GOARCH = "amd64"
go build -ldflags "-X vectrify/agent-runner/config.Version=1.0.0" -o dist/vectrify-runner-linux-amd64 .
Remove-Item Env:\GOOS, Env:\GOARCH
```

The resulting binaries are fully self-contained — no Go runtime or any other dependency is needed on the target machine.

---

## Running as a background service

### Linux — systemd

```bash
# Copy binary
sudo cp vectrify-runner /usr/local/bin/vectrify-runner
sudo chmod +x /usr/local/bin/vectrify-runner

# Create service file (replace 'youruser' with your actual username)
sudo tee /etc/systemd/system/vectrify-runner.service << 'EOF'
[Unit]
Description=Vectrify Agent Runner
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/vectrify-runner
Restart=always
RestartSec=5
User=youruser
WorkingDirectory=/home/youruser
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable vectrify-runner
sudo systemctl start  vectrify-runner

# Check status
sudo systemctl status vectrify-runner
journalctl -u vectrify-runner -f
```

### macOS — launchd

```bash
cp vectrify-runner /usr/local/bin/vectrify-runner

# Create plist (replace 'youruser' with your actual username)
cat > ~/Library/LaunchAgents/ai.vectrify.runner.plist << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>ai.vectrify.runner</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/vectrify-runner</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/vectrify-runner.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/vectrify-runner.log</string>
</dict>
</plist>
EOF

launchctl load ~/Library/LaunchAgents/ai.vectrify.runner.plist

# Check status
launchctl list | grep vectrify
tail -f /tmp/vectrify-runner.log
```

### Windows — Task Scheduler (runs at login)

```powershell
# Copy binary somewhere permanent
Copy-Item .\vectrify-runner.exe "C:\Program Files\VectrifyRunner\vectrify-runner.exe"

# Register a scheduled task that starts at logon and restarts on failure
$action  = New-ScheduledTaskAction -Execute "C:\Program Files\VectrifyRunner\vectrify-runner.exe"
$trigger = New-ScheduledTaskTrigger -AtLogOn
$settings = New-ScheduledTaskSettingsSet -RestartCount 99 -RestartInterval (New-TimeSpan -Minutes 1)
Register-ScheduledTask `
    -TaskName "VectrifyRunner" `
    -Action   $action `
    -Trigger  $trigger `
    -Settings $settings `
    -RunLevel Limited `
    -Force

# Start it now without waiting for next login
Start-ScheduledTask -TaskName "VectrifyRunner"

# Check it's running
Get-ScheduledTask -TaskName "VectrifyRunner" | Select-Object TaskName, State
```

---

## Development workflow

### Run locally against a dev API

```bash
# Point to your local dev API
cat > ~/.vectrify-runner/config.yaml << 'EOF'
api_url:        ws://localhost:11083/api/v1/runner/ws
runner_key:     vrun_YOUR_KEY_HERE
workspace_root: /home/yourname/projects
allow_shell:    true
log_level:      debug
EOF

./vectrify-runner
```

### Run with a custom config path

```bash
./vectrify-runner --config ./my-test-config.yaml
```

### Rebuild and restart quickly

```bash
go build -o vectrify-runner . && ./vectrify-runner
```

---

## Troubleshooting

**`runner_key must start with 'vrun_'`**
The key in config.yaml must start with `vrun_`. Re-copy it from the Vectrify UI.

**`Unauthorized` on connect**
The key may have been deleted or the runner was deactivated in the Vectrify UI. Create a new runner and update the key in your config.

**`path is outside the workspace root`**
The AI agent tried to access a file outside `workspace_root`. Check the path and ensure `workspace_root` covers the project directories you want the agent to access.

**Shell commands fail on Windows**
Ensure PowerShell is available: `powershell -Command "Get-Host"`. The runner uses `powershell -NoProfile -NonInteractive -Command "..."` internally.

**Connection keeps dropping**
Enable debug logging (`log_level: debug`) to see the exact error. Check that port 443 outbound to `api.vectrify.ai` is not blocked by a firewall or corporate proxy.

**`go: command not found` when building**
Go is not installed or not on PATH. See [Step 1](#step-1--install-go-if-you-dont-have-it) above.

---

## Security

| Guarantee | How |
|---|---|
| **Path containment** | Every file path is resolved to absolute, then checked against `workspace_root` before any I/O. Rejects `../` traversal. |
| **Shell gating** | Shell execution is blocked unless `allow_shell: true` in config. The API also enforces this independently. |
| **Outbound only** | The runner makes one outbound WebSocket connection. No ports are listened on. |
| **Run as regular user** | Never run as root or Administrator. The runner has only the filesystem permissions of your user account. |
| **Key never logged** | The `runner_key` appears only in the WebSocket URL query string. It is never written to log output. |
