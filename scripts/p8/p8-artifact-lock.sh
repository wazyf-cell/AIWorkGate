#!/usr/bin/env bash
# =============================================================================
# P8-④ 插件 tarball 完整性锁定：记录 / 校验 SHA256 + 版本
#
# 契约来源：
#   plans/2026-11-05-magpie接入与公网部署计划.md  §P8「做什么 4」
#   plans/2026-11-05-安全加固计划.md              §S-6「做什么 4」
#
# 目的：把「装了什么」写成一条可审计的锁记录；安装/启动前校验。
#       哈希不符 = 拒绝，不提供「强制装」的隐藏开关（只能显式改锁记录，
#       且改锁记录本身会留在 .history 里）。
#
# 锁文件格式（TSV，一行一 artifact；# 开头为注释）：
#   id <TAB> kind <TAB> version <TAB> sha256 <TAB> bytes <TAB> recorded_at <TAB> source
#
# 用法：
#   p8-artifact-lock.sh record --id trae --kind tarball --file x.tgz --version 1.4.2 --apply
#   p8-artifact-lock.sh verify --id trae --file x.tgz [--version 1.4.2]
#   p8-artifact-lock.sh list
#
# 退出码：
#   0  通过 / 计划成功
#   2  哈希或版本不符（拒绝安装）
#   3  锁记录里没有该 id
#   4  参数错误
#   5  目标文件不存在
# =============================================================================
set -euo pipefail

PROG=${0##*/}
readonly LOCK_MAGIC="# p8-artifact-lock v1"
LOCK_FILE="./p8-artifact-lock.tsv"
LOCALE=C

usage() {
  cat <<EOF
用法： $PROG <verb> [选项]

verbs：
  record   记录一个 artifact 的 sha256 + 版本（写锁文件，需 --apply）
  verify   校验目标文件与锁记录是否一致（只读）
  list     列出锁记录

record 选项：
  --id NAME           artifact 标识（插件 id 或 bun），必填
  --kind KIND         tarball | bun | binary（默认 tarball）
  --file PATH         目标文件，必填
  --version V         版本号（默认 unversioned；强烈建议填）
  --source TEXT       来源（URL/包名，默认取文件名）
  --apply             真正写入（默认只打印计划）

verify 选项：
  --id NAME           必填
  --file PATH         必填
  --version V         给定时同时校验版本；锁里的版本比它新 => 判拒绝（防降级）
  --allow-downgrade   允许校验通过一个比锁记录旧的版本

公共选项：
  --lock FILE         锁文件路径（默认 ./p8-artifact-lock.tsv）
  -h, --help          显示本帮助

退出码： 0 通过 / 2 哈希或版本不符 / 3 无锁记录 / 4 参数错误 / 5 文件不存在

语义要点：
  * 同 id 同版本不同哈希 => record 直接拒绝（同版本内容变了＝供应链红灯）
  * 同 id 新版本 => 替换当前记录，旧行追加到 <lock>.history
  * verify 只认锁文件，不认识「新下载的东西」默认可信
EOF
}

die_usage() { printf '参数错误：%s\n（用 --help 查看用法）\n' "$*" >&2; exit 4; }
die_hash()  { printf '拒绝：%s\n' "$*" >&2; exit 2; }
die_miss()  { printf '拒绝：%s\n' "$*" >&2; exit 3; }
die_nofile(){ printf '拒绝：%s\n' "$*" >&2; exit 5; }

VERB=${1:-}
case $VERB in
  record|verify|list) shift ;;
  -h|--help|'') usage; exit 0 ;;
  *) die_usage "未知 verb '$VERB'（record|verify|list）" ;;
esac

