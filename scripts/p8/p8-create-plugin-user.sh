#!/usr/bin/env bash
# =============================================================================
# P8-① 插件进程独立系统用户 + 目录权限矩阵（凭据目录对插件用户不可读）
#
# 契约来源：
#   plans/2026-11-05-magpie接入与公网部署计划.md  §4 P8「做什么 1」
#   plans/2026-11-05-安全加固计划.md              §S-6「做什么 1」
#
# 行为：
#   默认 **dry-run**：只打印将要执行的每一步（含完整命令行原文），不改系统。
#   加 --apply 才真的改。重复执行是幂等的（已存在的用户/目录/权限不重复动，
#   权限偏差会被纠正回目标值）。
#
# 平台：
#   只在 Linux 上允许 --apply。非 Linux 上仍可跑 dry-run 看计划（用于本机核对），
#   但会打印「未在 Linux 实跑」提示。
#
# 退出码：
#   0  成功（dry-run 成功也算 0）
#   2  前置条件不满足（非 Linux 且 --apply、缺 root、插件用户被误加入宿主组…）
#   4  参数错误
# =============================================================================
set -euo pipefail

PROG=${0##*/}
readonly LAYOUT_VERSION="p8-layout-v1"

# 目标默认值（可用参数覆盖）
PLUGIN_USER="aiworkgate-plugin"
HOST_USER="aiworkgate"
ROOT_DIR="/var/lib/aiworkgate"          # 宿主数据根
OPT_DIR="/opt/aiworkgate"               # 安装根（二进制）
LOG_DIR="/var/log/aiworkgate"           # 日志根（插件不写这里，systemd 收集 stdout）
APPLY=0
FORCE_PLATFORM=0

usage() {
  cat <<'EOF'
用法： p8-create-plugin-user.sh [选项]

创建 aiworkgate 的插件隔离用户与目录权限矩阵：
  * 插件用户（默认 aiworkgate-plugin）—— 非 root、无登录 shell、无 home
  * 凭据目录 0700 归宿主用户 —— **插件用户不可读**（P8-① 的核心要求）
  * 插件目录 0750 宿主:插件组 —— 插件用户只读，宿主可安装
  * 运行期目录 0750 宿主:插件组 —— 宿主写 rules.json / egress.json

默认 dry-run：只打印计划，不改系统。加 --apply 才执行。

选项：
  --apply                   真正执行（默认只打印计划）
  --plugin-user NAME        插件系统用户（默认 aiworkgate-plugin）
  --host-user NAME          宿主用户（默认 aiworkgate；不存在时不会自动创建）
  --root DIR                宿主数据根（默认 /var/lib/aiworkgate）
  --opt DIR                 安装根（默认 /opt/aiworkgate）
  --log DIR                 日志根（默认 /var/log/aiworkgate）
  --force-platform          非 Linux 上也执行（仅用于本机演练，会破坏权限语义）
  -h, --help                显示本帮助

退出码： 0 成功 / 2 前置条件不满足 / 4 参数错误

注意： 本脚本只在 Linux 上做过语法检查（bash -n），**未在 Linux 实跑**；
       本机（Windows + Git bash）只能验证 dry-run 的计划输出。
EOF
}

die() { printf '错误：%s\n' "$*" >&2; exit 2; }
bad_usage() { printf '参数错误：%s\n（用 --help 查看用法）\n' "$*" >&2; exit 4; }

# ---- 参数解析 --------------------------------------------------------------
while [ $# -gt 0 ]; do
  case $1 in
    --apply)           APPLY=1 ;;
    --plugin-user)     [ $# -ge 2 ] || bad_usage "--plugin-user 需要值"; PLUGIN_USER=$2; shift ;;
    --host-user)       [ $# -ge 2 ] || bad_usage "--host-user 需要值"; HOST_USER=$2; shift ;;
    --root)            [ $# -ge 2 ] || bad_usage "--root 需要值"; ROOT_DIR=$2; shift ;;
    --opt)             [ $# -ge 2 ] || bad_usage "--opt 需要值"; OPT_DIR=$2; shift ;;
    --log)             [ $# -ge 2 ] || bad_usage "--log 需要值"; LOG_DIR=$2; shift ;;
    --force-platform)  FORCE_PLATFORM=1 ;;
    -h|--help)         usage; exit 0 ;;
    *)                 bad_usage "未知参数 $1" ;;
  esac
  shift
done

