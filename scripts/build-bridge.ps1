# build-bridge.ps1 — P1/P8 收尾（task-18）+ 多目标构建链（task-88）
#
# 把 opchost/bridge（bun/TypeScript 插件宿主桥）编译成单文件二进制，并记录 SHA256
# （对齐 P8-⑥「bun 二进制随包内置并记录哈希，禁止运行时下载」）。
#
# 用法:
#   pwsh scripts/build-bridge.ps1                                # windows 目标构建 + 写哈希记录
#   pwsh scripts/build-bridge.ps1 -Target linux -Force           # Linux 交叉编译（ELF）
#   pwsh scripts/build-bridge.ps1 -Target all -Force             # 两目标都构建
#   pwsh scripts/build-bridge.ps1 -Verify                        # 只校验已有产物与哈希记录
#   pwsh scripts/build-bridge.ps1 -Force                         # 产物已存在时强制覆盖
#   pwsh scripts/build-bridge.ps1 -BunPath <路径>                 # 指定 bun 实体（默认 magpie 自带 1.4.4）
#
# 产物（task-88 起分目标）:
#   windows: dist/magpie-bridge.exe        单文件（约 110MB，.gitignore 忽略）
#            dist/magpie-bridge.sha256     哈希记录（入库：.gitignore 里用 ! 排除了它）
#   linux:   dist/magpie-bridge-linux      单文件 ELF（bun-linux-x64，.gitignore 忽略）
#            dist/magpie-bridge-linux.sha256  哈希记录（入库）
#   mtime 守卫（task-85 收窄：排除 testdata）对两目标共用——源码比任一**已存在**
#   产物新即拒绝构建（自判依据：缺一个产物不该让另一个目标的守卫失效）。

