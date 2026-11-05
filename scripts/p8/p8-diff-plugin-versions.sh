#!/usr/bin/env bash
# =============================================================================
# P8-⑤ 插件更新差异摘要（人工可读）：新增域名 / 新增文件 / child_process 调用点变化
#
# 契约来源：
#   plans/2026-11-05-magpie接入与公网部署计划.md  §P8「做什么 5」
#   plans/2026-11-05-安全加固计划.md              §S-6「做什么 5」
#   plans/2026-11-05-magpie接入与公网部署计划.md  D-58（装前人工审查：新增域名/文件/hooks）
#
# 用途：把「新版比旧版多了什么」摆到人眼前，替代「点点更新按钮」。
#       自动化只看有没有**新增域名**和**新增 child_process 引用**；其余交人工。
#
# 用法：
#   p8-diff-plugin-versions.sh --old <旧版目录> --new <新版目录>
#   p8-diff-plugin-versions.sh --old a --new b --fail-on-new-domains   # CI 用：有新增域名即 exit 2
#
# 退出码：
#   0  差分完成（无阻断项）
#   2  有新增域名（仅在 --fail-on-new-domains 时）
#   3  目录不存在
#   4  参数错误
#
# 已知限制（不是缺陷，写清楚避免误判）：
#   * 域名提取是 JSON 文本扫描，不是解析器：manifest 必须是 standard JSON，
#     `network_domains` 若被注释或写成表达式则提取为空。
#   * child_process 检测是 grep 级，不是 AST：只能发现字面量引用，
#     动态拼接（`process["chil"+"d_process"]`）检测不到——这类要人工看代码。
#   * 不检测混淆/编码后的载荷。
# =============================================================================
set -euo pipefail

PROG=${0##*/}
OLD_DIR=""; NEW_DIR=""
FAIL_ON_NEW_DOMAINS=0
SHOW_FILES=1
export LC_ALL=C

usage() {
  cat <<EOF
用法： $PROG --old DIR --new DIR [选项]

对比两版插件目录，输出人工可读的安全差异摘要：
  1. 版本号（manifest.json 的 version）
  2. 网络域名增删（manifest.permissions.network_domains）
  3. 文件增删改（按 sha256 判定改动）
  4. child_process 引用/直接调用点的增删（行号不敏感）

选项：
  --old DIR                旧版目录（必填）
  --new DIR                新版目录（必填）
  --fail-on-new-domains    出现新增域名时 exit 2（用于自动更新闸门）
  --no-files               跳过文件清单（大插件时更快）
  -h, --help               显示本帮助

退出码： 0 完成 / 2 有新增域名(仅带 --fail-on-new-domains) / 3 目录不存在 / 4 参数错误
EOF
}

die_usage() { printf '参数错误：%s\n（用 --help 查看用法）\n' "$*" >&2; exit 4; }
die_dir()   { printf '错误：%s\n' "$*" >&2; exit 3; }

while [ $# -gt 0 ]; do
  case $1 in
    --old)                 [ $# -ge 2 ] || die_usage "--old 需要值"; OLD_DIR=$2; shift ;;
    --new)                 [ $# -ge 2 ] || die_usage "--new 需要值"; NEW_DIR=$2; shift ;;
    --fail-on-new-domains) FAIL_ON_NEW_DOMAINS=1 ;;
    --no-files)            SHOW_FILES=0 ;;
    -h|--help)             usage; exit 0 ;;
    *)                     die_usage "未知参数 $1" ;;
  esac
  shift
done
[ -n "$OLD_DIR" ] || die_usage "--old 必填"
[ -n "$NEW_DIR" ] || die_usage "--new 必填"
[ -d "$OLD_DIR" ] || die_dir "旧版目录不存在：$OLD_DIR"
[ -d "$NEW_DIR" ] || die_dir "新版目录不存在：$NEW_DIR"

TMP=$(mktemp -d) || { printf '无法创建临时目录\n' >&2; exit 3; }
cleanup() { rm -rf -- "$TMP"; }
trap cleanup EXIT

# ---- 通用提取 --------------------------------------------------------------
find_manifest() {
  if [ -f "$1/manifest.json" ]; then
    printf '%s\n' "$1/manifest.json"
    return 0
  fi
  found=$(find "$1" -maxdepth 2 -name manifest.json -type f 2>/dev/null | head -1 || true)
  [ -n "$found" ] && printf '%s\n' "$found"
  return 0
}

