#Requires -Version 5.1
<#
.SYNOPSIS
    Development helper for AIWorkGate: runs the Vite dev server and the Go host.

.DESCRIPTION
    Two processes are needed:
      * web\   : npm run dev   -> http://127.0.0.1:5173
      * host   : go run ./cmd/aiworkgate -> http://127.0.0.1:7865

    The host injects AIWORKGATE_* variables INTO plugins; it does not read any
    AIWORKGATE_* variable itself. So this script sets no environment variable
    and there is no dev-only switch in the host. Just run the two commands.

    Vite proxies /api to a hard-coded http://127.0.0.1:7865. The host tries
    7865 first and then 7866..7875 when the port is busy, so if 7865 is taken
    the SPA dev server will point at the wrong port; free 7865 or edit
    web\vite.config.ts server.proxy locally.

.PARAMETER NoWeb
    Do not start the Vite dev server (use the SPA embedded in the Go binary).

.PARAMETER NoHost
    Do not start the Go host (start it yourself with: go run ./cmd/aiworkgate).

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1
#>
[CmdletBinding()]
param(
    [switch]$NoWeb,
    [switch]$NoHost
)

$ErrorActionPreference = "Stop"

$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

if (-not $NoWeb) {
    $web = Join-Path $Root "web"
    if (-not (Test-Path (Join-Path $web "node_modules"))) {
        throw "web\node_modules is missing; run: cd web; npm ci"
    }
    Write-Host "==> starting Vite dev server in a new window (http://127.0.0.1:5173)"
    Start-Process -FilePath "cmd.exe" -ArgumentList "/c", "npm run dev" -WorkingDirectory $web | Out-Null
}

if (-not $NoHost) {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw "go was not found in PATH; install Go 1.26.x first."
    }
    Write-Host "==> starting host: go run ./cmd/aiworkgate (Ctrl+C stops the host)"
    & go run ./cmd/aiworkgate
}
else {
    Write-Host "==> host not started; run: go run ./cmd/aiworkgate"
}
