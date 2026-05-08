# install.ps1 - Install Vectrify Agent Runner as a Windows Service
#
# Run from an Administrator PowerShell — either directly or via the one-liner:
#   iwr -useb https://github.com/vectrify/vectrify-agent-runner/releases/latest/download/install.ps1 | iex
#
# Usage:
#   .\install.ps1

$GITHUB_REPO = "kevin4885/vectrify-agent-runner"

# Force TLS 1.2 — Windows PowerShell 5 defaults to TLS 1.0 which GitHub rejects.
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

# ── Administrator check ───────────────────────────────────────────────────────
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)

if (-not $isAdmin) {
    Write-Host ""
    Write-Host "  ERROR: This installer must run as Administrator." -ForegroundColor Red
    Write-Host ""
    Write-Host "  Open PowerShell as Administrator and run:" -ForegroundColor Yellow
    Write-Host "  [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; iwr -useb https://github.com/$GITHUB_REPO/releases/latest/download/install.ps1 | iex" -ForegroundColor Cyan
    Write-Host ""
    if ($PSCommandPath) { Read-Host "  Press Enter to exit" }
    exit 1
}

$ErrorActionPreference = "Stop"

$ServiceName = "VectrifyRunner"
$ServiceDisplay = "Vectrify Agent Runner"
$InstallDir  = "C:\Program Files\VectrifyRunner"
$ConfigDir   = "C:\ProgramData\VectrifyRunner"
$ConfigFile  = "$ConfigDir\config.yaml"
$ExeName     = "vectrify-runner.exe"
$ExeDest     = "$InstallDir\$ExeName"

# ── Banner ────────────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "  Vectrify Agent Runner - Windows Installer" -ForegroundColor Cyan
Write-Host ""

# ── Locate binary ─────────────────────────────────────────────────────────────
$candidates = @(
    ".\$ExeName",
    ".\dist\vectrify-runner-windows-amd64.exe"
)

$src        = $null
$downloaded = $false
foreach ($c in $candidates) {
    if (Test-Path $c) { $src = (Resolve-Path $c).Path; break }
}

if (-not $src) {
    $asset       = "vectrify-runner-windows-amd64.exe"
    $downloadUrl = "https://github.com/$GITHUB_REPO/releases/latest/download/$asset"
    $tmpExe      = "$env:TEMP\vectrify-runner-$(Get-Random).exe"

    Write-Host "  Downloading $asset..." -NoNewline
    try {
        Invoke-WebRequest -Uri $downloadUrl -OutFile $tmpExe -UseBasicParsing
        $src        = $tmpExe
        $downloaded = $true
        Write-Host " done" -ForegroundColor Green
    } catch {
        Write-Host " FAILED" -ForegroundColor Red
        Write-Host "  ERROR: $_" -ForegroundColor Red
        Read-Host "  Press Enter to exit"
        exit 1
    }
}

Write-Host "  Binary : $src" -ForegroundColor DarkGray
Write-Host ""

# ── Prompt helpers ────────────────────────────────────────────────────────────
function Read-Required {
    param([string]$Label)
    while ($true) {
        $val = (Read-Host "  $Label").Trim()
        if ($val -ne "") { return $val }
        Write-Host "  This field is required." -ForegroundColor Yellow
    }
}

function Read-WithDefault {
    param([string]$Label, [string]$Default)
    $val = (Read-Host "  $Label [$Default]").Trim()
    return $(if ($val -ne "") { $val } else { $Default })
}

function Read-YesNo {
    param([string]$Label, [bool]$Default = $false)
    $hint = if ($Default) { "Y/n" } else { "y/N" }
    while ($true) {
        $val = (Read-Host "  $Label [$hint]").Trim().ToLower()
        if ($val -eq "")  { return $Default }
        if ($val -eq "y") { return $true    }
        if ($val -eq "n") { return $false   }
        Write-Host "  Please enter Y or N." -ForegroundColor Yellow
    }
}

function Read-Choice {
    param([string]$Label, [string[]]$Choices, [string]$Default)
    $hint = $Choices -join " | "
    while ($true) {
        $val = (Read-Host "  $Label ($hint) [$Default]").Trim().ToLower()
        if ($val -eq "") { return $Default }
        if ($Choices -contains $val) { return $val }
        Write-Host "  Choose one of: $hint" -ForegroundColor Yellow
    }
}

# ── Collect configuration ─────────────────────────────────────────────────────
Write-Host "  Configure the runner:" -ForegroundColor White
Write-Host ""

# workspace_root
while ($true) {
    $workspaceRoot = Read-Required -Label "Workspace root folder path"
    if (Test-Path $workspaceRoot -PathType Container) { break }
    Write-Host "  Directory not found: $workspaceRoot" -ForegroundColor Yellow
    $create = Read-YesNo -Label "Create it?" -Default $false
    if ($create) { New-Item -ItemType Directory -Force $workspaceRoot | Out-Null; break }
}

