#!/usr/bin/env bash
# Builds the Linux host for a server deployment (P5-6).
#
#   web:  npm run build (only when internal/webui/dist is missing)
#   host: CGO_ENABLED=0 cross-compile with the version stamped in
#
# Output: build/linux-<arch>/aiworkgate (+ SHA256SUMS). build/ is gitignored.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-0.1.0-dev}"
ARCH="${ARCH:-amd64}"
OUT="build/linux-${ARCH}"

if [ ! -f internal/webui/dist/index.html ]; then
  echo "internal/webui/dist is missing; run 'npm run build' in web/ first." >&2
  exit 1
fi

mkdir -p "$OUT"
echo "==> linux/${ARCH} build ${VERSION}"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath \
  -ldflags "-s -w -X aiworkgate/internal/app.Version=${VERSION}" \
  -o "$OUT/aiworkgate" ./cmd/aiworkgate

sha256sum "$OUT/aiworkgate" | tee "$OUT/SHA256SUMS"
ls -lh "$OUT/aiworkgate"
