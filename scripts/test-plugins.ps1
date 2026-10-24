#Requires -Version 5.1
<#
.SYNOPSIS
    Runs every plugin module's tests, one module at a time.

.DESCRIPTION
    The root go.mod intentionally does not include plugins/: each plugin is its
    own module. "go test ./..." at the repository root therefore never covers
    them. This script walks plugins/*/ and fails if any module fails.

    -ldflags=-buildid= works around Windows security products that deny the
    buildid rewrite of *.test.exe. If a module still cannot launch its test
    binary ("Access is denied"), the script retries through a renamed copy.

.PARAMETER Modules
    Module directory names (default: every directory under plugins/).

.PARAMETER Race
    Also run with the race detector.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\scripts\test-plugins.ps1
    powershell -ExecutionPolicy Bypass -File .\scripts\test-plugins.ps1 -Race
#>
[CmdletBinding()]
param(
    [string[]]$Modules,
    [switch]$Race
)

$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $PSScriptRoot
$Plugins = Join-Path $Root 'plugins'

$flags = @('-count=1', '-ldflags=-buildid=')
if ($Race) { $flags += '-race' }

if (-not $Modules) {
    $Modules = @(Get-ChildItem $Plugins -Directory |
        Where-Object { Test-Path (Join-Path $_.FullName 'go.mod') } |
        ForEach-Object { $_.Name })
}

$failed = @()
foreach ($id in $Modules) {
    $dir = Join-Path $Plugins $id
    if (-not (Test-Path (Join-Path $dir 'go.mod'))) {
        Write-Host "[skip] $id (no go.mod)"
        continue
    }
    Write-Host "== $id =="
    Push-Location $dir
    try {
        $prevEAP = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        $output = & go test @flags ./... 2>&1
        $code = $LASTEXITCODE
        if ($code -ne 0 -and ($output -join "`n") -match 'Access is denied') {
            $pkgs = & go list ./...
            if ($LASTEXITCODE -eq 0 -and @($pkgs).Count -eq 1) {
                $tmp = Join-Path $env:TEMP "aiworkgate-$id-check.exe"
                Write-Host "  test binary blocked; retrying as $tmp"
                if ($Race) {
                    & go test -c -race -ldflags=-buildid= -o $tmp .
                } else {
                    & go test -c -ldflags=-buildid= -o $tmp .
                }
                if ($LASTEXITCODE -eq 0) {
                    & $tmp "-test.count=1"
                    $code = $LASTEXITCODE
                }
                Remove-Item $tmp -Force -ErrorAction SilentlyContinue
            }
        }
        $ErrorActionPreference = $prevEAP
        if ($code -ne 0) {
            $output | Write-Host
            $failed += $id
        } else {
            Write-Host "  ok"
        }
    } finally {
        Pop-Location
    }
}

if ($failed.Count -gt 0) {
    Write-Host ("FAILED: " + ($failed -join ', '))
    exit 1
}
Write-Host "all plugin modules passed"
