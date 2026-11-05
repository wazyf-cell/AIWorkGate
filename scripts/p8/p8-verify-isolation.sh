#!/usr/bin/env bash
# =============================================================================
# P8-① 验收复核：插件进程用户非 root、凭据目录对插件用户不可读
#
# 对应验收项：
#   plans/2026-11-05-magpie接入与公网部署计划.md §P8 验收 1
#   plans/2026-11-05-安全加固计划.md §S-6 怎么验
#
# 本脚本只读，不改任何东西。在**服务器**上以 root 或具备 sudo 的用户执行。
#
# 退出码：
#   0  全部通过
#   1  至少一项 FAIL
#   2  环境不支持（无 runuser/sudo 时负向测试被跳过；该情况单独打印 SKIP）
#   4  参数错误
# =============================================================================
set -euo pipefail

PROG=${0##*/}
PLUGIN_USER="aiworkgate-plugin"
CRED_DIR="/var/lib/aiworkgate/credentials"
PROC_NAMES="magpie-bridge aiworkgate-plugin"
VERBOSE=0

usage() {
  cat <<EOF
用法： $PROG [选项]

P8-① 验收复核（只读）：
  1. 插件用户存在、uid != 0
  2. 运行中的插件进程用户是插件用户（非 root、非宿主用户）
  3. 凭据目录模式 0700 且属主 != 插件用户
  4. 插件用户实际尝试读凭据目录 -> 必须 Permission denied（负向测试）
  5. 插件目录链上无 group/other 可写

选项：
  --plugin-user NAME   插件用户（默认 aiworkgate-plugin）
  --cred-dir DIR       凭据目录（默认 /var/lib/aiworkgate/credentials）
  --proc NAMES         进程名（空格分隔，默认 "magpie-bridge aiworkgate-plugin"）
  --verbose            打印每步细节
  -h, --help           显示本帮助

退出码： 0 通过 / 1 失败 / 2 环境不支持 / 4 参数错误
注意： 本脚本只在 Linux 上做过 bash -n 语法检查，**未在 Linux 实跑**；
       本机（Windows）只能验证语法与参数解析。
EOF
}

die() { printf 'FAIL：%s\n' "$*" >&2; exit 1; }
bad_usage() { printf '参数错误：%s\n（用 --help 查看用法）\n' "$*" >&2; exit 4; }

while [ $# -gt 0 ]; do
  case $1 in
    --plugin-user) [ $# -ge 2 ] || bad_usage "--plugin-user 需要值"; PLUGIN_USER=$2; shift ;;
    --cred-dir)    [ $# -ge 2 ] || bad_usage "--cred-dir 需要值"; CRED_DIR=$2; shift ;;
    --proc)        [ $# -ge 2 ] || bad_usage "--proc 需要值"; PROC_NAMES=$2; shift ;;
    --verbose)     VERBOSE=1 ;;
    -h|--help)     usage; exit 0 ;;
    *)             bad_usage "未知参数 $1" ;;
  esac
  shift
done

PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS + 1)); printf 'PASS  %s\n' "$*"; }
fail() { FAIL=$((FAIL + 1)); printf 'FAIL  %s\n' "$*"; }
skip() { SKIP=$((SKIP + 1)); printf 'SKIP  %s\n' "$*"; }

printf '=== P8-① 隔离验收复核 ===\n'
printf '插件用户：%s   凭据目录：%s\n\n' "$PLUGIN_USER" "$CRED_DIR"

# 1) 用户存在且非 root -------------------------------------------------------
if ! id -u "$PLUGIN_USER" >/dev/null 2>&1; then
  fail "用户 $PLUGIN_USER 不存在（先跑 p8-create-plugin-user.sh --apply）"
else
  uid=$(id -u "$PLUGIN_USER")
  if [ "$uid" -eq 0 ]; then
    fail "用户 $PLUGIN_USER uid=0（root）——P8-① 要求非 root"
  else
    ok "用户 $PLUGIN_USER 存在且非 root（uid=$uid）"
  fi
fi

# 2) 运行中的插件进程属于插件用户 ------------------------------------------
found_proc=0
for name in $PROC_NAMES; do
  if ! command -v pgrep >/dev/null 2>&1; then break; fi
  pids=$(pgrep -x "$name" 2>/dev/null || true)
  [ -n "$pids" ] || continue
  found_proc=1
  for pid in $pids; do
    owner=$(ps -o user= -p "$pid" 2>/dev/null || echo '?')
    if [ "$owner" = "$PLUGIN_USER" ]; then
      ok "进程 $name pid=$pid 属主=$owner"
    elif [ "$owner" = '?' ]; then
      skip "进程 $name pid=$pid 属主未知（ps 不可用）"
    else
      fail "进程 $name pid=$pid 属主=$owner（期望 $PLUGIN_USER，非 root）"
    fi
  done
