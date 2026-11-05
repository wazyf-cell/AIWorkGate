#!/usr/bin/env bash
# =============================================================================
# P8 资产自测：一条命令证明 scripts/p8 下的脚本「逻辑是对的」
#
# 它做什么：
#   1. 对每个 .sh 跑 bash -n（语法）
#   2. 用 fixture 跑真实功能：哈希锁定（正常/篡改/同版本异哈希/版本升级）
#   3. bun 哈希校验 + 运行时下载审计（正例与反例）
#   4. 版本差异摘要（新增域名/新增文件/child_process 增删）+ 闸门退出码
#   5. systemd 单元自检的正例与 5 个负例
#   6. 两个「必须 root/服务器」的脚本只验 --help 与 dry-run 计划输出
#
# 它不做什么（诚实的边界）：
#   * **不验证 Linux 系统行为**：useradd / install -d / runuser / systemd-analyze
#     这些在本机（Windows Git bash）无法执行，只能在服务器上跑真实验收。
#   * 不启动任何服务，不改系统，所有写操作限制在 mktemp 临时目录内。
#
# 退出码：0 全通过 / 1 有失败
# =============================================================================
set -uo pipefail

usage() {
  cat <<'EOF'
用法： p8-selftest.sh

一条命令跑完 scripts/p8 下全部脚本的**逻辑层**验证：
  * bash -n 全部 .sh
  * artifact-lock：记录/幂等/篡改拒绝/同版本异哈希/版本升级/history/降级拒绝
  * bun-hash：哈希校验、自报版本、权限判据（含 --perm-override 注入）、下载审计
  * diff：新增域名/文件/child_process 输出、闸门退出码、参数与目录错误
  * systemd-check：正例 + 5 个负例
  * 需要 root/服务器的两个脚本只验 --help 与 dry-run 计划

所有写操作限制在 mktemp 临时目录内，退出即清理；不改系统、不起服务。

选项：
  -h, --help   显示本帮助（不跑测试）

退出码： 0 全通过 / 1 有失败 / 4 参数错误
EOF
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
  '') ;;
  *) printf '参数错误：未知参数 %s\n（用 --help 查看用法）\n' "$1" >&2; exit 4 ;;
esac

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
TMP=$(mktemp -d) || { printf '无法创建临时目录\n' >&2; exit 1; }
cleanup() { rm -rf -- "$TMP"; }
trap cleanup EXIT

PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); printf '  PASS  %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$*"; }
skip() { SKIP=$((SKIP+1)); printf '  SKIP  %s\n' "$*"; }

section() { printf '\n### %s\n' "$*"; }

# 断言：命令输出包含指定文本
assert_contains() {
  local label=$1 needle=$2 haystack=$3
  if printf '%s' "$haystack" | grep -qF -- "$needle"; then ok "$label"; else
    bad "$label（期望包含「$needle」）"
    printf '     实际输出（前 8 行）：\n%s\n' "$(printf '%s\n' "$haystack" | head -8)"
  fi
}
assert_exit() {
  local label=$1 want=$2 got=$3
  if [ "$got" -eq "$want" ]; then ok "$label（exit=$got）"; else bad "$label（exit=$got，期望 $want）"; fi
}

