#!/usr/bin/env bash
# Deploys the Linux host to a server over SSH (P7).
#
#   scripts/deploy-linux.sh user@host            # uses build/linux-amd64/aiworkgate
#   scripts/deploy-linux.sh user@host --binary /path/to/aiworkgate
#   scripts/deploy-linux.sh user@host --dry-run  # print the exact commands only
#
# The previous binary is kept as /opt/aiworkgate/aiworkgate.prev; use
# scripts/rollback-linux.sh to go back. No secrets are copied by this script.
set -euo pipefail

cd "$(dirname "$0")/.."

TARGET="${1:-}"
shift || true
BINARY="build/linux-amd64/aiworkgate"
DRY_RUN=0

while [ $# -gt 0 ]; do
  case "$1" in
    --binary)
      if [ $# -lt 2 ] || [ -z "${2:-}" ]; then
        echo "missing value for --binary" >&2
        exit 2
      fi
      BINARY="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$TARGET" ]; then
  echo "usage: $0 user@host [--binary path] [--dry-run]" >&2
  exit 2
fi
# A missing user@host that leaves only flags would otherwise treat "--dry-run"
# as the ssh target and run a real deployment.
case "$TARGET" in
  -*) echo "first argument must be user@host, got \"$TARGET\"" >&2; exit 2 ;;
esac
if [ ! -f "$BINARY" ]; then
  echo "binary not found: $BINARY (run scripts/build-linux.sh first)" >&2
  exit 1
fi

SUM="$(sha256sum "$BINARY" | awk '{print $1}')"
REMOTE_TMP="/tmp/aiworkgate-$(date +%Y%m%d%H%M%S)"

run() {
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "+ $*"
  else
    "$@"
  fi
}

echo "==> target: $TARGET"
echo "==> binary: $BINARY (sha256 $SUM)"

# 1. Keep the current binary for rollback. A first install has nothing to keep.
run ssh "$TARGET" "sudo mkdir -p /opt/aiworkgate && if [ -f /opt/aiworkgate/aiworkgate ]; then sudo cp -p /opt/aiworkgate/aiworkgate /opt/aiworkgate/aiworkgate.prev; fi"

# 2. Upload out of place, verify, then swap atomically: install to .new and
# rename within the same directory — `install` onto the live file would be a
# truncate+overwrite, leaving a truncated binary if interrupted.
run scp "$BINARY" "$TARGET:$REMOTE_TMP"
run ssh "$TARGET" "echo '$SUM  $REMOTE_TMP' | sha256sum -c - && sudo install -m 0755 '$REMOTE_TMP' /opt/aiworkgate/aiworkgate.new && sudo mv -f /opt/aiworkgate/aiworkgate.new /opt/aiworkgate/aiworkgate && rm -f '$REMOTE_TMP'"

# 3. Restart and wait for readiness.
run ssh "$TARGET" "sudo systemctl restart aiworkgate && for i in \$(seq 1 20); do curl -fsS http://127.0.0.1:7865/healthz >/dev/null && exit 0; sleep 1; done; echo 'health check failed; run scripts/rollback-linux.sh' >&2; exit 1"

echo "==> deployed; verify from your machine:"
echo "    curl -sS -o /dev/null -w '%{http_code}\\n' https://<domain>/v1/models"
echo "    curl -sS -H 'Authorization: Bearer <key>' https://<domain>/v1/models | head -c 200"
