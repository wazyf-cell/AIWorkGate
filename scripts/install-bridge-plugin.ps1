# install-bridge-plugin.ps1 — task-78：桥插件实例安装脚本（task-75/76 配方固化）
#
# 把一个 magpie 渠道接入 aiworkgate 桥插件体系：在目标实例的 plugins/ 下落
# bridge-<id>/ 目录（magpie-bridge.exe + magpie 插件包只读拷贝 + golden 形状
# manifest.json），入口/参数/凭据隔离全部符合 internal/opchost 的契约。
#
# 幂等：已存在的 bridge-<id>/ 只同步 exe 与 manifest，**绝不触碰**
# plugin-data/<id>（credentials.json 等凭据）与 magpie 本体（只读源）。
#
# 用法:
#   pwsh scripts/install-bridge-plugin.ps1 -Channel trae -Target preview
#   pwsh scripts/install-bridge-plugin.ps1 -Channel trae -Target preview -DryRun
#   pwsh scripts/install-bridge-plugin.ps1 -Channel trae -Target 'D:\instances\formal'
#   pwsh scripts/install-bridge-plugin.ps1 -Channel trae -Target preview -Domains 'trae.ai','*.trae.ai'
#
# -Target: preview（runtime/preview-appdata/AIWorkGate）| formal（占位：正式实例
# 目录确定后在此补映射，当前报错）| 任意绝对路径（实例的 AIWorkGate 数据目录）。
#
# 域名默认从 magpie 插件包内 SITES/https 引用自动枚举；-Domains 显式覆盖。
# 落盘后需重启宿主（或等 plugins/Discover）才会发现新插件——本脚本不重启。

