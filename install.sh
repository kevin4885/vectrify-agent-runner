#!/usr/bin/env bash
# install.sh - Install Vectrify Agent Runner as a system service
#
# Linux  : systemd  (runs at boot as a system service)
# macOS  : launchd  (runs at boot as a LaunchDaemon)
#
# Usage:
#   sudo ./install.sh

set -euo pipefail

# When piped (e.g. curl ... | bash) stdin is the pipe, not the terminal.
# Redirect to /dev/tty so interactive prompts work correctly.
if [ ! -t 0 ] && [ -e /dev/tty ]; then
    exec < /dev/tty
fi

GITHUB_REPO="vectrify/vectrify-agent-runner"

# ── Platform detection ────────────────────────────────────────────────────────
OS="$(uname -s)"
ARCH="$(uname -m)"

case "$OS" in
    Linux)  PLATFORM="linux"  ;;
    Darwin) PLATFORM="darwin" ;;
    *)
        echo "ERROR: Unsupported platform: $OS" >&2
        exit 1
        ;;
esac

case "$ARCH" in
    x86_64)        GO_ARCH="amd64" ;;
    aarch64|arm64) GO_ARCH="arm64" ;;
    *)
        echo "ERROR: Unsupported architecture: $ARCH" >&2
        exit 1
        ;;
esac

# ── Root check ────────────────────────────────────────────────────────────────
if [ "$(id -u)" -ne 0 ]; then
    echo "ERROR: This installer must be run as root. Use: sudo ./install.sh" >&2
    exit 1
fi

# The user who called sudo (used for systemd User= directive).
ORIGINAL_USER="${SUDO_USER:-$(logname 2>/dev/null || echo "")}"

INSTALL_BIN="/usr/local/bin/vectrify-runner"
CONFIG_DIR="/etc/vectrify-runner"
CONFIG_FILE="$CONFIG_DIR/config.yaml"

# ── Banner ────────────────────────────────────────────────────────────────────
echo ""
echo "  Vectrify Agent Runner - Installer"
echo "  Platform : $OS / $ARCH"
echo ""

# ── Locate binary ─────────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CANDIDATES=(
    "$SCRIPT_DIR/vectrify-runner"
    "$SCRIPT_DIR/dist/vectrify-runner-${PLATFORM}-${GO_ARCH}"
)

SRC=""
for c in "${CANDIDATES[@]}"; do
    if [ -f "$c" ]; then SRC="$c"; break; fi
done

DOWNLOADED=false
if [ -z "$SRC" ]; then
    ASSET="vectrify-runner-${PLATFORM}-${GO_ARCH}"
    DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/latest/download/${ASSET}"
    TMP_BIN="$(mktemp)"

    printf "  Downloading %s..." "$ASSET"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$DOWNLOAD_URL" -o "$TMP_BIN"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO "$TMP_BIN" "$DOWNLOAD_URL"
    else
        echo ""
        echo "  ERROR: curl or wget is required." >&2
        exit 1
    fi
    chmod +x "$TMP_BIN"
    SRC="$TMP_BIN"
    DOWNLOADED=true
    echo " done"
fi

echo "  Binary : $SRC"
echo ""

# ── Prompt helpers ────────────────────────────────────────────────────────────
read_required() {
    local label="$1"
    local val=""
    while [ -z "$val" ]; do
        read -rp "  $label: " val
        val="$(echo "$val" | xargs 2>/dev/null || echo "$val")"
        [ -z "$val" ] && echo "  This field is required."
    done
    echo "$val"
}

read_with_default() {
    local label="$1"
    local default="$2"
    local val=""
    read -rp "  $label [$default]: " val
    val="$(echo "$val" | xargs 2>/dev/null || echo "$val")"
    echo "${val:-$default}"
}

read_yesno() {
    local label="$1"
    local default="$2"   # "y" or "n"
    local hint
    hint=$([ "$default" = "y" ] && echo "Y/n" || echo "y/N")
    while true; do
        local val=""
        read -rp "  $label [$hint]: " val
        val="$(echo "${val:-$default}" | tr '[:upper:]' '[:lower:]' | xargs 2>/dev/null || echo "${val:-$default}")"
        case "$val" in
            y|yes) echo "true";  return ;;
            n|no)  echo "false"; return ;;
            *)     echo "  Please enter y or n." ;;
        esac
    done
}

read_choice() {
    local label="$1"
    local choices="$2"   # space-separated
    local default="$3"
    while true; do
        local val=""
        read -rp "  $label ($choices) [$default]: " val
        val="$(echo "${val:-$default}" | xargs 2>/dev/null || echo "${val:-$default}")"
        if echo " $choices " | grep -q " $val "; then
            echo "$val"; return
        fi
        echo "  Choose one of: $choices"
    done
}

# ── Collect configuration ─────────────────────────────────────────────────────
echo "  Configure the runner:"
echo ""

# workspace_root
while true; do
    WORKSPACE_ROOT="$(read_required "Workspace root folder path")"
    if [ -d "$WORKSPACE_ROOT" ]; then break; fi
    echo "  Directory not found: $WORKSPACE_ROOT"
    local_create=""
    read -rp "  Create it? [y/N]: " local_create
    local_create="$(echo "${local_create:-n}" | tr '[:upper:]' '[:lower:]')"
    if [ "$local_create" = "y" ]; then mkdir -p "$WORKSPACE_ROOT"; break; fi
