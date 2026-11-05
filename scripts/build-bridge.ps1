# build-bridge.ps1 — P1/P8 收尾（task-18）
#
# 把 opchost/bridge（bun/TypeScript 插件宿主桥）编译成单文件 exe，并记录 SHA256
# （对齐 P8-⑥「bun 二进制随包内置并记录哈希，禁止运行时下载」）。
#
# 用法:
#   pwsh scripts/build-bridge.ps1                      # 构建真包 + 写哈希记录
#   pwsh scripts/build-bridge.ps1 -Verify              # 只校验已有产物与哈希记录
#   pwsh scripts/build-bridge.ps1 -Force               # 产物已存在时强制覆盖
#   pwsh scripts/build-bridge.ps1 -BunPath <路径>       # 指定 bun 实体（默认 magpie 自带 1.4.4）
#
# 产物:
#   dist/magpie-bridge.exe        单文件（约 110MB，.gitignore 忽略）
#   dist/magpie-bridge.sha256     哈希记录（入库：.gitignore 里用 ! 排除了它）

[CmdletBinding()]
param(
    # magpie 自带的 bun 实体（F-11：~/.cache/magpie/bun/ 1.4.4，110MB 运行时）。
    # PATH 上的 `bun` 是 npm shim（1.3.14），不是它。
    [string]$BunPath = "$env:USERPROFILE\.cache\magpie\bun\1.4.4\magpie-bun.exe",
    [switch]$Verify,
    [switch]$Force
)

$ErrorActionPreference = 'Stop'

$root   = Split-Path -Parent $PSScriptRoot            # scripts/ 的上级 = 仓库根
$bridge = Join-Path $root 'opchost\bridge'
$dist   = Join-Path $root 'dist'
$entry  = Join-Path $bridge 'src\main.ts'
$exe    = Join-Path $dist 'magpie-bridge.exe'
$shaFile = Join-Path $dist 'magpie-bridge.sha256'

function Get-BridgeSha256([string]$Path) {
    (Get-FileHash -Algorithm SHA256 -Path $Path).Hash.ToLowerInvariant()
}

# ---- -Verify：只复核 ----
if ($Verify) {
    if (-not (Test-Path $exe))    { Write-Error "VERIFY FAIL: 产物不存在 $exe"; exit 1 }
    if (-not (Test-Path $shaFile)){ Write-Error "VERIFY FAIL: 哈希记录不存在 $shaFile"; exit 1 }
    $first = (Get-Content $shaFile -TotalCount 1 -EA Stop)
    $stored = ($first -split '\s+', 2)[0].Trim()
    if ($stored.Length -ne 64) { Write-Error "VERIFY FAIL: 哈希记录首行不是 sha256（$first）"; exit 1 }
    $actual = Get-BridgeSha256 $exe
    if ($stored -ceq $actual) {
        Write-Host "VERIFY OK   $actual"
        exit 0
    }
    Write-Error "VERIFY FAIL  stored=$stored`nVERIFY FAIL  actual=$actual"
    exit 1
}

# ---- 构建 ----
if (-not (Test-Path $BunPath)) {
    Write-Error "bun 实体不存在: $BunPath（请先装 magpie，或传 -BunPath 指定实体）"
    exit 1
}
if (-not (Test-Path $entry)) {
    Write-Error "桥入口不存在: $entry"
    exit 1
}
$bunVersion = & $BunPath --version
Write-Host "bun: $bunVersion ($BunPath)"
Write-Host "entry: $entry"

New-Item -ItemType Directory -Force -Path $dist | Out-Null
if (Test-Path $exe) {
    if (-not $Force) {
        Write-Error "产物已存在: $exe`n删除它或用 -Force 覆盖（-Verify 只校验不构建）"
        exit 1
    }
    Remove-Item $exe -Force
}

Push-Location $bridge
try {
    & $BunPath build --compile $entry --outfile $exe
    if ($LASTEXITCODE -ne 0) { throw "bun build 退出码 $LASTEXITCODE" }
} finally {
    Pop-Location
}

$hash = Get-BridgeSha256 $exe
$size = (Get-Item $exe).Length
$stamp = Get-Date -Format 's'
$record = "$hash  $([IO.Path]::GetFileName($exe))"
Set-Content -Path $shaFile -Value @(
    $record
    "# built=$stamp  bun=$bunVersion  size=$size bytes"
) -Encoding ascii

Write-Host "OK  exe=$exe"
Write-Host "OK  size=$size bytes ($([math]::Round($size / 1MB, 1)) MB)"
Write-Host "OK  sha256=$hash"
Write-Host "OK  record=$shaFile"
Write-Host "提示: 修改起点为 $bridge（--plugin-dir 由宿主在 entry.args 里给）"