done
[ "$found_proc" -eq 1 ] || skip "当前没有 $PROC_NAMES 进程在跑（验收时在服务运行中复核）"

# 3) 凭据目录模式 0700、属主非插件用户 ------------------------------------
if [ ! -e "$CRED_DIR" ]; then
  skip "凭据目录 $CRED_DIR 不存在（首次部署正常；验收前先完成 P4 凭据同步）"
else
  mode=$(stat -c '%a' "$CRED_DIR" 2>/dev/null || echo '?')
  owner=$(stat -c '%U' "$CRED_DIR" 2>/dev/null || echo '?')
  group=$(stat -c '%G' "$CRED_DIR" 2>/dev/null || echo '?')
  if [ "$mode" = '?' ]; then
    skip "stat 不可用，无法读模式"
  elif [ "$mode" != "700" ] && [ "$mode" != "0700" ]; then
    fail "凭据目录模式=$mode，期望 0700"
  elif [ "$owner" = "$PLUGIN_USER" ]; then
    fail "凭据目录属主=$owner 是插件用户自己——隔离不成立"
  else
    ok "凭据目录模式=$mode 属主=$owner:  $group（插件用户不可读）"
  fi
fi

# 4) 负向测试：插件用户实际尝试读凭据目录 ----------------------------------
#    用 runuser 或 sudo -u 以插件用户身份执行，必须失败。
#    ⚠️ 判定纪律：只有「test -r 返回 1（不可读）」才算通过。
#       rc=0  -> 能读，FAIL。
#       rc=2/126/127/255 或其它 -> **探测本身没跑成**，必须 SKIP 而不是 PASS
#       （Windows 11 自带 sudo.exe，会被误当成切换用户工具，实测踩过）。
neg() {
  if command -v runuser >/dev/null 2>&1; then
    runuser -u "$PLUGIN_USER" -- "$@"
  elif [ "$(uname -s 2>/dev/null)" = 'Linux' ] && command -v sudo >/dev/null 2>&1; then
    sudo -n -u "$PLUGIN_USER" -- "$@"
  else
    return 255  # 无法切换用户
  fi
}
if [ ! -e "$CRED_DIR" ]; then
  skip "凭据目录不存在，负向测试无对象"
elif [ "$(uname -s 2>/dev/null)" != 'Linux' ]; then
  skip "非 Linux 平台，无法做用户切换的负向测试（本机只验证语法）"
else
  set +e
  neg sh -c 'test -r "$1"' -- "$CRED_DIR" >/dev/null 2>&1
  rc=$?
  set -e
  case $rc in
    0)      fail "插件用户 test -r $CRED_DIR 成功——凭据对插件用户可读（P8-① 未达成）" ;;
    1)      ok   "插件用户 test -r $CRED_DIR 失败（不可读）——负向测试通过" ;;
    255)    skip "无 runuser/sudo，负向测试跳过；请在服务器以 root 复核" ;;
    *)      skip "以插件用户执行探测失败（rc=$rc），结果不确定，不作 PASS 处理" ;;
  esac

  # 真正的 open 尝试：期望输出 Permission denied（强制 C locale，避免中文提示干扰匹配）
  if [ $rc -eq 1 ]; then
    set +e
    out=$(neg env LC_ALL=C sh -c 'ls -a "$1" 2>&1' -- "$CRED_DIR" 2>&1)
    rc2=$?
    set -e
    if [ $rc2 -ne 0 ] && printf '%s' "$out" | grep -q 'Permission denied'; then
      ok "插件用户 ls 凭据目录被拒（Permission denied）——文件系统层确认"
    elif [ $rc2 -eq 0 ]; then
      fail "插件用户 ls 凭据目录成功——凭据对插件用户可读"
    else
      skip "ls 探测结果不确定（rc=$rc2）：$out"
    fi
  fi
fi

# 5) 插件目录链上不得有 group/other 可写位 ---------------------------------
for d in /var/lib/aiworkgate/plugins /var/lib/aiworkgate/runtime; do
  [ -e "$d" ] || { skip "目录 $d 不存在（首次部署正常）"; continue; }
  mode=$(stat -c '%a' "$d" 2>/dev/null || echo '?')
  case $mode in
    '?') skip "无法读 $d 模式" ;;
    *2*|*6*) fail "$d 模式=$mode 含可写位给组/其他（期望 0750）" ;;
    *)    ok "$d 模式=$mode" ;;
  esac
done

printf '\n=== 汇总：PASS=%d FAIL=%d SKIP=%d ===\n' "$PASS" "$FAIL" "$SKIP"
if [ "$FAIL" -ne 0 ]; then exit 1; fi
if [ "$SKIP" -gt 0 ]; then exit 2; fi
exit 0