done

# runner_key
while true; do
    RUNNER_KEY="$(read_required "Runner key (vrun_...)")"
    if echo "$RUNNER_KEY" | grep -qE '^vrun_.+'; then break; fi
    echo "  Key must start with 'vrun_'"
done

# allow_shell
ALLOW_SHELL="$(read_yesno "Allow shell commands?" "n")"

# log_level
LOG_LEVEL="$(read_choice "Log level" "info debug warn error" "info")"

# reconnect_max_backoff
while true; do
    BACKOFF="$(read_with_default "Max reconnect backoff in seconds" "60")"
    if echo "$BACKOFF" | grep -qE '^[0-9]+$' && [ "$BACKOFF" -gt 0 ]; then break; fi
    echo "  Must be a positive integer."
done

KEY_PREVIEW="${RUNNER_KEY:0:8}..."

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "  ----------------------------------------"
echo "  workspace_root : $WORKSPACE_ROOT"
echo "  runner_key     : $KEY_PREVIEW"
echo "  allow_shell    : $ALLOW_SHELL"
echo "  log_level      : $LOG_LEVEL"
echo "  backoff        : $BACKOFF s"
echo "  install path   : $INSTALL_BIN"
echo "  config file    : $CONFIG_FILE"
echo "  ----------------------------------------"
echo ""

read -rp "  Proceed with install? [Y/n]: " confirm
confirm="$(echo "${confirm:-y}" | tr '[:upper:]' '[:lower:]')"
if [ "$confirm" != "y" ]; then echo "  Aborted."; exit 0; fi
echo ""

# ── Step 1: Install binary ────────────────────────────────────────────────────
printf "  [1/4] Installing binary..."
install -m 755 "$SRC" "$INSTALL_BIN"
echo " done"

# ── Step 2: Write config ──────────────────────────────────────────────────────
printf "  [2/4] Writing config..."
mkdir -p "$CONFIG_DIR"
chmod 750 "$CONFIG_DIR"

cat > "$CONFIG_FILE" <<EOF
api_url:               wss://api.vectrify.ai/api/v1/runner/ws
runner_key:            $RUNNER_KEY
workspace_root:        $WORKSPACE_ROOT
allow_shell:           $ALLOW_SHELL
log_level:             $LOG_LEVEL
reconnect_max_backoff: $BACKOFF
EOF

chmod 640 "$CONFIG_FILE"
echo " done"

# ── Step 3: Register service ──────────────────────────────────────────────────
if [ "$PLATFORM" = "linux" ]; then

    printf "  [3/4] Installing systemd service..."

    # Determine User= for the unit — prefer the invoking user, fall back to root.
    UNIT_USER="${ORIGINAL_USER:-root}"

    cat > /etc/systemd/system/vectrify-runner.service <<EOF
[Unit]
Description=Vectrify Agent Runner
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$INSTALL_BIN --config $CONFIG_FILE
Restart=always
RestartSec=5
User=$UNIT_USER
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    echo " done"

    printf "  [4/4] Enabling and starting service..."
    systemctl enable vectrify-runner --quiet
    systemctl restart vectrify-runner
    echo " done"

    echo ""
    systemctl status vectrify-runner --no-pager -l
    echo ""
    echo "  Manage with:"
    echo "    sudo systemctl start   vectrify-runner"
    echo "    sudo systemctl stop    vectrify-runner"
    echo "    sudo systemctl restart vectrify-runner"
    echo "    journalctl -u vectrify-runner -f"

elif [ "$PLATFORM" = "darwin" ]; then

    PLIST_PATH="/Library/LaunchDaemons/ai.vectrify.runner.plist"
    # Determine UserName for the plist — prefer the invoking user, fall back to root.
    PLIST_USER="${ORIGINAL_USER:-root}"

    printf "  [3/4] Installing launchd daemon..."

    # Unload any existing instance before replacing the plist.
    launchctl unload "$PLIST_PATH" 2>/dev/null || true

    cat > "$PLIST_PATH" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
    "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>ai.vectrify.runner</string>
    <key>ProgramArguments</key>
    <array>
        <string>$INSTALL_BIN</string>
        <string>--config</string>
        <string>$CONFIG_FILE</string>
    </array>
    <key>UserName</key>
    <string>$PLIST_USER</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/var/log/vectrify-runner.log</string>
    <key>StandardErrorPath</key>
    <string>/var/log/vectrify-runner.log</string>
</dict>
</plist>
EOF

    chmod 644 "$PLIST_PATH"
    echo " done"

    printf "  [4/4] Loading service..."
    launchctl load "$PLIST_PATH"
    echo " done"

    echo ""
    echo "  Manage with:"
    echo "    sudo launchctl start  ai.vectrify.runner"
    echo "    sudo launchctl stop   ai.vectrify.runner"
    echo "    tail -f /var/log/vectrify-runner.log"

fi

echo ""
echo "  Install complete!"
echo ""

# Clean up temp download if applicable.
if [ "$DOWNLOADED" = "true" ]; then
    rm -f "$TMP_BIN"
fi
