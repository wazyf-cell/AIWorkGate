#!/usr/bin/env bash
# Rolls the Linux host back to the previous binary (P7).
#
#   scripts/rollback-linux.sh user@host [--dry-run]
set -euo pipefail

cd "$(dirname "$0")/.."

TARGET="${1:-}"
shift || true
DRY_RUN=0
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done
if [ -z "$TARGET" ]; then
  echo "usage: $0 user@host [--dry-run]" >&2
  exit 2
fi

run() {
  if [ "$DRY_RUN" -eq 1 ]; then echo "+ $*"; else "$@"; fi
}

run ssh "$TARGET" "test -f /opt/aiworkgate/aiworkgate.prev || { echo 'no previous binary; refusing to guess' >&2; exit 1; } && sudo install -m 0755 /opt/aiworkgate/aiworkgate.prev /opt/aiworkgate/aiworkgate && sudo systemctl restart aiworkgate && for i in \$(seq 1 20); do curl -fsS http://127.0.0.1:7865/healthz >/dev/null && echo rolled-back && exit 0; sleep 1; done; echo 'rollback health check failed' >&2; exit 1"

echo "==> rollback done. Diagnose with: ssh $TARGET 'journalctl -u aiworkgate -n 200 --no-pager'"