ID=""; KIND="tarball"; FILE=""; VERSION=""; SOURCE=""; APPLY=0; ALLOW_DOWNGRADE=0
while [ $# -gt 0 ]; do
  case $1 in
    --id)              [ $# -ge 2 ] || die_usage "--id 需要值"; ID=$2; shift ;;
    --kind)            [ $# -ge 2 ] || die_usage "--kind 需要值"; KIND=$2; shift ;;
    --file)            [ $# -ge 2 ] || die_usage "--file 需要值"; FILE=$2; shift ;;
    --version)         [ $# -ge 2 ] || die_usage "--version 需要值"; VERSION=$2; shift ;;
    --source)          [ $# -ge 2 ] || die_usage "--source 需要值"; SOURCE=$2; shift ;;
    --lock)            [ $# -ge 2 ] || die_usage "--lock 需要值"; LOCK_FILE=$2; shift ;;
    --apply)           APPLY=1 ;;
    --allow-downgrade) ALLOW_DOWNGRADE=1 ;;
    -h|--help)         usage; exit 0 ;;
    *)                 die_usage "未知参数 $1" ;;
  esac
  shift
done
readonly LOCK_HISTORY="$LOCK_FILE.history"

# ---- 工具函数 --------------------------------------------------------------
# GNU coreutils 的文件名含反斜杠/换行时会把整行前缀一个 '\'（Windows 路径必现）。
# 只有 Linux 服务器上不会触发，但脚本要能在本机做自测，故显式剥离。
sha256_of() {
  local out
  out=$(sha256sum -- "$1")
  out=${out%% *}
  out=${out#\\}
  printf '%s\n' "$out"
}
bytes_of()  { wc -c < "$1" | tr -d ' '; }
now_utc()   { date -u +%Y-%m-%dT%H:%M:%SZ; }

# 取锁文件里该 id 的最后一条记录（后写的为准）
lock_entry() {
  [ -f "$LOCK_FILE" ] || return 0
  awk -F'\t' -v id="$1" 'BEGIN{IGNORECASE=0} $1==id {line=$0} END{if(line!="")print line}' "$LOCK_FILE"
}

field() { printf '%s' "$1" | awk -F'\t' -v n="$2" '{print $n}'; }

# 版本比较：$1 > $2 时返回 0
ver_gt() {
  [ -n "$1" ] && [ -n "$2" ] || return 1
  [ "$1" = "$2" ] && return 1
  top=$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -1)
  [ "$top" = "$1" ]
}

ensure_lock_header() {
  if [ ! -f "$LOCK_FILE" ]; then
    printf '%s\n' "$LOCK_MAGIC" > "$LOCK_FILE"
    printf '# id\tkind\tversion\tsha256\tbytes\trecorded_at\tsource\n' >> "$LOCK_FILE"
  fi
}

# ---- list ------------------------------------------------------------------
if [ "$VERB" = 'list' ]; then
  if [ ! -f "$LOCK_FILE" ]; then
    printf '（锁文件 %s 不存在）\n' "$LOCK_FILE"
    exit 0
  fi
  printf '锁文件：%s\n' "$LOCK_FILE"
  awk -F'\t' '$1 !~ /^#/ && NF >= 4 {
      printf "  %-40s %-8s %-12s %s\n", $1, $2, $3, substr($4,1,16)
    }' "$LOCK_FILE"
  n=$(awk -F'\t' '$1 !~ /^#/ && NF >= 4' "$LOCK_FILE" | wc -l | tr -d ' ')
  printf '共 %s 条记录\n' "$n"
  exit 0
fi

# ---- 公共前置 --------------------------------------------------------------
[ -n "$ID" ]   || die_usage "--id 必填"
[ -n "$FILE" ] || die_usage "--file 必填"
[ -f "$FILE" ] || die_nofile "文件不存在：$FILE"

SHA=$(sha256_of "$FILE")
SIZE=$(bytes_of "$FILE")
EXISTING=$(lock_entry "$ID")

