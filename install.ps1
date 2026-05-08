# install.ps1 - Install Vectrify Agent Runner as a Windows Service
#
# One-liner (run from an Administrator PowerShell):
#   iwr -useb https://github.com/kevin4885/vectrify-agent-runner/releases/latest/download/install.ps1 | iex

[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Install-VectrifyRunner {

    $GITHUB_REPO    = "kevin4885/vectrify-agent-runner"
    $ServiceName    = "VectrifyRunner"
    $ServiceDisplay = "Vectrify Agent Runner"
    $InstallDir     = "C:\Program Files\VectrifyRunner"
    $ConfigDir      = "C:\ProgramData\VectrifyRunner"
    $ConfigFile     = "$ConfigDir\config.yaml"
    $LogFile        = "$ConfigDir\vectrify-runner.log"
    $ExeDest        = "$InstallDir\vectrify-runner.exe"

    # ── Admin check ───────────────────────────────────────────────────────────
    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if (-not $isAdmin) {
        Write-Host ""
        Write-Host "  ERROR: Run from an Administrator PowerShell." -ForegroundColor Red
        Write-Host "  Right-click PowerShell -> Run as administrator, then try again." -ForegroundColor Yellow
        Write-Host ""
        return
    }

    $ErrorActionPreference = "Stop"

    Write-Host ""
    Write-Host "  Vectrify Agent Runner - Installer" -ForegroundColor Cyan
    Write-Host ""

    # ── Locate or download binary ─────────────────────────────────────────────
    $src = $null; $downloaded = $false
    foreach ($c in @(".\vectrify-runner.exe", ".\dist\vectrify-runner-windows-amd64.exe")) {
        if (Test-Path $c) { $src = (Resolve-Path $c).Path; break }
    }
    if (-not $src) {
        $asset = "vectrify-runner-windows-amd64.exe"
        $url   = "https://github.com/$GITHUB_REPO/releases/latest/download/$asset"
        $tmp   = "$env:TEMP\vectrify-runner-$(Get-Random).exe"
        Write-Host "  Downloading $asset..." -NoNewline
        try {
            Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing
            $src = $tmp; $downloaded = $true
            Write-Host " done" -ForegroundColor Green
        } catch {
            Write-Host " FAILED" -ForegroundColor Red
            Write-Host "  $_" -ForegroundColor Red
            return
        }
    }
    Write-Host "  Binary : $src" -ForegroundColor DarkGray
    Write-Host ""

    # ── Update path: existing install detected ────────────────────────────────
    if (Test-Path $ConfigFile) {
        Write-Host "  Existing install detected - updating binary..." -NoNewline
        $svc = Get-Service $ServiceName -EA SilentlyContinue
        if ($svc -and $svc.Status -eq "Running") {
            sc.exe stop $ServiceName | Out-Null
            Start-Sleep 2
        }
        New-Item -ItemType Directory -Force $InstallDir | Out-Null
        Copy-Item -Force $src $ExeDest
        Start-Service $ServiceName
        Write-Host " done" -ForegroundColor Green
        Write-Host ""
        $st = (Get-Service $ServiceName).Status
        Write-Host "  $ServiceName : $st" -ForegroundColor $(if ($st -eq "Running") { "Green" } else { "Yellow" })
        Write-Host ""
        if ($downloaded) { Remove-Item $src -EA 0 }
        return
    }

    # ── Prompt helpers ────────────────────────────────────────────────────────
    function Ask-Required([string]$Label) {
        while ($true) {
            $v = (Read-Host "  $Label").Trim()
            if ($v) { return $v }
            Write-Host "  Required." -ForegroundColor Yellow
        }
    }
    function Ask-Default([string]$Label, [string]$Def) {
        $v = (Read-Host "  $Label [$Def]").Trim()
        return $(if ($v) { $v } else { $Def })
    }
    function Ask-YesNo([string]$Label, [bool]$Def = $false) {
        $hint = if ($Def) { "Y/n" } else { "y/N" }
        while ($true) {
            $v = (Read-Host "  $Label [$hint]").Trim().ToLower()
            if ($v -eq "")  { return $Def }
            if ($v -eq "y") { return $true }
            if ($v -eq "n") { return $false }
            Write-Host "  Enter Y or N." -ForegroundColor Yellow
        }
    }
    function Ask-Choice([string]$Label, [string[]]$Choices, [string]$Def) {
        $hint = $Choices -join " | "
        while ($true) {
            $v = (Read-Host "  $Label ($hint) [$Def]").Trim().ToLower()
            if ($v -eq "") { return $Def }
            if ($Choices -contains $v) { return $v }
            Write-Host "  Choose: $hint" -ForegroundColor Yellow
        }
    }

    # ── Collect config ────────────────────────────────────────────────────────
    Write-Host "  Configure the runner:" -ForegroundColor White
    Write-Host ""

    while ($true) {
        $workspaceRoot = Ask-Required "Workspace root folder"
        if (Test-Path $workspaceRoot -PathType Container) { break }
        Write-Host "  Not found." -ForegroundColor Yellow
        if (Ask-YesNo "Create it?" $false) { New-Item -ItemType Directory -Force $workspaceRoot | Out-Null; break }
    }
    while ($true) {
        $runnerKey = Ask-Required "Runner key (vrun_...)"
        if ($runnerKey -match '^vrun_.+') { break }
        Write-Host "  Must start with vrun_" -ForegroundColor Yellow
    }
    $allowShell = Ask-YesNo  "Allow shell commands?" $false
    $logLevel   = Ask-Choice "Log level" @("info","debug","warn","error") "info"
    while ($true) {
        $bs = Ask-Default "Max reconnect backoff seconds" "60"
        if ($bs -match '^\d+$' -and [int]$bs -gt 0) { $backoff = [int]$bs; break }
        Write-Host "  Must be a positive integer." -ForegroundColor Yellow
    }

    $allowShellYaml = if ($allowShell) { "true" } else { "false" }

    # ── Summary + confirm ─────────────────────────────────────────────────────
    Write-Host ""
    Write-Host "  workspace : $workspaceRoot"
    Write-Host "  key       : $($runnerKey.Substring(0,[Math]::Min(8,$runnerKey.Length)))..."
    Write-Host "  shell     : $allowShellYaml  |  log: $logLevel  |  backoff: ${backoff}s"
    Write-Host ""
    if (-not (Ask-YesNo "Proceed?" $true)) { if ($downloaded) { Remove-Item $src -EA 0 }; return }
    Write-Host ""

    # ── Install ───────────────────────────────────────────────────────────────
    Write-Host "  [1/5] Installing binary..."   -NoNewline
    New-Item -ItemType Directory -Force $InstallDir | Out-Null
    Copy-Item -Force $src $ExeDest
    Write-Host " done" -ForegroundColor Green

    Write-Host "  [2/5] Writing config..."      -NoNewline
    New-Item -ItemType Directory -Force $ConfigDir | Out-Null
    @"
api_url:               wss://api.vectrify.ai/api/v1/runner/ws
runner_key:            $runnerKey
workspace_root:        $workspaceRoot
allow_shell:           $allowShellYaml
log_level:             $logLevel
reconnect_max_backoff: $backoff
log_file:              $LogFile
"@ | Set-Content -Encoding UTF8 $ConfigFile
    Write-Host " done" -ForegroundColor Green

    Write-Host "  [3/5] Registering service..." -NoNewline
    $svc = Get-Service $ServiceName -EA SilentlyContinue
    if ($svc) {
        if ($svc.Status -eq "Running") { Stop-Service $ServiceName -Force -EA 0; Start-Sleep 2 }
        sc.exe delete $ServiceName | Out-Null; Start-Sleep 1
    }
    New-Service -Name $ServiceName -DisplayName $ServiceDisplay -StartupType Automatic `
        -BinaryPathName "`"$ExeDest`" --config `"$ConfigFile`"" | Out-Null
    sc.exe description $ServiceName "Connects to Vectrify Cloud and executes agent commands on this machine." | Out-Null
    Write-Host " done" -ForegroundColor Green

    Write-Host "  [4/5] Restart-on-failure..."  -NoNewline
    sc.exe failure $ServiceName reset= 3600 actions= restart/5000/restart/10000/restart/30000 | Out-Null
    Write-Host " done" -ForegroundColor Green

    Write-Host "  [5/5] Starting service..."    -NoNewline
    Start-Service $ServiceName
    Write-Host " done" -ForegroundColor Green

    # ── Done ──────────────────────────────────────────────────────────────────
    Write-Host ""
    $st = (Get-Service $ServiceName).Status
    Write-Host "  $ServiceName : $st" -ForegroundColor $(if ($st -eq "Running") { "Green" } else { "Yellow" })
    Write-Host ""
    Write-Host "  Done!" -ForegroundColor Cyan
    Write-Host ""
    Write-Host "  Logs : $LogFile"
    Write-Host ""

    if ($downloaded) { Remove-Item $src -EA 0 }
}

Install-VectrifyRunner
