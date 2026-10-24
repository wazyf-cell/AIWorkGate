#Requires -Version 5.1
<#
.SYNOPSIS
    Builds AIWorkGate: the Vue SPA, the Go host, and a portable release folder.

.DESCRIPTION
    Steps:
      1. web:  npm ci (only when web\node_modules is missing) and npm run build.
               vite writes the bundle to internal\webui\dist, which is embedded
               into the Go binary with go:embed.
      2. host: CGO_ENABLED=0 go build -trimpath
               -ldflags "-s -w -X aiworkgate/internal/app.Version=<Version>"
      3. release layout: build\AIWorkGate-<Version>\
             aiworkgate.exe
             plugins\        staging folder for plugin zips / directories
             docs\
             README.md
      4. prints the exe size and SHA-256.

    The host does NOT read plugins\ next to the exe. At runtime plugins live in
    %APPDATA%\AIWorkGate\plugins\<id> (drop-in, one directory per plugin).
    The release plugins\ folder is only a staging area for the zips you install
    from the admin UI (Plugins page, "plugin zip path").

.PARAMETER Version
    Version stamped into aiworkgate/internal/app.Version (default 0.1.0-dev)
    and used in the release folder name.

.PARAMETER SkipWeb
    Reuse the existing internal\webui\dist instead of rebuilding the SPA.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\scripts\build.ps1 -Version 0.1.0
#>
[CmdletBinding()]
param(
    [string]$Version = "0.1.0-dev",
    [switch]$SkipWeb
)

$ErrorActionPreference = "Stop"

$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "go was not found in PATH; install Go 1.26.x first."
}

$distIndex = Join-Path $Root "internal\webui\dist\index.html"

if ($SkipWeb) {
    if (-not (Test-Path $distIndex)) {
        throw "internal\webui\dist\index.html is missing; run without -SkipWeb (or run npm run build in web\) first."
    }
    Write-Host "==> web: skipped (-SkipWeb), reusing internal\webui\dist"
}
else {
    if (-not (Get-Command npm -ErrorAction SilentlyContinue)) {
        throw "npm was not found in PATH; install Node.js or pass -SkipWeb."
    }
    $web = Join-Path $Root "web"
    if (-not (Test-Path (Join-Path $web "node_modules"))) {
        Write-Host "==> web: npm ci"
        Push-Location $web
        try {
            & npm ci
            if ($LASTEXITCODE -ne 0) { throw "npm ci failed with exit code $LASTEXITCODE" }
        }
        finally {
            Pop-Location
        }
    }
    Write-Host "==> web: npm run build"
    Push-Location $web
    try {
        & npm run build
        if ($LASTEXITCODE -ne 0) { throw "npm run build failed with exit code $LASTEXITCODE" }
    }
    finally {
        Pop-Location
    }
}

$buildDir = Join-Path $Root "build"
New-Item -ItemType Directory -Force -Path $buildDir | Out-Null
$exePath = Join-Path $buildDir "aiworkgate.exe"

Write-Host "==> host: go build (version $Version)"
$ldflags = "-s -w -X aiworkgate/internal/app.Version=$Version"
$oldCgo = $env:CGO_ENABLED
$env:CGO_ENABLED = "0"
try {
    & go build -trimpath -buildvcs=false -ldflags $ldflags -o $exePath ./cmd/aiworkgate
    if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
}
finally {
    if ($null -eq $oldCgo) {
        Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
    }
    else {
        $env:CGO_ENABLED = $oldCgo
    }
}

# Portable layout. The exe is self-contained (SPA embedded, SQLite is pure Go).
$releaseName = "AIWorkGate-$Version"
$releaseDir = Join-Path $buildDir $releaseName
if (Test-Path $releaseDir) {
    Remove-Item -Recurse -Force $releaseDir
}
New-Item -ItemType Directory -Force -Path $releaseDir | Out-Null

Copy-Item -Path $exePath -Destination (Join-Path $releaseDir "aiworkgate.exe")

$readme = Join-Path $Root "README.md"
if (Test-Path $readme) {
    Copy-Item -Path $readme -Destination (Join-Path $releaseDir "README.md")
}

$docs = Join-Path $Root "docs"
if (Test-Path $docs) {
    Copy-Item -Path $docs -Destination (Join-Path $releaseDir "docs") -Recurse
}

$releasePlugins = Join-Path $releaseDir "plugins"
New-Item -ItemType Directory -Force -Path $releasePlugins | Out-Null
$note = @(
    "This folder is only a staging area for plugin zips/directories.",
    "",
    "The running host loads plugins from:",
    "  %APPDATA%\AIWorkGate\plugins\<plugin-id>\",
    "",
    "Install a plugin by entering its zip path on the host's Plugins page",
    "(POST /api/plugins/install with {""zip_path"": ""...""}), or copy a plugin",
    "directory there and restart the host. Each plugin directory must contain",
    "manifest.json at its root and the executable named by manifest entry.exe.",
    "Plugin private data (survives updates) lives in:",
    "  %APPDATA%\AIWorkGate\plugin-data\<plugin-id>\"
)
Set-Content -Path (Join-Path $releasePlugins "README.txt") -Value $note -Encoding ASCII

$item = Get-Item $exePath
$hash = (Get-FileHash -Algorithm SHA256 -Path $exePath).Hash
Set-Content -Path (Join-Path $releaseDir "SHA256SUMS.txt") -Value ("{0}  aiworkgate.exe" -f $hash) -Encoding ASCII

Write-Host ""
Write-Host "==> done"
Write-Host ("exe     : {0}" -f $exePath)
Write-Host ("size    : {0:N0} bytes ({1:N2} MiB)" -f $item.Length, ($item.Length / 1MB))
Write-Host ("sha256  : {0}" -f $hash)
Write-Host ("release : {0}" -f $releaseDir)
