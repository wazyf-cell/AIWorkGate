#Requires -Version 5.1
<#
.SYNOPSIS
    Builds and packs an AIWorkGate plugin into an installable zip.

.DESCRIPTION
    Implements docs/插件生成契约.md: read manifest.json, build entry.exe,
    stage only the runtime files, and write a zip whose manifest.json sits at
    the archive root. The archive is re-opened and checked with the same rules
    the host's installer applies before the path is printed.

.PARAMETER PluginDir
    Plugin source directory containing manifest.json (default: current dir).

.PARAMETER Output
    Destination zip path. Default: <PluginDir>\build\<id>.zip

.PARAMETER ExtraFiles
    Additional runtime files/directories relative to PluginDir (e.g. assets).
    Source files (*.go) are never packed.

.PARAMETER PrebuiltExe
    Use this file as the entry binary instead of running go build.

.PARAMETER SigningKey
    Ed25519 key file from tools/plugin-sign -gen. When set, the packer writes
    manifest.sig so an install with plugins.trusted_keys configured accepts it.

.PARAMETER KeyID
    Key id the signature is filed under (must match plugins.trusted_keys).

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\scripts\pack-plugin.ps1 `
        -PluginDir .\examples\plugin-template
#>
[CmdletBinding()]
param(
    [string]$PluginDir = ".",
    [string]$Output,
    [string[]]$ExtraFiles = @(),
    [string]$PrebuiltExe,
    [string]$SigningKey,
    [string]$KeyID
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.IO.Compression.FileSystem

function Fail([string]$Message) {
    Write-Host "pack-plugin: $Message"
    exit 1
}

function Test-SafeRelative([string]$Path) {
    if ([string]::IsNullOrWhiteSpace($Path)) { return $false }
    if ([System.IO.Path]::IsPathRooted($Path)) { return $false }
    if ($Path.Contains(':')) { return $false }
    if ($Path -match '(^|[\\/])\.\.([\\/]|$)') { return $false }
    return $true
}

$PluginDir = (Resolve-Path -LiteralPath $PluginDir).Path
$manifestPath = Join-Path $PluginDir 'manifest.json'
if (-not (Test-Path -LiteralPath $manifestPath)) {
    Fail "manifest.json not found in $PluginDir"
}

$manifest = Get-Content -LiteralPath $manifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
$id = [string]$manifest.id
if ($id -notmatch '^[a-z0-9][a-z0-9-]{1,31}$') {
    Fail "invalid id '$id' (must match ^[a-z0-9][a-z0-9-]{1,31}$)"
}
$level = 0
if (-not [int]::TryParse([string]$manifest.api_level, [ref]$level) -or $level -lt 1 -or $level -gt 2) {
    Fail "api_level must be 1 or 2, got '$($manifest.api_level)'"
}
$entry = [string]$manifest.entry.exe
if (-not (Test-SafeRelative $entry)) {
    Fail "entry.exe '$entry' must be a safe relative path (no drive, no leading /, no ..)"
}

if (-not $Output) {
    $Output = Join-Path $PluginDir "build\$id.zip"
}
$Output = [System.IO.Path]::GetFullPath($Output)
$outDir = Split-Path -Parent $Output
if ($outDir) { New-Item -ItemType Directory -Force -Path $outDir | Out-Null }

$stage = Join-Path ([System.IO.Path]::GetTempPath()) ("aiworkgate-pack-$id-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $stage | Out-Null
try {
    $entryTarget = Join-Path $stage $entry
    $entryDir = Split-Path -Parent $entryTarget
    if ($entryDir) { New-Item -ItemType Directory -Force -Path $entryDir | Out-Null }

    if ($PrebuiltExe) {
        $PrebuiltExe = (Resolve-Path -LiteralPath $PrebuiltExe).Path
        Copy-Item -LiteralPath $PrebuiltExe -Destination $entryTarget -Force
    } else {
        Push-Location $PluginDir
        try {
            $env:CGO_ENABLED = '0'
            & go build -trimpath -buildvcs=false -o $entryTarget .
            $code = $LASTEXITCODE
        } finally {
            Pop-Location
        }
        if ($code -ne 0) { Fail "go build failed (exit $code)" }
    }

    Copy-Item -LiteralPath $manifestPath -Destination (Join-Path $stage 'manifest.json') -Force

    foreach ($extra in $ExtraFiles) {
        if (-not (Test-SafeRelative $extra)) { Fail "extra path '$extra' is not a safe relative path" }
        $src = Join-Path $PluginDir $extra
        if (-not (Test-Path -LiteralPath $src)) { Fail "extra path '$extra' does not exist" }
        $dst = Join-Path $stage $extra
        if (Test-Path -LiteralPath $src -PathType Container) {
            New-Item -ItemType Directory -Force -Path $dst | Out-Null
            Copy-Item -Path (Join-Path $src '*') -Destination $dst -Recurse -Force
        } else {
            $parent = Split-Path -Parent $dst
            if ($parent) { New-Item -ItemType Directory -Force -Path $parent | Out-Null }
            Copy-Item -LiteralPath $src -Destination $dst -Force
        }
    }

    if ($SigningKey) {
        if (-not $KeyID) { Fail "-KeyID is required with -SigningKey" }
        $SigningKey = (Resolve-Path -LiteralPath $SigningKey).Path
        $repo = Split-Path -Parent $PSScriptRoot
        Push-Location $repo
        try {
            & go run ./tools/plugin-sign -key $SigningKey -key-id $KeyID -dir $stage -quiet
            $code = $LASTEXITCODE
        } finally {
            Pop-Location
        }
        if ($code -ne 0) { Fail "signing failed (exit $code)" }
    }

    if (Test-Path -LiteralPath $Output) { Remove-Item -LiteralPath $Output -Force }
    [System.IO.Compression.ZipFile]::CreateFromDirectory(
        $stage, $Output, [System.IO.Compression.CompressionLevel]::Optimal, $false)

    # Verify with the host's install rules: flat root, manifest present, entry
    # present, every name relative, nothing over the size cap.
    $zip = [System.IO.Compression.ZipFile]::OpenRead($Output)
    try {
        $names = @()
        [long]$total = 0
        foreach ($e in $zip.Entries) {
            $name = $e.FullName.Replace('\', '/')
            if (-not (Test-SafeRelative $name)) { Fail "packed entry '$name' is not a safe relative path" }
            $names += $name
            $total += $e.Length
        }
        if ($names -notcontains 'manifest.json') {
            Fail "manifest.json is not at the archive root (host requires body root or a single top-level dir)"
        }
        if ($names -notcontains $entry.Replace('\', '/')) {
            Fail "entry '$entry' is missing from the archive root"
        }
        if ($SigningKey -and ($names -notcontains 'manifest.sig')) {
            Fail "manifest.sig is missing from the archive root"
        }
        if ($total -gt 1GB) { Fail "uncompressed size $total exceeds the 1 GiB host cap" }
    } finally {
        $zip.Dispose()
    }

    Write-Host "packed $id ($level) -> $Output"
    $names | Sort-Object | ForEach-Object { Write-Host "  $_" }
} finally {
    Remove-Item -Recurse -Force -LiteralPath $stage -ErrorAction SilentlyContinue
}
