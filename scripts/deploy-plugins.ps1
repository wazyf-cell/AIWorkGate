# Rebuilds the host, the frontend and the requested plugins, syncs them into
# %APPDATA%\AIWorkGate\plugins and restarts everything. Mirrors handoff section
# P0.5 so a deployment is one reproducible command instead of ad-hoc copy-paste.
#
#   pwsh -File scripts\deploy-plugins.ps1
#   pwsh -File scripts\deploy-plugins.ps1 -Plugins zcode,cline -SkipWeb
param(
  [string[]]$Plugins = @(
    'aichat','autoglm','cline','codearts','gemini','lobsterai',
    'loomy','minimax','officeace','opencode','raccoon','zcode'
  ),
  [switch]$SkipWeb,
  [switch]$NoRestart
)

$ErrorActionPreference = 'Stop'
# `-File ... -Plugins a,b,c` hands the list over as one literal string, so split
# on commas to keep both pwsh and powershell.exe callers working.
$Plugins = @($Plugins | ForEach-Object { $_ -split ',' } | Where-Object { $_ })
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

function Fail($message) { Write-Error $message; exit 1 }

# 1) Stop the host first: it owns the plugin children and locks their exes.
if (-not $NoRestart) {
  Get-Process aiworkgate -ErrorAction SilentlyContinue | Stop-Process -Force
  Get-Process '*-plugin' -ErrorAction SilentlyContinue | Stop-Process -Force
  Start-Sleep -Seconds 2
}

# 2) Build every requested plugin with the manifest's expected file name.
foreach ($id in $Plugins) {
  Push-Location "plugins\$id"
  try {
    go build ./... ; if ($LASTEXITCODE -ne 0) { Fail "$id`: go build ./... failed" }
    go vet ./...   ; if ($LASTEXITCODE -ne 0) { Fail "$id`: go vet failed" }
    go build -o "$id-plugin.exe" . ; if ($LASTEXITCODE -ne 0) { Fail "$id`: go build -o failed" }
    Remove-Item "aiworkgate-plugin-$id.exe" -ErrorAction SilentlyContinue
  } finally { Pop-Location }
  Write-Host "built $id"
}

# 3) Frontend (only needed when web/ changed).
if (-not $SkipWeb) {
  Push-Location web
  try {
    npm run build ; if ($LASTEXITCODE -ne 0) { Fail 'npm run build failed' }
  } finally { Pop-Location }
}

# 4) Host.
go build -o build\aiworkgate.exe ./cmd/aiworkgate ; if ($LASTEXITCODE -ne 0) { Fail 'host build failed' }

# 5) Sync manifests + exes into the runtime directory.
$runtime = Join-Path $env:APPDATA 'AIWorkGate\plugins'
foreach ($id in $Plugins) {
  $dest = Join-Path $runtime $id
  New-Item -ItemType Directory -Force -Path $dest | Out-Null
  Copy-Item "plugins\$id\manifest.json" $dest -Force
  Copy-Item "plugins\$id\$id-plugin.exe" $dest -Force
  Write-Host "synced $id"
}

# 6) Restart and wait for every plugin process.
if (-not $NoRestart) {
  Start-Process -FilePath (Join-Path $repo 'build\aiworkgate.exe') -WorkingDirectory $repo
  # The host starts every plugin it knows, not only the ones this run rebuilt.
  $want = (Get-ChildItem (Join-Path $repo 'plugins') -Directory | Where-Object { Test-Path (Join-Path $_.FullName 'manifest.json') } | Measure-Object).Count
  for ($i = 0; $i -lt 30; $i++) {
    Start-Sleep -Seconds 1
    $have = (Get-Process '*-plugin' -ErrorAction SilentlyContinue | Measure-Object).Count
    if ($have -ge $want) { break }
  }
  $have = (Get-Process '*-plugin' -ErrorAction SilentlyContinue | Measure-Object).Count
  Write-Host "plugin processes: $have / $want"
  if ($have -lt $want) { Fail "only $have of $want plugins started; see %APPDATA%\AIWorkGate\logs" }
}