# 从 manifest 里抽 network_domains 的字符串数组（一次扫描，容忍跨行）
domains_of() {
  local manifest
  manifest=$(find_manifest "$1")
  [ -n "$manifest" ] && [ -f "$manifest" ] || return 0
  awk -v key="network_domains" '
    { flat = flat $0 }
    END {
      n = length(flat); if (n == 0) exit 0
      target = "\"" key "\""
      p = index(flat, target); if (p == 0) exit 0
      i = p + length(target)
      while (i <= n) {
        c = substr(flat, i, 1)
        if (c == " " || c == ":" || c == "\r") { i++; continue }
        break
      }
      if (i > n) exit 0
      if (substr(flat, i, 1) != "[") exit 0
      depth = 0; instr = 0; esc = 0; buf = ""
      for (; i <= n; i++) {
        c = substr(flat, i, 1)
        if (instr) {
          if (esc) { buf = buf c; esc = 0; continue }
          if (c == "\\") { esc = 1; continue }
          if (c == "\"") { print buf; buf = ""; instr = 0; continue }
          buf = buf c; continue
        }
        if (c == "\"") { instr = 1; buf = ""; continue }
        if (c == "[") { depth++; continue }
        if (c == "]") { depth--; if (depth == 0) exit 0 }
      }
    }' "$manifest" | sed 's/[[:space:]]*$//' | grep -v '^$' | sort -u
}

version_of() {
  local manifest
  manifest=$(find_manifest "$1")
  [ -n "$manifest" ] && [ -f "$manifest" ] || { printf 'unknown\n'; return 0; }
  awk '
    { flat = flat $0 }
    END {
      if (match(flat, /"version"[[:space:]]*:[[:space:]]*"[^"]*"/)) {
        s = substr(flat, RSTART, RLENGTH)
        sub(/^"version"[[:space:]]*:[[:space:]]*"/, "", s)
        sub(/"$/, "", s)
        print s
      } else print "unknown"
    }' "$manifest"
}

files_of() { ( cd "$1" && find . -type f | sed 's|^\./||' | sort ); }

# 相对路径 -> sha256（只对两边都存在的文件调用）
sha_of_rel() { sha256sum -- "$1/$2" | awk '{print $1}'; }

# child_process 相关行：normalize 成 "相对路径<TAB>去首尾空白的代码"，行号不参与比较
cp_lines() {
  ( cd "$1" && grep -rnE 'child_process' \
      --include='*.mjs' --include='*.js' --include='*.cjs' --include='*.ts' . 2>/dev/null || true ) \
  | sed -E 's/^\.\///' \
  | awk -F: '{
      path = $1; line = $0
      sub(/^[^:]*:[0-9]+:/, "", line)
      gsub(/^[ \t]+|[ \t]+$/, "", line)
      if (line != "") printf "%s\t%s\n", path, line
    }' | sort -u
}

# 直接调用（非 import 语句）：execSync / spawnSync / execFile / execFileSync / fork(
cp_calls() {
  ( cd "$1" && grep -rnE '(execSync|spawnSync|execFileSync|execFile)[[:space:]]*\(|\.fork[[:space:]]*\(|\b(child_process)\.(exec|spawn|execFile)' \
      --include='*.mjs' --include='*.js' --include='*.cjs' --include='*.ts' . 2>/dev/null || true ) \
  | sed -E 's/^\.\///' \
  | awk -F: '{
      path = $1; line = $0
      sub(/^[^:]*:[0-9]+:/, "", line)
      gsub(/^[ \t]+|[ \t]+$/, "", line)
      if (line != "") printf "%s\t%s\n", path, line
    }' | sort -u
}

section() { printf '\n%s\n' "$*"; printf '%s\n' "----------------------------------------------------------------------"; }

printf '=== P8 插件更新差异摘要 ===\n'
printf '旧版：%s\n' "$OLD_DIR"
printf '新版：%s\n' "$NEW_DIR"

# ---- 1) 版本 ---------------------------------------------------------------
old_ver=$(version_of "$OLD_DIR")
new_ver=$(version_of "$NEW_DIR")
section "[1] 版本"
printf '  %s  ->  %s\n' "$old_ver" "$new_ver"
if [ "$old_ver" = "$new_ver" ]; then
  printf '  ⚠ 版本号未变但内容可能已变（同版本内容变化在供应链上是红灯，务必逐项看下去）\n'
fi

# ---- 2) 网络域名 -----------------------------------------------------------
domains_of "$OLD_DIR" > "$TMP/domains.old"
domains_of "$NEW_DIR" > "$TMP/domains.new"
section "[2] 网络域名（manifest.permissions.network_domains）"
printf '  旧版 %s 个 / 新版 %s 个\n' \
  "$(wc -l < "$TMP/domains.old" | tr -d ' ')" "$(wc -l < "$TMP/domains.new" | tr -d ' ')"
comm -13 "$TMP/domains.old" "$TMP/domains.new" > "$TMP/domains.added"
comm -23 "$TMP/domains.old" "$TMP/domains.new" > "$TMP/domains.removed"

added_domains=$(wc -l < "$TMP/domains.added" | tr -d ' ')
removed_domains=$(wc -l < "$TMP/domains.removed" | tr -d ' ')
if [ "$added_domains" -gt 0 ]; then
  printf '\n  ★ 新增域名 %s 个（插件被投毒后外传数据的常见载体，逐个确认真实必要）：\n' "$added_domains"
  sed 's/^/    + /' "$TMP/domains.added"
else
  printf '  无新增域名\n'
fi
if [ "$removed_domains" -gt 0 ]; then
  printf '\n  移除域名 %s 个：\n' "$removed_domains"
  sed 's/^/    - /' "$TMP/domains.removed"
fi

