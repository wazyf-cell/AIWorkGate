#Requires -Version 5.1
<#
.SYNOPSIS
    Runs the host test suite with the Windows-safe build flags.

.DESCRIPTION
    Some Windows security products block Go's buildid rewrite of *.test.exe
    with "Access is denied" right after the linker writes the file. Passing
    -ldflags=-buildid= skips that rewrite; the tests themselves are unchanged.

.PARAMETER Packages
    Go package patterns. Defaults to the whole host (internal/...).

.PARAMETER Race
    Also run with the race detector.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\scripts\test-host.ps1
    powershell -ExecutionPolicy Bypass -File .\scripts\test-host.ps1 -Race
#>
[CmdletBinding()]
param(
    [string[]]$Packages = @('./internal/...'),
    [switch]$Race
)

$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

$flags = @('-count=1', '-ldflags=-buildid=')
if ($Race) { $flags += '-race' }

& go build ./...
if ($LASTEXITCODE -ne 0) { throw 'go build failed' }

& go vet ./internal/... ./cmd/...
if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }

& go test @Packages @flags
if ($LASTEXITCODE -ne 0) { throw 'go test failed' }