[CmdletBinding()]
param(
    # magpie 自带的 bun 实体（F-11：~/.cache/magpie/bun/ 1.4.4，110MB 运行时）。
    # PATH 上的 `bun` 是 npm shim（1.3.14），不是它。
    [string]$BunPath = "$env:USERPROFILE\.cache\magpie\bun\1.4.4\magpie-bun.exe",
    [ValidateSet('windows', 'linux', 'all')]
    [string]$Target = 'windows',
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

function Get-BridgeSha256([string]$Path) {
    (Get-FileHash -Algorithm SHA256 -Path $Path).Hash.ToLowerInvariant()
}

# ---- 多目标产物定义（task-88）----
$targets = @{
    windows = @{
        outFile = Join-Path $dist 'magpie-bridge.exe'
        shaFile = Join-Path $dist 'magpie-bridge.sha256'
        bunTarget = ''   # 空 = 本机默认目标
    }
    linux = @{
        outFile = Join-Path $dist 'magpie-bridge-linux'
        shaFile = Join-Path $dist 'magpie-bridge-linux.sha256'
        bunTarget = '--target=bun-linux-x64'
    }
}
$targetNames = switch ($Target) {
    'all' { @('windows', 'linux') }
    default { @($Target) }
}

# ---- task-78：源码/产物 mtime 守卫（task-85 收窄扫描面；task-88 扩展到多目标）----
# 源码比**任一已存在**产物新即拒绝——缺一个产物不该让另一个目标的守卫失效，
# 因为两个产物出自同一份源码，源码更新了就该一起重建。
function Test-BridgeSourceStale {
    $newest = Get-BridgeRuntimeSource | Sort-Object LastWriteTimeUtc -Descending | Select-Object -First 1
    if ($null -eq $newest) { return $false }
    foreach ($name in $targetNames) {
        $outFile = $targets[$name].outFile
        if (-not (Test-Path $outFile)) { continue }   # 产物不存在 = 无从比较，交由"产物已存在"分支
        if ($newest.LastWriteTimeUtc -gt (Get-Item $outFile).LastWriteTimeUtc) { return $true }
    }
    return $false
}

function Get-BridgeRuntimeSource {
    Get-ChildItem (Join-Path $bridge 'src') -Recurse -File -Include '*.ts', '*.mjs' | Where-Object {
        $rel = $_.FullName.Substring((Join-Path $bridge 'src').Length + 1)
        $excluded = $false
        foreach ($dir in $script:BridgeSourceDirsToExclude) {
            if ($rel.StartsWith($dir, [StringComparison]::OrdinalIgnoreCase)) { $excluded = $true; break }
        }
        -not $excluded
    }
}

# ---- task-78：源码/产物 mtime 守卫 ----
# 防「旧二进制静默缺新功能」坑（task-75 实测：dist exe 早于 magpie-auth.ts 提交，
# 桥缺 magpie 登录导入且无任何报错）。拒绝构建并提示 -Force；-SkipStaleCheck
# 显式跳过（必须回显）。
$script:BridgeSourceDirsToExclude = @('admin\testdata')

if (-not $Verify -and -not $SkipStaleCheck -and (Test-BridgeSourceStale)) {
    $newest = Get-BridgeRuntimeSource | Sort-Object LastWriteTimeUtc -Descending | Select-Object -First 1
    $existing = $targetNames | Where-Object { Test-Path $targets[$_].outFile } |
        ForEach-Object { "$_ : $((Get-Item $targets[$_].outFile).LastWriteTimeUtc.ToString('s'))" }
    Write-Error ("STALE BUILD REFUSED: 源码比产物新，请 -Force 重建`n" +
        "  newest source : $($newest.FullName) ($($newest.LastWriteTimeUtc.ToString('s')))`n" +
        "  existing      : $($existing -join '  |  ')`n" +
        "  跳过本守卫：-SkipStaleCheck（须自证产物可接受）")
    exit 1
}
if ($SkipStaleCheck) {
    Write-Host "WARNING: -SkipStaleCheck 生效，源码/产物 mtime 守卫已跳过"
}

# ---- 前置校验 ----
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
Write-Host "target(s): $($targetNames -join ', ')"

# ---- -Verify：只复核（校验目标产物与哈希记录一致）----
if ($Verify) {
    $allOk = $true
    foreach ($name in $targetNames) {
        $outFile = $targets[$name].outFile
        $shaFile = $targets[$name].shaFile
        if (-not (Test-Path $outFile))    { Write-Error "VERIFY FAIL($name): 产物不存在 $outFile"; $allOk = $false; continue }
        if (-not (Test-Path $shaFile))    { Write-Error "VERIFY FAIL($name): 哈希记录不存在 $shaFile"; $allOk = $false; continue }
        $first = (Get-Content $shaFile -TotalCount 1 -EA Stop)
        $stored = ($first -split '\s+', 2)[0].Trim()
        if ($stored.Length -ne 64) { Write-Error "VERIFY FAIL($name): 哈希记录首行不是 sha256（$first）"; $allOk = $false; continue }
        $actual = Get-BridgeSha256 $outFile
        if ($stored -ceq $actual) {
            Write-Host "VERIFY OK($name)   $actual"
        } else {
            Write-Error "VERIFY FAIL($name)  stored=$stored`nVERIFY FAIL($name)  actual=$actual"
            $allOk = $false
        }
    }
    if ($allOk) { exit 0 }
    exit 1
}

# ---- 构建 ----
New-Item -ItemType Directory -Force -Path $dist | Out-Null
$stamp = Get-Date -Format 's'
Push-Location $bridge
try {
    foreach ($name in $targetNames) {
        $t = $targets[$name]
        $outFile = $t.outFile
        if (Test-Path $outFile) {
            if (-not $Force) {
                Write-Error "产物已存在($name): $outFile`n删除它或用 -Force 覆盖（-Verify 只校验不构建）"
                exit 1
            }
            Remove-Item $outFile -Force
        }
        $bunArgs = @('build', '--compile')
        if ($t.bunTarget -ne '') { $bunArgs += $t.bunTarget }
        $bunArgs += $entry
        $bunArgs += '--outfile'
        $bunArgs += $outFile
        Write-Host "building $name -> $outFile"
        & $BunPath @bunArgs
        if ($LASTEXITCODE -ne 0) { throw "bun build($name) 退出码 $LASTEXITCODE" }

        $hash = Get-BridgeSha256 $outFile
        $size = (Get-Item $outFile).Length
        $record = "$hash  $([IO.Path]::GetFileName($outFile))"
        Set-Content -Path $t.shaFile -Value @(
            $record
            "# built=$stamp  bun=$bunVersion  size=$size bytes"
        ) -Encoding ascii

        Write-Host "OK($name)  out=$outFile"
        Write-Host "OK($name)  size=$size bytes ($([math]::Round($size / 1MB, 1)) MB)"
        Write-Host "OK($name)  sha256=$hash"
        Write-Host "OK($name)  record=$($t.shaFile)"
    }
} finally {
    Pop-Location
}
Write-Host "提示: 修改起点为 $bridge（--plugin-dir 由宿主在 entry.args 里给）"