# ---- 3) 文件 ---------------------------------------------------------------
files_of "$OLD_DIR" > "$TMP/files.old"
files_of "$NEW_DIR" > "$TMP/files.new"
comm -13 "$TMP/files.old" "$TMP/files.new" > "$TMP/files.added"
comm -23 "$TMP/files.old" "$TMP/files.new" > "$TMP/files.removed"
comm -12 "$TMP/files.old" "$TMP/files.new" > "$TMP/files.common"

added_n=$(wc -l < "$TMP/files.added" | tr -d ' ')
removed_n=$(wc -l < "$TMP/files.removed" | tr -d ' ')
changed_n=0
: > "$TMP/files.changed"
while IFS= read -r rel; do
  [ -n "$rel" ] || continue
  if [ "$(sha_of_rel "$OLD_DIR" "$rel")" != "$(sha_of_rel "$NEW_DIR" "$rel")" ]; then
    printf '%s\n' "$rel" >> "$TMP/files.changed"
    changed_n=$((changed_n + 1))
  fi
done < "$TMP/files.common"

section "[3] 文件"
printf '  改 %s / 新 %s / 删 %s\n' "$changed_n" "$added_n" "$removed_n"
if [ "$SHOW_FILES" -eq 1 ]; then
  [ "$added_n" -gt 0 ]   && sed 's/^/    + /' "$TMP/files.added"
  [ "$removed_n" -gt 0 ] && sed 's/^/    - /' "$TMP/files.removed"
  [ "$changed_n" -gt 0 ] && sed 's/^/    ~ /' "$TMP/files.changed"
else
  printf '  （--no-files：已跳过清单）\n'
fi

# ---- 4) child_process ------------------------------------------------------
cp_lines "$OLD_DIR" > "$TMP/cp.old"
cp_lines "$NEW_DIR" > "$TMP/cp.new"
cp_calls "$OLD_DIR" > "$TMP/cpcalls.old"
cp_calls "$NEW_DIR" > "$TMP/cpcalls.new"

section "[4] child_process 调用点变化"
printf '  引用（import/require）行数： 旧 %s -> 新 %s\n' \
  "$(wc -l < "$TMP/cp.old" | tr -d ' ')" "$(wc -l < "$TMP/cp.new" | tr -d ' ')"
comm -13 "$TMP/cp.old" "$TMP/cp.new" > "$TMP/cp.added"
comm -23 "$TMP/cp.old" "$TMP/cp.new" > "$TMP/cp.removed"
cp_added_n=$(wc -l < "$TMP/cp.added" | tr -d ' ')
cp_removed_n=$(wc -l < "$TMP/cp.removed" | tr -d ' ')
if [ "$cp_added_n" -gt 0 ]; then
  printf '\n  ★ 新增 %s 处 child_process 引用（能起子进程 = 能绕过出口白名单的典型手法）：\n' "$cp_added_n"
  sed 's/^/    + /' "$TMP/cp.added"
else
  printf '  无新增 child_process 引用\n'
fi
if [ "$cp_removed_n" -gt 0 ]; then
  printf '\n  移除 %s 处：\n' "$cp_removed_n"
  sed 's/^/    - /' "$TMP/cp.removed"
fi

comm -13 "$TMP/cpcalls.old" "$TMP/cpcalls.new" > "$TMP/cpcalls.added"
cpcaps_added_n=$(wc -l < "$TMP/cpcalls.added" | tr -d ' ')
if [ "$cpcaps_added_n" -gt 0 ]; then
  printf '\n  ★ 新增直接调用点 %s 处（execSync/spawnSync/execFile/.fork 等；grep 级，可能含同名误报）：\n' "$cpcaps_added_n"
  sed 's/^/    + /' "$TMP/cpcalls.added"
else
  printf '  无新增直接调用点\n'
fi

# ---- 5) 结论 ---------------------------------------------------------------
section "[5] 结论"
risk=0
[ "$added_domains" -gt 0 ] && { printf '  · 有新增域名 -> 必须人工确认真实必要\n'; risk=1; }
[ "$added_n" -gt 0 ]       && { printf '  · 有新增文件 -> 逐个看内容（尤其 .mjs/.js）\n'; risk=1; }
[ "$cp_added_n" -gt 0 ]    && { printf '  · 有新增 child_process -> 高风险，确认是否可绕出口白名单\n'; risk=1; }
[ "$(find_manifest "$NEW_DIR")" != "" ] && [ "$(domains_of "$NEW_DIR" | wc -l | tr -d ' ')" -eq 0 ] \
  && printf '  ⚠ 新版没声明任何 network_domains -> 按设计应与发布者的预期核对（无声明=不该联网）\n'
if [ "$risk" -eq 0 ]; then
  printf '  无阻断级差异：可进入「提示优先」的更新流程（仍需人点确认）\n'
fi

if [ "$FAIL_ON_NEW_DOMAINS" -eq 1 ] && [ "$added_domains" -gt 0 ]; then
  printf '\n[gate] --fail-on-new-domains：存在 %s 个新增域名，按闸门拒绝自动更新（exit 2）\n' "$added_domains"
  exit 2
fi
exit 0
