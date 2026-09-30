# build.ps1 - Build all platform binaries for vectrify-agent-runner
#
# Usage:
#   .\build.ps1                  # version defaults to 1.0.0
#   .\build.ps1 -Version 1.2.3

param(
    [string]$Version = "1.0.0"
)

$ErrorActionPreference = "Stop"

# Windows: NO "-s -w". Stripping symbols/DWARF from an unsigned Go exe makes it
# look opaque to Microsoft Defender's ML classifier (Trojan:Win32/Bearfoos.A!ml
# false positive). Linux/macOS keep the smaller stripped build.
# -trimpath removes the build machine's absolute paths from every binary.
$LDFlagsWin   = "-X vectrify/agent-runner/config.Version=$Version"
$LDFlagsOther = "-s -w -X vectrify/agent-runner/config.Version=$Version"
$OutDir  = "dist"

# Windows version-info + manifest resource (company/product/version, asInvoker).
# Generated into resource_windows_amd64.syso (gitignored); the _windows_amd64
# suffix means only the Windows build links it. Signed-off metadata makes the
# exe look like a normal product to AV heuristics.
$vparts = ($Version -replace '[-+].*$','') -split '\.'
$vmaj = if ($vparts.Count -gt 0 -and $vparts[0] -match '^\d+$') { [int]$vparts[0] } else { 0 }
$vmin = if ($vparts.Count -gt 1 -and $vparts[1] -match '^\d+$') { [int]$vparts[1] } else { 0 }
$vpat = if ($vparts.Count -gt 2 -and $vparts[2] -match '^\d+$') { [int]$vparts[2] } else { 0 }
$vfull = "$vmaj.$vmin.$vpat.0"

Write-Host ""
Write-Host "  Vectrify Agent Runner  -  build" -ForegroundColor Cyan
Write-Host "  Version : $Version"
Write-Host "  Output  : .\$OutDir\"
Write-Host ""

New-Item -ItemType Directory -Force $OutDir | Out-Null

Write-Host "  Generating Windows version resource..." -NoNewline
# Static fields live in winres/versioninfo.json; flags override the version.
$ErrorActionPreference = "Continue"   # native stderr must not abort the script
$viOut = go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.4.1 -64 `
    -o resource_windows_amd64.syso `
    -file-version $vfull -product-version $vfull `
    -ver-major $vmaj -ver-minor $vmin -ver-patch $vpat -ver-build 0 `
    -product-ver-major $vmaj -product-ver-minor $vmin -product-ver-patch $vpat -product-ver-build 0 `
    winres/versioninfo.json 2>&1
$viExit = $LASTEXITCODE
$ErrorActionPreference = "Stop"
if ($viExit -eq 0) { Write-Host " OK" -ForegroundColor Green } else { Write-Host " FAILED (continuing without it)" -ForegroundColor Yellow; Write-Host ($viOut | Out-String) }

$targets = @(
    @{ GOOS = "windows"; GOARCH = "amd64"; Output = "vectrify-runner-windows-amd64.exe" },
    @{ GOOS = "linux";   GOARCH = "amd64"; Output = "vectrify-runner-linux-amd64"        },
    @{ GOOS = "linux";   GOARCH = "arm64"; Output = "vectrify-runner-linux-arm64"        },
    @{ GOOS = "darwin";  GOARCH = "amd64"; Output = "vectrify-runner-darwin-amd64"       },
    @{ GOOS = "darwin";  GOARCH = "arm64"; Output = "vectrify-runner-darwin-arm64"       }
)

$succeeded = 0
$failed    = 0

foreach ($t in $targets) {
    $env:GOOS   = $t.GOOS
    $env:GOARCH = $t.GOARCH
    $out = "$OutDir\$($t.Output)"

    Write-Host ("  {0,-8} {1,-8}  ->  {2,-44}" -f $t.GOOS, $t.GOARCH, $t.Output) -NoNewline

    $ld = if ($t.GOOS -eq "windows") { $LDFlagsWin } else { $LDFlagsOther }
    go build -trimpath -ldflags $ld -o $out . 2>&1 | Out-Null

    if ($LASTEXITCODE -eq 0) {
        $mb = (Get-Item $out).Length / 1MB
        Write-Host ("  OK  ({0:N1} MB)" -f $mb) -ForegroundColor Green
        $succeeded++
    } else {
        Write-Host "  FAILED" -ForegroundColor Red
        $failed++
    }
}

Remove-Item Env:\GOOS, Env:\GOARCH -ErrorAction SilentlyContinue

Write-Host ""
if ($failed -eq 0) {
    Write-Host "  All $succeeded builds succeeded." -ForegroundColor Green
} else {
    Write-Host "  $succeeded succeeded, $failed failed." -ForegroundColor Yellow
}
Write-Host ""
