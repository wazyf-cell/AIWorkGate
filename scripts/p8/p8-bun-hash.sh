#!/usr/bin/env bash
# =============================================================================
# P8-⑥ bun 二进制哈希锁定 + 「禁止运行时下载」审计
#
# 契约来源：
#   plans/2026-11-05-magpie接入与公网部署计划.md  §P8「做什么 6」
#   plans/2026-11-05-安全加固计划.md              §S-6「做什么 6」
#   D-05（打包 bun，不依赖服务器装 Node）、F-11（bun 1.4.4 单文件 110MB）
#
# 三条硬规则（本脚本是它们的可执行形式）：
#   1. bun 随包内置，**哈希写进 pin 文件**；启动/部署前校验。
#   2. **禁止运行时下载**：安装脚本/桥里不得出现 bun 的下载 URL。
#   3. 二进制不得对插件用户或其他组可写（否则等于允许原地替换）。
#
# 用法：
#   p8-bun-hash.sh record --file /opt/aiworkgate/bin/bun --version 1.4.4 --apply
#   p8-bun-hash.sh verify --file /opt/aiworkgate/bin/bun
#   p8-bun-hash.sh audit  --path /opt/aiworkgate --path ./scripts
#
# 退出码：
#   0 通过 / 2 不符或审计命中 / 3 无 pin 记录 / 4 参数错误 / 5 文件不存在
# =============================================================================
set -euo pipefail

PROG=${0##*/}
PIN_FILE="./p8-bun-pin.tsv"
readonly PIN_MAGIC="# p8-bun-pin v1"
export LC_ALL=C

VERB=${1:-}
case $VERB in
  record|verify|audit) shift ;;
  -h|--help|'') usage_now=1 ;;
  *) printf '参数错误：未知 verb %s\n' "$VERB" >&2; exit 4 ;;
esac

FILE=""; VERSION=""; APPLY=0; PATHS=()

usage() {
  cat <<EOF
用法： $PROG <verb> [选项]

verbs：
  record   把 bun 二进制的 sha256 + 版本写进 pin 文件（需 --apply）
  verify   校验 bun 二进制与 pin 一致 + 权限不可写 + （可选）版本串一致
  audit    扫描给定路径，发现「运行时下载 bun」或「curl|bash 装脚本」的痕迹即失败

record / verify 选项：
  --file PATH         bun 二进制路径（必填）
  --version V         版本号（record 必填；verify 给定时会跑 --version 交叉核对）
  --pin FILE          pin 文件（默认 ./p8-bun-pin.tsv）
  --allow-writable    跳过「不得可写」检查（仅本地演练，勿在生产用）
  --perm-override S   跳过 stat，直接用给定符号权限（如 -rw-r--r--）判定。
                      用于 stat 不可靠的环境（容器伪模式、MSYS/Cygwin 不映射组写位）
                      或离线自测。
  --apply             record 时真正写入（默认只打印计划）

audit 选项：
  --path PATH         要审计的路径，可重复；默认 ./scripts 与 /opt/aiworkgate
  --pin FILE          未使用（接受以便统一传参）

公共：
  -h, --help          显示本帮助

退出码： 0 通过 / 2 不符或审计命中 / 3 无 pin 记录 / 4 参数错误 / 5 文件不存在

审计规则（正则，命中任一即 exit 2）：
  1. bun.sh/install、bun.sh/download（运行时下载 bun）
  2. npm i -g bun / npm install --global bun
  3. curl|wget 直接灌给 sh/bash（含 sudo 变体、进程替换 <(...) 变体）
EOF
}

if [ "${usage_now:-0}" = "1" ]; then usage; exit 0; fi
# --help 在任何位置都要生效（放在解析前处理）
for a in "$@"; do
  case $a in -h|--help) usage; exit 0 ;; esac
done

ALLOW_WRITABLE=0
PERM_OVERRIDE=""
while [ $# -gt 0 ]; do
  case $1 in
    --file)           [ $# -ge 2 ] || { printf '参数错误：--file 需要值\n' >&2; exit 4; }; FILE=$2; shift ;;
    --version)        [ $# -ge 2 ] || { printf '参数错误：--version 需要值\n' >&2; exit 4; }; VERSION=$2; shift ;;
    --pin)            [ $# -ge 2 ] || { printf '参数错误：--pin 需要值\n' >&2; exit 4; }; PIN_FILE=$2; shift ;;
    --path)           [ $# -ge 2 ] || { printf '参数错误：--path 需要值\n' >&2; exit 4; }; PATHS+=("$2"); shift ;;
    --perm-override)  [ $# -ge 2 ] || { printf '参数错误：--perm-override 需要值\n' >&2; exit 4; }; PERM_OVERRIDE=$2; shift ;;
    --allow-writable) ALLOW_WRITABLE=1 ;;
    --apply)          APPLY=1 ;;
    -h|--help)        usage; exit 0 ;;
    *)                printf '参数错误：未知参数 %s\n（用 --help 查看用法）\n' "$1" >&2; exit 4 ;;
  esac
  shift