# ---- verify ----------------------------------------------------------------
if [ "$VERB" = 'verify' ]; then
  if [ -z "$EXISTING" ]; then
    die_miss "锁记录里没有 id=$ID（先 record，再 verify）；文件=$FILE sha256=$SHA"
  fi
  l_ver=$(field "$EXISTING" 3)
  l_sha=$(field "$EXISTING" 4)
  l_kind=$(field "$EXISTING" 2)
  printf '校验 id=%s  锁版本=%s  锁kind=%s\n' "$ID" "$l_ver" "$l_kind"
  printf '  期望 sha256：%s\n' "$l_sha"
  printf '  实际 sha256：%s\n' "$SHA"

  if [ "$SHA" != "$l_sha" ]; then
    die_hash "哈希不符：$FILE 与锁记录不一致（版本锁定生效，拒绝安装）。
  若这是刻意的版本升级：先 record --version <新版本> --apply 更新锁记录并留痕。
  若你没打算升级：**这个 tarball 不可信，不要装。**"
  fi
  if [ -n "$VERSION" ]; then
    if [ "$VERSION" != "$l_ver" ]; then
      if ver_gt "$l_ver" "$VERSION" && [ "$ALLOW_DOWNGRADE" -ne 1 ]; then
        die_hash "版本降级被拒：请求 $VERSION < 锁定 $l_ver（确需降级加 --allow-downgrade）"
      fi
      die_hash "版本不符：请求 $VERSION != 锁定 $l_ver"
    fi
  fi
  printf 'PASS  哈希与版本一致（kid=%s ver=%s sha=%s…）\n' "$ID" "$l_ver" "${SHA:0:16}"
  exit 0
fi

# ---- record ----------------------------------------------------------------
[ -n "$VERSION" ] || VERSION="unversioned"
[ -n "$SOURCE" ]  || SOURCE=$(basename -- "$FILE")
TIMESTAMP=$(now_utc)

if [ -n "$EXISTING" ]; then
  l_ver=$(field "$EXISTING" 3)
  l_sha=$(field "$EXISTING" 4)
  if [ "$l_ver" = "$VERSION" ] && [ "$l_sha" = "$SHA" ]; then
    printf '无变化（幂等）：id=%s 版本=%s 哈希已是最新记录，不写盘。\n' "$ID" "$VERSION"
    exit 0
  fi
  if [ "$l_ver" = "$VERSION" ] && [ "$l_sha" != "$SHA" ]; then
    die_hash "同一版本出现不同内容：id=$ID 版本=$VERSION
  锁记录 sha256：$l_sha
  当前文件 sha256：$SHA
  同版本内容变化＝供应链红灯。请核对上游发布（包是否被替换/投毒），不要直接覆盖记录。"
  fi
  printf '更新记录：id=%s  %s -> %s\n' "$ID" "$l_ver" "$VERSION"
  printf '  旧 sha256：%s（保留到 %s）\n' "$l_sha" "$LOCK_HISTORY"
else
  printf '新增记录：id=%s 版本=%s\n' "$ID" "$VERSION"
fi
NEW_LINE=$(printf '%s\t%s\t%s\t%s\t%s\t%s\t%s' "$ID" "$KIND" "$VERSION" "$SHA" "$SIZE" "$TIMESTAMP" "$SOURCE")
printf '  写入行：%s\n' "$NEW_LINE"
printf '  锁文件：%s\n' "$LOCK_FILE"

if [ "$APPLY" -ne 1 ]; then
  printf '\n[dry-run] 未写入。确认后加 --apply。\n'
  exit 0
fi

ensure_lock_header
if [ -n "$EXISTING" ]; then
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$(field "$EXISTING" 1)" "$(field "$EXISTING" 2)" "$(field "$EXISTING" 3)" \
    "$(field "$EXISTING" 4)" "$(field "$EXISTING" 5)" "$(field "$EXISTING" 6)" \
    "$(field "$EXISTING" 7)" >> "$LOCK_HISTORY"
  tmp="$LOCK_FILE.tmp.$$"
  awk -F'\t' -v id="$ID" '$1!=id' "$LOCK_FILE" > "$tmp"
  printf '%s\n' "$NEW_LINE" >> "$tmp"
  mv -- "$tmp" "$LOCK_FILE"
else
  printf '%s\n' "$NEW_LINE" >> "$LOCK_FILE"
fi
printf '[apply] 已写入 %s\n' "$LOCK_FILE"
