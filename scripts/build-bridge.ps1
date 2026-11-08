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
    [switch]$Force,
    # task-78：跳过源码/exe mtime 守卫（默认拒绝在源码比产物新时构建）。
    [switch]$SkipStaleCheck
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

# ---- task-78：源码/exe mtime 守卫 ----
# 防「旧 exe 静默缺新功能」坑（task-75 实测：dist exe 早于 magpie-auth.ts 提交，
# 桥缺 magpie 登录导入且无任何报错）。产物存在且最新源码 mtime 比它新时：
# 默认拒绝构建并提示 -Force；-SkipStaleCheck 显式跳过（必须回显）。
function Test-BridgeSourceStale {
    if (-not (Test-Path $exe)) { return $false }
    $exeTime = (Get-Item $exe).LastWriteTimeUtc
    $newest = Get-ChildItem (Join-Path $bridge 'src') -Recurse -File -Include '*.ts', '*.mjs' |
        Sort-Object LastWriteTimeUtc -Descending | Select-Object -First 1
    if ($null -eq $newest) { return $false }
    return $newest.LastWriteTimeUtc -gt $exeTime
}

if (-not $Verify -and -not $SkipStaleCheck -and (Test-BridgeSourceStale)) {
    $newest = Get-ChildItem (Join-Path $bridge 'src') -Recurse -File -Include '*.ts', '*.mjs' |
        Sort-Object LastWriteTimeUtc -Descending | Select-Object -First 1
    Write-Error ("STALE BUILD REFUSED: 源码比 exe 新，请 -Force 重建`n" +
        "  newest source : $($newest.FullName) ($($newest.LastWriteTimeUtc.ToString('s')))`n" +
        "  existing exe  : $exe ($((Get-Item $exe).LastWriteTimeUtc.ToString('s')))`n" +
        "  跳过本守卫：-SkipStaleCheck（须自证产物可接受）")
    exit 1
}
if ($SkipStaleCheck) {
    Write-Host "WARNING: -SkipStaleCheck 生效，源码/exe mtime 守卫已跳过"
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