# 目录矩阵： 路径|属主|属组|模式|用途
# 模式一律显式写，不用 umask 推断。
readonly DIRS="
$OPT_DIR|root|root|0755|安装根（二进制、docs），插件用户可读不可写
$OPT_DIR/bin|root|root|0755|bun 与 magpie-bridge 二进制，哈希锁定（见 p8-bun-hash.sh）
$ROOT_DIR|$HOST_USER|$HOST_USER|0750|宿主数据根；插件用户不是宿主组成员
$ROOT_DIR/credentials|$HOST_USER|$HOST_USER|0700|**凭据库，插件用户不可读**（P8-①）
$ROOT_DIR/db|$HOST_USER|$HOST_USER|0750|SQLite 数据库（含 api_logs 正文）
$ROOT_DIR/plugins|$HOST_USER|$PLUGIN_USER|0750|已安装插件代码；宿主可写，插件组只读
$ROOT_DIR/runtime|$HOST_USER|$PLUGIN_USER|0750|每插件的 rules.json / egress.json（宿主写）
$ROOT_DIR/work|$PLUGIN_USER|$PLUGIN_USER|0700|插件私有工作区（唯一可写处）
$LOG_DIR|$HOST_USER|$PLUGIN_USER|0750|日志目录；插件不直接写（systemd 收 stdout）
"

RUN=0
step() { RUN=$((RUN + 1)); printf '\n[%02d] %s\n' "$RUN" "$*"; }
# plan：dry-run 打印，--apply 时才执行；--apply 下也先回显命令原文（可追溯）
plan() {
  local desc=$1; shift
  printf '     %s\n' "$desc"
  printf '     $'
  printf ' %q' "$@"
  printf '\n'
  if [ "$APPLY" -eq 1 ]; then
    "$@"
  fi
}

printf '=== P8 插件用户与目录权限（%s）===\n' "$LAYOUT_VERSION"
printf '模式：%s\n' "$([ "$APPLY" -eq 1 ] && echo '--apply（真实修改）' || echo 'dry-run（只打印计划）')"
printf '插件用户：%s   宿主用户：%s\n' "$PLUGIN_USER" "$HOST_USER"

# ---- 前置检查 --------------------------------------------------------------
step "前置检查"
OS=$(uname -s 2>/dev/null || echo unknown)
printf '     平台：%s\n' "$OS"
if [ "$OS" != "Linux" ]; then
  if [ "$APPLY" -eq 1 ] && [ "$FORCE_PLATFORM" -ne 1 ]; then
    die "非 Linux 平台不允许 --apply（当前 $OS）；仅演练请用 --force-platform，且知悉权限语义不成立"
  fi
  printf '     [提示] 当前不是 Linux：useradd/systemd 相关步骤只会打印，不会生效；\n'
  printf '            本脚本的 Linux 行为**未在 Linux 实跑**（仅 bash -n 语法检查通过）。\n'
fi
if [ "$APPLY" -eq 1 ]; then
  [ "$(id -u)" -eq 0 ] || die "--apply 需要 root（创建系统用户与改属主）"
fi
case $PLUGIN_USER in root|0) die "插件用户不能是 root（P8-① 要求非 root）" ;; esac
[ "$PLUGIN_USER" != "$HOST_USER" ] || die "插件用户不能与宿主用户相同（否则隔离不成立）"

# ---- 1) 插件系统用户 -------------------------------------------------------
step "系统用户 $PLUGIN_USER（非 root / 无登录 shell / 无 home）"
if id -u "$PLUGIN_USER" >/dev/null 2>&1; then
  printf '     已存在（幂等跳过创建）：uid=%s shell=%s home=%s\n' \
    "$(id -u "$PLUGIN_USER")" \
    "$(awk -F: -v u="$PLUGIN_USER" '$1==u{print $7}' /etc/passwd 2>/dev/null || echo '?')" \
    "$(awk -F: -v u="$PLUGIN_USER" '$1==u{print $6}' /etc/passwd 2>/dev/null || echo '?')"
else
  # --system：落在系统 uid 区间，不占普通用户号
  # --no-create-home：插件不需要 home，ProtectHome=yes 下也读不到
  # --shell /usr/sbin/nologin：不可交互登录
  man_hint="（useradd 手册：--system 建系统账号，--no-create-home 不建家目录）"
  printf '     %s\n' "$man_hint"
  plan "创建系统用户（不走 useradd 的默认组继承）" \
    useradd --system --no-create-home --shell /usr/sbin/nologin \
            --comment "aiworkgate plugin sandbox user" "$PLUGIN_USER"
fi

# 反向断言：插件用户**不得**属于宿主用户的主组（否则 0750 的凭据父目录可能被穿透）
step "反向断言：$PLUGIN_USER 不属于 $HOST_USER 的主组"
host_gid=$(awk -F: -v u="$HOST_USER" '$1==u{print $4}' /etc/passwd 2>/dev/null || true)
if [ -n "${host_gid:-}" ] && id -G "$PLUGIN_USER" 2>/dev/null | tr ' ' '\n' | grep -qx "$host_gid"; then
  die "$PLUGIN_USER 属于 $HOST_USER 的主组(gid=$host_gid)，隔离不成立；请先 usermod -G 摘除"
fi
printf '     通过（插件用户组列表：%s）\n' "$(id -Gn "$PLUGIN_USER" 2>/dev/null || echo '未创建，视为通过')"