done

sha256_of() {
  local out
  out=$(sha256sum -- "$1")
  out=${out%% *}
  out=${out#\\}     # coreutils 对含反斜杠的路径会加前缀转义（Windows 自测时触发）
  printf '%s\n' "$out"
}
now_utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }

pin_entry() {
  [ -f "$PIN_FILE" ] || return 0
  awk -F'\t' -v v="$1" '$1==v {line=$0} END{if(line!="")print line}' "$PIN_FILE"
}

# ---- audit -----------------------------------------------------------------
if [ "$VERB" = 'audit' ]; then
  if [ ${#PATHS[@]} -eq 0 ]; then
    PATHS=("./scripts" "/opt/aiworkgate")
  fi
  # 三条规则合成一条 ERE；用 [] 拼字符避免脚本自身被字面量匹配（审计目标是别处）
  RULE='bun\.sh/(install|download)|npm[[:space:]]+(i|install)[[:space:]]+(-g|--global)[[:space:]]+bun|(curl|wget)[^|\n]*\|[[:space:]]*(sudo[[:space:]]+)?(ba)?sh|(ba)?sh[[:space:]]+<[[:space:]]*\([[:space:]]*(curl|wget)'
  printf '=== bun 供应链审计（禁止运行时下载 / 禁止 curl|bash）===\n'
  hits=0
  for p in "${PATHS[@]}"; do
    if [ ! -e "$p" ]; then
      printf '  SKIP  %s 不存在\n' "$p"
      continue
    fi
    printf '  扫描  %s\n' "$p"
    out=$(grep -rnEI "$RULE" "$p" 2>/dev/null || true)
    if [ -n "$out" ]; then
      hits=$((hits + $(printf '%s\n' "$out" | wc -l | tr -d ' ')))
      printf '%s\n' "$out" | sed 's/^/    HIT /'
    fi
  done
  printf '\n命中 %s 处\n' "$hits"
  if [ "$hits" -gt 0 ]; then
    printf 'FAIL  发现运行期下载/管道装脚本痕迹。bun 必须随包内置并锁定哈希（P8-⑥/⑦）。\n'
    exit 2
  fi
  printf 'PASS  未发现运行期下载或 curl|bash 痕迹\n'
  exit 0
fi

# ---- record / verify 公共前置 ---------------------------------------------
[ -n "$FILE" ] || { printf '参数错误：--file 必填\n' >&2; exit 4; }
[ -f "$FILE" ] || { printf '错误：bun 二进制不存在：%s\n' "$FILE" >&2; exit 5; }
SHA=$(sha256_of "$FILE")
SIZE=$(wc -c < "$FILE" | tr -d ' ')
EXISTING=$(pin_entry "${VERSION:-$(awk -F'\t' '$1!~/^#/{print $1; exit}' "$PIN_FILE" 2>/dev/null || true)}")

# ---- verify ----------------------------------------------------------------
if [ "$VERB" = 'verify' ]; then
  if [ ! -f "$PIN_FILE" ]; then
    printf '拒绝：pin 文件不存在（%s）。先 record 把内置 bun 的哈希钉住。\n' "$PIN_FILE" >&2
    exit 3
  fi
  # 版本：给了 --version 就按版本找，否则取 pin 里第一条
  want_ver=${VERSION:-$(awk -F'\t' '$1!~/^#/{print $1; exit}' "$PIN_FILE")}
  line=$(pin_entry "$want_ver")
  if [ -z "$line" ]; then
    printf '拒绝：pin 里没有 bun 版本 %s\n' "$want_ver" >&2
    exit 3
  fi
  p_sha=$(printf '%s' "$line" | awk -F'\t' '{print $2}')
  p_size=$(printf '%s' "$line" | awk -F'\t' '{print $3}')
  p_name=$(printf '%s' "$line" | awk -F'\t' '{print $4}')
  printf '=== bun 哈希校验 ===\n'
  printf '  文件：%s\n  pin 版本：%s（文件名 %s，%s 字节）\n' "$FILE" "$want_ver" "$p_name" "$p_size"
  printf '  期望 sha256：%s\n  实际 sha256：%s（%s 字节）\n' "$p_sha" "$SHA" "$SIZE"
  if [ "$SHA" != "$p_sha" ]; then
    printf 'FAIL  哈希不符：这个 bun 不是锁定的那个。禁止启动（P8-⑥）。\n' >&2
    exit 2
  fi
  printf 'PASS  sha256 一致\n'

  if [ -n "$VERSION" ]; then
    if out=$("$FILE" --version 2>/dev/null); then
      out=$(printf '%s' "$out" | tr -d '\r')
      if printf '%s' "$out" | grep -qF "$VERSION"; then
        printf 'PASS  二进制自报版本含 %s（%s）\n' "$VERSION" "$out"
      else
        printf 'FAIL  二进制自报版本为「%s」，与 pin 的 %s 不符\n' "$out" "$VERSION" >&2
        exit 2
      fi
    else
      printf 'SKIP  无法执行 %s --version（架构不符或非可执行文件）\n' "$FILE"
    fi
  fi

  if [ "$ALLOW_WRITABLE" -eq 0 ]; then
    # 判据用符号权限位，不用八进制子串匹配：
    #   0755 的 "755" 含数字 7，用 *7* 匹配会误判（本脚本初版就踩过）。
    #   -rwxr-xr-x 的第 6 位是组写、第 9 位是其他写（1-based: 2..10）。
    if [ -n "$PERM_OVERRIDE" ]; then
      symbolic=$PERM_OVERRIDE
      printf '（--perm-override=%s：跳过 stat，直接用给定位串判定）\n' "$symbolic"
    else
      symbolic=$(stat -c '%A' "$FILE" 2>/dev/null || echo '?')
    fi
    if [ "$symbolic" = '?' ]; then
      printf 'SKIP  无法读取文件权限\n'
    else
      gw=${symbolic:5:1}   # 0-based 5 == 第 6 位 == 组写
      ow=${symbolic:8:1}   # 0-based 8 == 第 9 位 == 其他写
      if [ "$gw" = 'w' ] || [ "$ow" = 'w' ]; then
        printf 'FAIL  %s 权限=%s（组或其他人可写=二进制可被原地替换）\n' "$FILE" "$symbolic" >&2
        exit 2
      fi
      printf 'PASS  权限=%s（组/其他不可写）\n' "$symbolic"
    fi
  fi
  exit 0
fi

# ---- record ----------------------------------------------------------------
[ -n "$VERSION" ] || { printf '参数错误：record 需要 --version\n' >&2; exit 4; }
NAME=$(basename -- "$FILE")
TIMESTAMP=$(now_utc)
printf '=== bun 哈希记录 ===\n'
printf '  文件：%s\n  版本：%s\n  sha256：%s\n  字节：%s\n' "$FILE" "$VERSION" "$SHA" "$SIZE"
if [ -n "$EXISTING" ] && [ "$EXISTING" != "" ]; then
  e_sha=$(printf '%s' "$EXISTING" | awk -F'\t' '{print $2}')
  if [ "$e_sha" = "$SHA" ]; then
    printf '无变化（幂等）：版本 %s 的哈希已是记录值。\n' "$VERSION"
    exit 0
  fi
  printf '警告：版本 %s 已存在但哈希不同（旧 %s）——换 bun 必须显式改 pin 并留痕。\n' "$VERSION" "${e_sha:0:16}"
fi
printf '  写入行：%s\t%s\t%s\t%s\t%s\n' "$VERSION" "$SHA" "$SIZE" "$NAME" "$TIMESTAMP"
if [ "$APPLY" -ne 1 ]; then
  printf '\n[dry-run] 未写入。确认后加 --apply。\n'
  exit 0
fi
if [ ! -f "$PIN_FILE" ]; then
  printf '%s\n' "$PIN_MAGIC" > "$PIN_FILE"
  printf '# version\tsha256\tbytes\tfilename\trecorded_at\n' >> "$PIN_FILE"
fi
tmp="$PIN_FILE.tmp.$$"
awk -F'\t' -v v="$VERSION" 'BEGIN{keep=1} $1!~/^#/ && $1==v {keep=0} {if(keep || $1~/^#/) print}' "$PIN_FILE" > "$tmp"
printf '%s\t%s\t%s\t%s\t%s\n' "$VERSION" "$SHA" "$SIZE" "$NAME" "$TIMESTAMP" >> "$tmp"
mv -- "$tmp" "$PIN_FILE"
printf '[apply] 已写入 %s\n' "$PIN_FILE"