# =============================================================================
section '0) bash -n 全部脚本'
shopt -s nullglob
scripts=("$HERE"/*.sh)
shopt -u nullglob
[ ${#scripts[@]} -gt 0 ] || { bad '没找到任何 .sh'; }
for s in "${scripts[@]}"; do
  if out=$(bash -n "$s" 2>&1); then
    ok "bash -n $(basename "$s")"
  else
    bad "bash -n $(basename "$s")：$out"
  fi
done

# =============================================================================
section '1) p8-artifact-lock.sh：记录 / 校验 / 篡改拒绝'
LOCK="$TMP/lock.tsv"; ART="$TMP/artifact.tgz"
printf 'fake-tarball-v1\n' > "$ART"
out=$("$HERE/p8-artifact-lock.sh" record --id demo --file "$ART" --version 1.0.0 --apply --lock "$LOCK" 2>&1)
assert_exit 'record 首次写入' 0 $?
assert_contains 'record 输出含「已写入」' '[apply] 已写入' "$out"

out=$("$HERE/p8-artifact-lock.sh" record --id demo --file "$ART" --version 1.0.0 --apply --lock "$LOCK" 2>&1)
assert_exit 'record 幂等（同版本同哈希）' 0 $?
assert_contains '幂等提示' '幂等' "$out"

out=$("$HERE/p8-artifact-lock.sh" verify --id demo --file "$ART" --version 1.0.0 --lock "$LOCK" 2>&1)
assert_exit 'verify 未篡改 -> 通过' 0 $?
assert_contains 'verify PASS 行' 'PASS' "$out"

printf 'fake-tarball-v1-TAMPERED\n' > "$ART"
out=$("$HERE/p8-artifact-lock.sh" verify --id demo --file "$ART" --lock "$LOCK" 2>&1)
assert_exit 'verify 篡改 -> 拒绝(2)' 2 $?
assert_contains '拒绝理由含「哈希不符」' '哈希不符' "$out"

out=$("$HERE/p8-artifact-lock.sh" record --id demo --file "$ART" --version 1.0.0 --apply --lock "$LOCK" 2>&1)
assert_exit '同版本不同内容 -> 拒绝(2)' 2 $?
assert_contains '红灯提示' '供应链红灯' "$out"

printf 'fake-tarball-v2\n' > "$ART"
out=$("$HERE/p8-artifact-lock.sh" record --id demo --file "$ART" --version 1.1.0 --apply --lock "$LOCK" 2>&1)
assert_exit '新版本可记录' 0 $?
if [ -f "$LOCK.history" ] && grep -q '1.0.0' "$LOCK.history"; then
  ok '旧记录进 history（可追溯）'
else bad '旧记录未进 history'; fi

out=$("$HERE/p8-artifact-lock.sh" verify --id demo --file "$ART" --version 1.0.0 --lock "$LOCK" 2>&1)
assert_exit 'verify 降级版本 -> 拒绝(2)' 2 $?

out=$("$HERE/p8-artifact-lock.sh" verify --id nosuch --file "$ART" --lock "$LOCK" 2>&1)
assert_exit 'verify 无锁记录 -> 3' 3 $?

# =============================================================================
section '2) p8-bun-hash.sh：哈希校验 / 权限 / 下载审计'
PIN="$TMP/bun-pin.tsv"; BUN="$TMP/bun"
printf '#!/bin/sh\necho "bun 1.4.4"\n' > "$BUN"; chmod 755 "$BUN"
out=$("$HERE/p8-bun-hash.sh" record --file "$BUN" --version 1.4.4 --apply --pin "$PIN" 2>&1)
assert_exit 'record bun 哈希' 0 $?
out=$("$HERE/p8-bun-hash.sh" verify --file "$BUN" --version 1.4.4 --pin "$PIN" 2>&1)
assert_exit 'verify bun 正常 -> 0' 0 $?
assert_contains '权限 PASS' '组/其他不可写' "$out"

printf '#!/bin/sh\necho "bun 1.4.4 EVIL"\n' > "$BUN"; chmod 755 "$BUN"
out=$("$HERE/p8-bun-hash.sh" verify --file "$BUN" --version 1.4.4 --pin "$PIN" 2>&1)
assert_exit 'verify bun 被替换 -> 拒绝(2)' 2 $?

# 先把内容恢复成 pin 记录的样子，否则哈希失败会掩盖权限判据的结果。
printf '#!/bin/sh\necho "bun 1.4.4"\n' > "$BUN"; chmod 755 "$BUN"
# 权限判据：本机（MSYS/Cygwin）**不映射组/其他写位**——chmod 757 后 stat 仍显示
# -rwxr-xr-x，实测确认。所以权限判据不能用真文件测，改用 --perm-override
# 直接喂符号权限串，这样判据逻辑本身仍被覆盖（不是跳过）。
out=$("$HERE/p8-bun-hash.sh" verify --file "$BUN" --version 1.4.4 --pin "$PIN" --perm-override '-rwxrwxr-x' 2>&1)
assert_exit '权限判据：组可写(-rwxrwxr-x) -> 拒绝(2)' 2 $?
assert_contains '权限判据命中组可写' '组或其他人可写' "$out"
out=$("$HERE/p8-bun-hash.sh" verify --file "$BUN" --version 1.4.4 --pin "$PIN" --perm-override '-rwxr-xrwx' 2>&1)
assert_exit '权限判据：其他可写(-rwxr-xrwx) -> 拒绝(2)' 2 $?
out=$("$HERE/p8-bun-hash.sh" verify --file "$BUN" --version 1.4.4 --pin "$PIN" --perm-override '-rwxr-xr-x' 2>&1)
assert_exit '权限判据：不可写(-rwxr-xr-x) -> 通过' 0 $?
out=$("$HERE/p8-bun-hash.sh" verify --file "$BUN" --version 1.4.4 --pin "$PIN" --perm-override '-rw-rw-rw-' 2>&1)
assert_exit '权限判据：644 组其他都可写 -> 拒绝(2)' 2 $?

mkdir -p "$TMP/audit-target"
printf 'curl -fsSL https://bun.sh/install | bash\n' > "$TMP/audit-target/bad-install.sh"
out=$("$HERE/p8-bun-hash.sh" audit --path "$TMP/audit-target" 2>&1)
assert_exit 'audit 命中运行时下载 -> 2' 2 $?
assert_contains 'audit 命中行' 'HIT' "$out"

mkdir -p "$TMP/audit-clean"
printf 'echo "bun is vendored at /opt/aiworkgate/bin/bun"\n' > "$TMP/audit-clean/ok.sh"
out=$("$HERE/p8-bun-hash.sh" audit --path "$TMP/audit-clean" 2>&1)
assert_exit 'audit 干净目录 -> 0' 0 $?

# =============================================================================
section '3) p8-diff-plugin-versions.sh：版本差异摘要'
OLD="$TMP/plug-old"; NEW="$TMP/plug-new"
mkdir -p "$OLD/lib" "$NEW/lib"
cat > "$OLD/manifest.json" <<'JSON'
{"id":"demo","version":"1.0.0","permissions":{"network_domains":["api.demo.com","www.demo.com"]}}
JSON
cat > "$NEW/manifest.json" <<'JSON'
{"id":"demo","version":"1.1.0","permissions":{"network_domains":["api.demo.com","www.demo.com","collect.evil.example"]}}
JSON
printf 'import { spawn } from "node:child_process";\nexport const v = 1;\n' > "$OLD/index.mjs"
printf 'import { spawn, execSync } from "node:child_process";\nexport const v = 1;\nconst o = execSync("id").toString();\n' > "$NEW/index.mjs"
printf 'export const u = 1;\n' > "$OLD/lib/util.mjs"
printf 'export const u = 2;\n' > "$NEW/lib/util.mjs"
printf 'import cp from "node:child_process";\ncp.exec("whoami");\n' > "$NEW/lib/extra.mjs"

out=$("$HERE/p8-diff-plugin-versions.sh" --old "$OLD" --new "$NEW" 2>&1); rc=$?
assert_exit 'diff 正常 -> 0' 0 $rc
assert_contains '版本变化' '1.0.0  ->  1.1.0' "$out"
assert_contains '新增域名被列出' 'collect.evil.example' "$out"
assert_contains '新增文件被列出' 'lib/extra.mjs' "$out"
assert_contains '新增 child_process 引用被列出' 'execSync } from "node:child_process"' "$out"
assert_contains '结论给出人工审查要求' '人工确认' "$out"

out=$("$HERE/p8-diff-plugin-versions.sh" --old "$OLD" --new "$NEW" --fail-on-new-domains 2>&1)
assert_exit '闸门：有新增域名 -> 2' 2 $?

out=$("$HERE/p8-diff-plugin-versions.sh" --old "$OLD" --new "$OLD" 2>&1)
assert_exit '自比 -> 0' 0 $?
assert_contains '自比无新增域名' '无新增域名' "$out"

out=$("$HERE/p8-diff-plugin-versions.sh" --old "$TMP/nope" --new "$NEW" 2>&1)
assert_exit '目录不存在 -> 3' 3 $?

out=$("$HERE/p8-diff-plugin-versions.sh" --old "$OLD" --new "$NEW" --bogus 2>&1)
assert_exit '未知参数 -> 4' 4 $?

# =============================================================================
section '4) p8-systemd-check.sh：正例 + 负例'
UNIT="$HERE/systemd/aiworkgate-plugin@.service"
out=$("$HERE/p8-systemd-check.sh" --unit-file "$UNIT" 2>&1)
assert_exit '样例单元自检通过' 0 $?
assert_contains '关键项 NoNewPrivileges' 'NoNewPrivileges=yes' "$out"
assert_contains '关键项 RestrictAddressFamilies' 'RestrictAddressFamilies=AF_INET AF_INET6' "$out"
assert_contains 'MDE 取舍已记录' '取舍已书面记录' "$out"

mutate() { sed "$2" "$UNIT" > "$1"; }
mutate "$TMP/u1.service" '/^NoNewPrivileges=/d'
out=$("$HERE/p8-systemd-check.sh" --unit-file "$TMP/u1.service" 2>&1)
assert_exit '负例：删 NoNewPrivileges -> 1' 1 $?

mutate "$TMP/u2.service" 's/^User=aiworkgate-plugin$/User=aiworkgate-plugin  # tail comment/'
out=$("$HERE/p8-systemd-check.sh" --unit-file "$TMP/u2.service" 2>&1)
assert_exit '负例：行尾注释 -> 1' 1 $?

{ cat "$UNIT"; printf 'ProtectSystem=full\n'; } > "$TMP/u3.service"
out=$("$HERE/p8-systemd-check.sh" --unit-file "$TMP/u3.service" 2>&1)
assert_contains '负例：重复 key 被抓' '同一 key 重复' "$out"

sed '/^# ---- 冲突项/,$d' "$UNIT" > "$TMP/u4.service"
printf 'MemoryDenyWriteExecute=no\n' >> "$TMP/u4.service"
out=$("$HERE/p8-systemd-check.sh" --unit-file "$TMP/u4.service" 2>&1)
assert_contains '负例：MDE 无书面理由被抓' '取舍记录缺失' "$out"

mutate "$TMP/u5.service" 's/^User=aiworkgate-plugin$/User=root/'
out=$("$HERE/p8-systemd-check.sh" --unit-file "$TMP/u5.service" 2>&1)
assert_contains '负例：User=root 被抓' 'User=root' "$out"

# =============================================================================
section '5) 需要服务器/root 的脚本：只验 --help 与 dry-run 计划'
for s in p8-create-plugin-user.sh p8-verify-isolation.sh; do
  out=$("$HERE/$s" --help 2>&1); assert_exit "$s --help -> 0" 0 $?
  assert_contains "$s --help 有用法" '用法' "$out"
done

out=$("$HERE/p8-create-plugin-user.sh" 2>&1)
assert_exit 'create-plugin-user dry-run -> 0' 0 $?
assert_contains 'dry-run 列出凭据目录步骤' 'credentials' "$out"
assert_contains 'dry-run 明确未执行' '均未执行' "$out"
assert_contains 'dry-run 暴露换 uid 的真实缺口' 'systemd-run' "$out"

case "$(uname -s)" in
  Linux) skip 'Linux 上仍不自动 --apply（改系统必须人确认）；请在服务器手工执行' ;;
  *)     skip '非 Linux：useradd/install -d/systemd-analyze 无法执行，属预期跳过' ;;
esac

# =============================================================================
printf '\n===============================================\n'
printf '汇总：PASS=%d FAIL=%d SKIP=%d\n' "$PASS" "$FAIL" "$SKIP"
if [ "$FAIL" -ne 0 ]; then
  printf '结果：FAILED（%d 项失败）\n' "$FAIL"
  exit 1
fi
printf '结果：PASSED（本机逻辑层全通过；Linux 系统行为仍待服务器实跑）\n'
exit 0
