# build.ps1 - Build all platform binaries for vectrify-agent-runner
#
# Usage:
#   .\build.ps1                  # version defaults to 1.0.0
#   .\build.ps1 -Version 1.2.3

param(
    [string]$Version = "1.0.0"
)

$ErrorActionPreference = "Stop"

$LDFlags = "-s -w -X vectrify/agent-runner/config.Version=$Version"
$OutDir  = "dist"

Write-Host ""
Write-Host "  Vectrify Agent Runner  -  build" -ForegroundColor Cyan
Write-Host "  Version : $Version"
Write-Host "  Output  : .\$OutDir\"
Write-Host ""

New-Item -ItemType Directory -Force $OutDir | Out-Null

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

    go build -ldflags $LDFlags -o $out . 2>&1 | Out-Null

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