# runner_key
while ($true) {
    $runnerKey = Read-Required -Label "Runner key (vrun_...)"
    if ($runnerKey -match '^vrun_.+') { break }
    Write-Host "  Key must start with 'vrun_'" -ForegroundColor Yellow
}

# allow_shell
$allowShell = Read-YesNo -Label "Allow shell commands?" -Default $false

# log_level
$logLevel = Read-Choice -Label "Log level" -Choices @("info","debug","warn","error") -Default "info"

# reconnect_max_backoff
while ($true) {
    $backoffStr = Read-WithDefault -Label "Max reconnect backoff in seconds" -Default "60"
    if ($backoffStr -match '^\d+$' -and [int]$backoffStr -gt 0) { $backoff = [int]$backoffStr; break }
    Write-Host "  Must be a positive integer." -ForegroundColor Yellow
}

$allowShellYaml = if ($allowShell) { "true" } else { "false" }
$keyPreview     = $runnerKey.Substring(0, [Math]::Min(8, $runnerKey.Length)) + "..."

# ── Summary ───────────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "  ----------------------------------------" -ForegroundColor DarkGray
Write-Host "  workspace_root : $workspaceRoot"
Write-Host "  runner_key     : $keyPreview"
Write-Host "  allow_shell    : $allowShellYaml"
Write-Host "  log_level      : $logLevel"
Write-Host "  backoff        : $backoff s"
Write-Host "  install dir    : $InstallDir"
Write-Host "  config file    : $ConfigFile"
Write-Host "  ----------------------------------------" -ForegroundColor DarkGray
Write-Host ""

$proceed = Read-YesNo -Label "Proceed with install?" -Default $true
if (-not $proceed) { Write-Host "  Aborted."; Write-Host ""; Read-Host "  Press Enter to exit"; exit 0 }
Write-Host ""

# ── Step 1: Install binary ────────────────────────────────────────────────────
Write-Host "  [1/5] Installing binary..." -NoNewline
New-Item -ItemType Directory -Force $InstallDir | Out-Null
Copy-Item -Force $src $ExeDest
Write-Host " done" -ForegroundColor Green

# ── Step 2: Write config ──────────────────────────────────────────────────────
Write-Host "  [2/5] Writing config..." -NoNewline
New-Item -ItemType Directory -Force $ConfigDir | Out-Null

@"
api_url:               wss://api.vectrify.ai/api/v1/runner/ws
runner_key:            $runnerKey
workspace_root:        $workspaceRoot
allow_shell:           $allowShellYaml
log_level:             $logLevel
reconnect_max_backoff: $backoff
"@ | Set-Content -Encoding UTF8 $ConfigFile

Write-Host " done" -ForegroundColor Green

# ── Step 3: Remove old service if present ────────────────────────────────────
Write-Host "  [3/5] Registering Windows Service..." -NoNewline
$existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existing) {
    if ($existing.Status -eq "Running") {
        Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
        Start-Sleep -Seconds 2
    }
    sc.exe delete $ServiceName | Out-Null
    Start-Sleep -Seconds 1
}

$binPathArg = "`"$ExeDest`" --config `"$ConfigFile`""
sc.exe create $ServiceName binPath= $binPathArg start= auto obj= LocalSystem DisplayName= $ServiceDisplay | Out-Null
sc.exe description $ServiceName "Connects to Vectrify Cloud and executes agent commands on this machine." | Out-Null
Write-Host " done" -ForegroundColor Green

# ── Step 4: Configure restart-on-failure ─────────────────────────────────────
Write-Host "  [4/5] Configuring restart-on-failure..." -NoNewline
sc.exe failure $ServiceName reset= 3600 actions= restart/5000/restart/10000/restart/30000 | Out-Null
Write-Host " done" -ForegroundColor Green

# ── Step 5: Start service ─────────────────────────────────────────────────────
Write-Host "  [5/5] Starting service..." -NoNewline
Start-Service -Name $ServiceName
Write-Host " done" -ForegroundColor Green

# ── Result ────────────────────────────────────────────────────────────────────
Write-Host ""
$status = (Get-Service -Name $ServiceName).Status
$color  = if ($status -eq "Running") { "Green" } else { "Yellow" }
Write-Host "  Service status : $status" -ForegroundColor $color
Write-Host ""
Write-Host "  Install complete!" -ForegroundColor Cyan
Write-Host ""
Write-Host "  Manage the service with:"
Write-Host "    Start-Service   $ServiceName"
Write-Host "    Stop-Service    $ServiceName"
Write-Host "    Restart-Service $ServiceName"
Write-Host "    Get-Service     $ServiceName"
Write-Host ""

# Clean up temp download if applicable.
if ($downloaded) { Remove-Item $src -ErrorAction SilentlyContinue }

Read-Host "  Press Enter to exit"