[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Channel,
    [Parameter(Mandatory)][string]$Target,
    [string[]]$Domains,
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$exe  = Join-Path $repo 'dist\magpie-bridge.exe'
$pkg  = Join-Path $env:USERPROFILE ".config\magpie\plugins\node_modules\@magpie-community\opencode-$Channel-auth"

function Say([string]$m) { Write-Host "[install-bridge] $m" }

# ---- 前置校验 ----
if ($Channel -notmatch '^[a-z0-9][a-z0-9-]{1,31}$') {
    Write-Error "Channel '$Channel' 不符合插件 id 规则（^[a-z0-9][a-z0-9-]{1,31}$）"; exit 4
}
if (-not (Test-Path $exe)) {
    Write-Error "桥 exe 不存在: $exe（先跑 scripts/build-bridge.ps1）"; exit 4
}
if (-not (Test-Path $pkg)) {
    Write-Error "magpie 插件包不存在: $pkg"; exit 4
}

# exe 守卫与 build-bridge 同口径：源码比产物新时拒绝（-DryRun 只警告）。
$exeTime = (Get-Item $exe).LastWriteTimeUtc
$newest = Get-ChildItem (Join-Path $repo 'opchost\bridge\src') -Recurse -File -Include '*.ts', '*.mjs' |
    Sort-Object LastWriteTimeUtc -Descending | Select-Object -First 1
if ($null -ne $newest -and $newest.LastWriteTimeUtc -gt $exeTime) {
    $msg = "STALE EXE: 源码 $($newest.Name) 比 dist exe 新——请先 build-bridge.ps1 -Force 重建"
    if ($DryRun) { Say "WARNING( DryRun 不阻断 ): $msg" } else { Write-Error $msg; exit 4 }
}

# ---- 目标解析 ----
switch -Wildcard ($Target) {
    'preview' { $dataDir = Join-Path $repo 'runtime\preview-appdata\AIWorkGate' }
    'formal'  {
        # task-82：正式实例 DataDir = %APPDATA%\AIWorkGate（task-81 实测
        # config port=17865；单实例 mutex 与预览互斥，落盘后由 Lead 择机启动）。
        # manifest 的 --plugin-dir 将指向正式目录绝对路径，绝不复用预览路径。
        $dataDir = Join-Path $env:APPDATA 'AIWorkGate'
        if (-not (Test-Path (Join-Path $dataDir 'config.json'))) {
            Write-Error "formal 实例目录异常：$dataDir 下没有 config.json（确认这是正式实例的 DataDir？）"; exit 4
        }
    }
    default   {
        if ([IO.Path]::IsPathRooted($Target) -and (Test-Path $Target)) { $dataDir = $Target }
        else { Write-Error "Target '$Target' 不是 preview/formal，也不是存在的绝对路径"; exit 4 }
    }
}
$pluginDir = Join-Path $dataDir "plugins\bridge-$Channel"
$existing  = Test-Path $pluginDir

# ---- 域名 ----
if (-not $Domains -or $Domains.Count -eq 0) {
    $indexMjs = Join-Path $pkg 'index.mjs'
    $urls = Select-String -Path $indexMjs -Pattern 'https://[A-Za-z0-9.-]+' -AllMatches |
        ForEach-Object { $_.Matches } | ForEach-Object { $_.Value }
    $hosts = $urls | ForEach-Object { ($_ -replace '^https://', '') } | Sort-Object -Unique
    $list = @()
    foreach ($host_ in $hosts) {
        if ($host_ -match '^\*') { $list += $host_ }
        else { $list += $host_; $list += "*.$host_" }
    }
    $Domains = @($list | Select-Object -Unique)
}
if (-not $Domains -or $Domains.Count -eq 0) { $Domains = @("$Channel.example.invalid") }

# ---- 动作清单 ----
$actions = @()
if (-not $existing) {
    $actions += "CREATE  $pluginDir"
    $actions += "COPY    magpie 插件包内容 -> $pluginDir\（只读源：$pkg）"
    $actions += "COPY    dist exe -> $pluginDir\magpie-bridge.exe"
    $actions += "WRITE   manifest.json（entry.exe=magpie-bridge.exe, args=--plugin-dir $pluginDir, capabilities=[upstream,accounts,checkin], domains=$($Domains -join ','))"
} else {
    $actions += "SYNC    $pluginDir\magpie-bridge.exe（已存在目录：仅同步 exe）"
    $actions += "WRITE   manifest.json（重写，幂等）"
    $actions += "KEEP    $pluginDir 已有文件不动（credentials 等由宿主 plugin-data/<id> 承载，本脚本从不触碰）"
}
$actions += "RESTART 提示：落盘后需重启宿主（或等 plugins/Discover）才会发现/刷新插件——本脚本不重启"

Say "channel=$Channel target=$dataDir existing=$existing dryRun=$DryRun"
foreach ($action in $actions) { Say $action }
if ($DryRun) {
    Say "DRYRUN 结束：未落盘任何文件"
    exit 0
}

# ---- 落盘 ----
New-Item -ItemType Directory -Force -Path $pluginDir | Out-Null
Copy-Item (Join-Path $pkg '*') $pluginDir -Recurse -Force
Say "copied magpie package ($(($(Get-ChildItem $pluginDir -File).Count)) files in dir)"

# exe：运行中被锁时用 rename 部署（task-75/76 实测技巧：Windows 允许重命名
# 运行中的 exe，覆盖不行）。
$target = Join-Path $pluginDir 'magpie-bridge.exe'
if (Test-Path $target) {
    $srcHash = (Get-FileHash $exe).Hash
    $dstHash = (Get-FileHash $target -ErrorAction SilentlyContinue).Hash
    if ($srcHash -eq $dstHash) { Say "exe unchanged（跳过拷贝）" }
    else {
        $old = "$target.old"
        if (Test-Path $old) { Remove-Item $old -Force -ErrorAction SilentlyContinue }
        Rename-Item $target $old -Force
        Copy-Item $exe $target -Force
        Say "exe synced（旧 exe 保留为 magpie-bridge.exe.old，重启宿主后可删）"
    }
} else {
    Copy-Item $exe $target -Force
    Say "exe written"
}

# manifest（golden 形状，--plugin-dir 用目标绝对路径）
$manifest = [ordered]@{
    id          = "bridge-$Channel"
    name        = "$(($Channel -replace '^.', { $_.ToString().ToUpper() })) Bridge"
    version     = '0.1.0'
    api_level   = 1
    description = "aiworkgate plugin contract bridge over the magpie opencode-$Channel-auth plugin"
    capabilities = @('upstream', 'accounts', 'checkin')
    permissions = [ordered]@{ network_domains = @($Domains) }
    checkin = [ordered]@{}; quota = [ordered]@{}; concurrency = [ordered]@{}
    rules = [ordered]@{}; egress = [ordered]@{}
    startup = [ordered]@{ mode = 'always' }
    entry = [ordered]@{ exe = 'magpie-bridge.exe'; args = @('--plugin-dir', $pluginDir) }
}
[IO.File]::WriteAllText(
    (Join-Path $pluginDir 'manifest.json'),
    ($manifest | ConvertTo-Json -Depth 6),
    (New-Object System.Text.UTF8Encoding($false))
)
Say "manifest written"

# 幂等证明：credentials 始终在宿主 plugin-data，脚本从未指向那里。
$credDir = Join-Path $dataDir "plugin-data\bridge-$Channel"
if (Test-Path $credDir) {
    Say "KEEP    $credDir（凭据数据未触碰）"
} else {
    Say "NOTE    $credDir 尚不存在（首次安装属正常，宿主启动后由桥创建）"
}
Say "DONE    bridge-$Channel 已就绪；重启宿主后生效"