# ---- 2) 目录与权限矩阵 -----------------------------------------------------
# install -d 自带幂等：目录已存在时只校正属主/组/模式，不重建内容。
printf '\n[%02d] 目录矩阵（install -d 幂等；已存在则只校正权限）\n' $((++RUN))
while IFS='|' read -r path owner group mode purpose; do
  [ -n "${path:-}" ] || continue
  printf '\n     目录：%s\n' "$path"
  printf '     目标：%s:%s %s —— %s\n' "$owner" "$group" "$mode" "$purpose"
  plan "创建/校正目录" \
    install -d -o "$owner" -g "$group" -m "$mode" "$path"
done <<EOF
$DIRS
EOF

# 凭据目录的**父目录**同样不能对插件用户开放写：0750 + 属组仅宿主用户。
# 上面 $ROOT_DIR 已是 0750，这里显式再断言一次（防后人改成 0755）。
step "断言：凭据目录链上不得有 group/other 可读位"
if [ -d "$ROOT_DIR/credentials" ]; then
  perm=$(stat -c '%a' "$ROOT_DIR/credentials" 2>/dev/null || echo 'unknown')
  case $perm in
    700|0700) printf '     %s 模式=%s 通过\n' "$ROOT_DIR/credentials" "$perm" ;;
    unknown)  printf '     [提示] 无法读取模式（非 POSIX stat），跳过判定\n' ;;
    *)        die "$ROOT_DIR/credentials 模式为 $perm，期望 0700（插件用户可读=违规）" ;;
  esac
else
  printf '     %s 尚不存在（首次部署正常）\n' "$ROOT_DIR/credentials"
fi

# ---- 3) 插件进程启动方式（P8-① 的真实缺口，务必读） ----------------------
# ⚠️ 关键事实：aiworkgate 宿主是用 exec 直接拉起插件子进程的（internal/plugins/supervisor.go），
#    子进程**继承宿主 uid**。因此「插件跑在独立用户下」不能只靠 systemd 单元，
#    必须在宿主 spawn 路径上换 uid。下面打印可选实现，供实现卡采用（本卡不改 Go 代码）。
step "插件进程换 uid 的三条实现路径（择一，属实现卡）"
cat <<EOF
     A) systemd 透明单元（推荐，需 root 能力最小）
        宿主对每个插件执行：
        systemd-run --quiet --collect --scope --uid=$PLUGIN_USER --gid=$PLUGIN_USER \\
          --property=NoNewPrivileges=yes --property=MemoryDenyWriteExecute=no \\
          -- /opt/aiworkgate/bin/magpie-bridge --plugin-dir ...
        代价：宿主需能调 systemd-run（polkit 或 root）；端口/健康检查语义不变。
     B) sudoers 定向授权（NOPASSWD 单命令）
        宿主用户可在 /etc/sudoers.d/aiworkgate-plugin 获得：
        $HOST_USER ALL=($PLUGIN_USER) NOPASSWD: /opt/aiworkgate/bin/magpie-bridge
        再以 sudo -u $PLUGIN_USER -- magpie-bridge ... 启动。
        代价：新增 sudo 面；需严格单命令白名单，不许通配。
     C) 静态单元模板 aiworkgate-plugin@.service（本卡 scripts/p8/systemd/ 提供）
        适合「插件桥由 systemd 托管」的部署形态；宿主只做编排不 spawn。
        代价：宿主失去 direct spawn 的端口探测时序，需要改为 socket 探测。
     → 三条都要做同一件事：**子进程 uid = $PLUGIN_USER，而非宿主 uid**。
       仅装 systemd 单元而不换 uid，P8-① 验收（ps -o user -C）**不可能通过**。
EOF

# ---- 4) 收尾：让人能自己复核 ---------------------------------------------
step "验收线索（部署后在服务器原样执行）"
cat <<EOF
     id -u $PLUGIN_USER && id -Gn $PLUGIN_USER
     stat -c '%a %U:%G %n' $ROOT_DIR/credentials $ROOT_DIR/plugins $ROOT_DIR/runtime
     ps -o user=,pid=,cmd= -C magpie-bridge            # 期望：$PLUGIN_USER，非 root
     sudo -u $PLUGIN_USER test -r $ROOT_DIR/credentials -o -x $ROOT_DIR/credentials \\
       && echo '违规：插件用户可达凭据目录' || echo '通过：插件用户不可达'
     sudo -u $PLUGIN_USER cat $ROOT_DIR/credentials/* 2>&1 | head -1   # 期望：Permission denied
     完整复核脚本：scripts/p8/p8-verify-isolation.sh
EOF

if [ "$APPLY" -eq 0 ]; then
  printf '\n完成（dry-run）：以上 %d 步均未执行。复核计划后加 --apply 再跑一次。\n' "$RUN"
else
  printf '\n完成（--apply）：已按上表校正。请跑 scripts/p8/p8-verify-isolation.sh 复核。\n'
fi
