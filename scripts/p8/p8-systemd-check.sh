#!/usr/bin/env bash
# =============================================================================
# P8-② systemd 加固单元自检（静态规则 + 可选 live 校验）
#
# 静态部分在任何机器都能跑（本机已实跑）；live 部分需要 systemd：
#   systemd-analyze verify <unit>    —— 真正的语法+依赖校验（**未在 Linux 实跑**）
#   systemctl cat  <unit>            —— 部署后回读（验收项 5 用）
#
# 校验规则对应 plans/2026-11-05-magpie接入与公网部署计划.md §P8 验收 5
# 与 §S-6「怎么验」systemctl cat 的期望项。
#
# 退出码：0 全部通过（可含 WARN）/ 1 有 FAIL / 2 live 校验被跳过
# =============================================================================
set -euo pipefail

PROG=${0##*/}
UNIT_FILE="scripts/p8/systemd/aiworkgate-plugin@.service"
LIVE=0

usage() {
  cat <<EOF
用法： $PROG [选项]

静态校验默认单元样例（也适用于任何自定义 unit 文件）：
  1. [Unit]/[Service] 段存在
  2. P8 要求的关键指令与取值逐项核对：
     NoNewPrivileges=yes  ProtectSystem=strict  ProtectHome=yes
     PrivateTmp=yes  RestrictAddressFamilies=AF_INET AF_INET6
  3. User= 非 root、Group= 存在
  4. MemoryDenyWriteExecute 必须**带书面取舍注释**（=yes 与 =no 都要）
  5. 禁止行尾注释（systemd 不支持，会把注释拼进值）
  6. 禁止同一 key 重复（systemd 静默取最后一个，等于隐藏 bug）
  7. ExecStart 绝对路径；ProtectSystem=strict 时必须声明 ReadWritePaths
  8. CapabilityBoundingSet 为空或受限

选项：
  --unit-file PATH   要校验的 unit 文件（默认本卡样例）
  --live             追加 systemd-analyze verify + systemctl cat（需服务器）
  -h, --help         显示本帮助

退出码： 0 通过 / 1 有 FAIL / 2 live 被跳过
EOF
}

UNIT_FILE_OVERRIDE=""
while [ $# -gt 0 ]; do
  case $1 in
    --unit-file) [ $# -ge 2 ] || { printf '参数错误：--unit-file 需要值\n' >&2; exit 1; }; UNIT_FILE_OVERRIDE=$2; shift ;;
    --live) LIVE=1 ;;
    -h|--help) usage; exit 0 ;;
    *) printf '参数错误：未知参数 %s\n（用 --help 查看用法）\n' "$1" >&2; exit 1 ;;
  esac
  shift
done
[ -n "$UNIT_FILE_OVERRIDE" ] && UNIT_FILE=$UNIT_FILE_OVERRIDE

PASS=0; WARN=0; FAIL=0
ok()   { PASS=$((PASS+1)); printf 'PASS  %s\n' "$*"; }
warn() { WARN=$((WARN+1)); printf 'WARN  %s\n' "$*"; }
fail() { FAIL=$((FAIL+1)); printf 'FAIL  %s\n' "$*"; }

printf '=== systemd 加固单元自检：%s ===\n' "$UNIT_FILE"
[ -f "$UNIT_FILE" ] || { printf 'FAIL  文件不存在：%s\n' "$UNIT_FILE" >&2; exit 1; }

# ---- 1) 段结构 -------------------------------------------------------------
for sec in Unit Service; do
  if grep -qE "^\[$sec\]" "$UNIT_FILE"; then ok "[$sec] 段存在"; else fail "[$sec] 段缺失"; fi
done

# ---- 2) P8 关键指令 ---------------------------------------------------------
check_directive() {
  local key=$1 expected=$2
  local line
  line=$(grep -E "^${key}=" "$UNIT_FILE" | tail -1 || true)
  if [ -z "$line" ]; then
    fail "$key= 缺失（期望 $expected）"
    return
  fi
  if [ "$line" = "$key=$expected" ]; then
    ok "$key=$expected"
  else
    fail "$key 取值异常：'$line'（期望 $key=$expected）"
  fi
}
check_directive NoNewPrivileges yes
check_directive ProtectSystem strict
check_directive ProtectHome yes
check_directive PrivateTmp yes
check_directive RestrictAddressFamilies 'AF_INET AF_INET6'

# ---- 3) 身份 ----------------------------------------------------------------
user_line=$(grep -E '^User=' "$UNIT_FILE" | tail -1 || true)
if [ -z "$user_line" ]; then
  fail 'User= 缺失（插件必须跑在独立用户下）'
elif printf '%s' "$user_line" | grep -qE '^User=(root|0)$'; then
  fail 'User=root —— P8-① 要求非 root'
else
  ok "User=${user_line#User=}"
fi
grep -qE '^Group=' "$UNIT_FILE" || fail 'Group= 缺失'
grep -qE '^CapabilityBoundingSet=$' "$UNIT_FILE" \
  && ok 'CapabilityBoundingSet= 为空（零能力）' \
  || fail 'CapabilityBoundingSet 未置空（插件桥不应持有任何能力）'

# ---- 4) MemoryDenyWriteExecute 必须书面取舍 --------------------------------
mde=$(grep -E '^MemoryDenyWriteExecute=' "$UNIT_FILE" | tail -1 || true)
if [ -z "$mde" ]; then
  fail 'MemoryDenyWriteExecute 未出现 —— P8-② 要求显式取舍（=no 也要写理由）'
else
  # 该指令之前 8 行内必须有说明注释（提及 bun / JIT / 取舍 任一关键词）
  n=$(grep -nE '^MemoryDenyWriteExecute=' "$UNIT_FILE" | tail -1 | cut -d: -f1)
  ctx=$(sed -n "$((n-8)),$((n-1))p" "$UNIT_FILE")
  if printf '%s' "$ctx" | grep -qE 'bun|JIT|取舍|JSC'; then
    ok "MemoryDenyWriteExecute=${mde#*=}（取舍已书面记录，见上方注释）"
  else
    fail "MemoryDenyWriteExecute=${mde#*=} 但前 8 行内没有 bun/JIT/取舍 的书面理由 —— 取舍记录缺失"
  fi
fi

# ---- 5) 行尾注释 -------------------------------------------------------------
inline=$(awk '$0 !~ /^[[:space:]]*#/ && $0 ~ /#/ { print NR": "$0 }' "$UNIT_FILE")
if [ -n "$inline" ]; then
  fail "发现行尾注释（systemd 会把 '#' 拼进值里）：
$inline"
else
  ok '无行尾注释'
fi

# ---- 5b) 行格式（独立的「逐行语法」检查，不依赖 systemd）--------------------
# 非空且非注释的行，只允许两种形态：
#   [Section]            段头
#   Key=Value…           指令（Key 以字母开头、只含字母数字）
# 其它任何形态（漏了 '='、指令名带空格、值前有多余 '=' 等）都是语法错。
badfmt=$(awk '
  { line=$0; sub(/^[[:space:]]+/,"",line); sub(/[[:space:]]+$/,"",line) }
  line=="" { next }
  line ~ /^#/ { next }
  line ~ /^\[[A-Za-z]+\]$/ { next }
  line ~ /^[A-Za-z][A-Za-z0-9]*=/ { next }
  { print NR": "$0 }
' "$UNIT_FILE")
if [ -n "$badfmt" ]; then
  fail "存在不是 [段] 也不是 Key=Value 的行：
$badfmt"
else
  ok '行格式全部合法（[段] 或 Key=Value）'
fi

# ---- 6) 重复 key --------------------------------------------------------------
dups=$(awk -F= '!/^[[:space:]]*#/ && /^[A-Za-z]+=/ { key=$1; if (seen[key]++) print key }' "$UNIT_FILE" | sort -u)
if [ -n "$dups" ]; then
  fail "同一 key 重复出现（systemd 静默取最后）：
$dups"
else
  ok '无重复 key'
fi

# ---- 7) ExecStart / ReadWritePaths -----------------------------------------
exe=$(grep -E '^ExecStart=' "$UNIT_FILE" | tail -1 || true)
if [ -z "$exe" ]; then
  fail 'ExecStart= 缺失'
elif printf '%s' "$exe" | grep -qE '^ExecStart=[^/]'; then
  fail 'ExecStart 不是绝对路径'
else
  ok 'ExecStart 为绝对路径'
fi
grep -qE '^ReadWritePaths=' "$UNIT_FILE" \
  && ok 'ReadWritePaths 已声明（ProtectSystem=strict 的唯一可写面）' \
  || warn 'ReadWritePaths 未声明（ProtectSystem=strict 下服务将完全只读）'

# ---- 8) 推荐项（缺了只 WARN，不算 FAIL） -----------------------------------
for rec in PrivateDevices RestrictNamespaces RestrictRealtime SystemCallFilter LockPersonality ProtectKernelTunables RestrictSUIDSGID; do
  grep -qE "^$rec=" "$UNIT_FILE" || warn "推荐指令缺失：$rec="
done

# ---- 9) live（systemd 环境）--------------------------------------------------
if [ "$LIVE" -eq 1 ]; then
  if command -v systemd-analyze >/dev/null 2>&1; then
    out=$(systemd-analyze verify "$UNIT_FILE" 2>&1 || true)
    if [ -z "$out" ]; then
      ok 'systemd-analyze verify：无告警'
    else
      warn "systemd-analyze verify 输出：
$out"
    fi
  else
    printf 'SKIP  systemd-analyze 不可用（未在 Linux 实跑）\n'
    SKIP_LIVE=1
  fi
  if command -v systemctl >/dev/null 2>&1; then
    printf '     部署后复核命令（验收项 5）：systemctl cat aiworkgate-plugin@<id>.service | grep -E "NoNewPrivileges|ProtectSystem|PrivateTmp|RestrictAddressFamilies"\n'
  fi
fi

printf '\n=== 汇总：PASS=%d WARN=%d FAIL=%d ===\n' "$PASS" "$WARN" "$FAIL"
if [ "$FAIL" -ne 0 ]; then exit 1; fi
if [ "${SKIP_LIVE:-0}" = "1" ]; then exit 2; fi
exit 0
