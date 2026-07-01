#!/usr/bin/env bash
# build/rete/build.sh — regenerate the vendored Rete.js bundle.
#
# This is an OCCASIONAL, MANUAL step (version bumps, plugin changes) — it is
# NEVER run in the normal dev/CI build or test path. `make build`/`make test`
# ship the committed blob at internal/server/static/vendor/rete/rete.min.js and
# never invoke node. Run this only to refresh that blob:
#
#     make vendor-rete            # (from the repo root)
#   or
#     build/rete/build.sh
#
# Requires node + npm on PATH (developed against node v24 / npm 11). All npm work
# happens in a throwaway temp dir so node_modules / package-lock never land in
# the repo — only the built ESM blob is copied back in.
#
# To bump a version: change the pins below AND the table in
# internal/server/static/vendor/rete/README.md, then re-run.
set -euo pipefail

# --- Pinned versions (keep in lockstep with vendor/rete/README.md) -----------
RETE="rete@2.0.6"
AREA="rete-area-plugin@2.1.5"
CONNECTION="rete-connection-plugin@2.0.5"
RENDER_UTILS="rete-render-utils@2.0.3"
REACT_PLUGIN="rete-react-plugin@2.1.0"
REACT="react@19.2.7"
REACT_DOM="react-dom@19.2.7"
STYLED="styled-components@6.4.3"
ESBUILD="esbuild@0.28.1"

# --- Paths -------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
OUT_DIR="${REPO_ROOT}/internal/server/static/vendor/rete"
OUT_FILE="${OUT_DIR}/rete.min.js"
ENTRY="${SCRIPT_DIR}/entry.mjs"

command -v node >/dev/null 2>&1 || { echo "error: node not on PATH"; exit 1; }
command -v npm  >/dev/null 2>&1 || { echo "error: npm not on PATH"; exit 1; }

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/rete-build.XXXXXX")"
trap 'rm -rf "${TMP_DIR}"' EXIT

echo "==> node $(node --version), npm $(npm --version)"
echo "==> building in ${TMP_DIR}"

cp "${ENTRY}" "${TMP_DIR}/entry.mjs"
cd "${TMP_DIR}"
npm init -y >/dev/null 2>&1
npm install --no-audit --no-fund \
  "${RETE}" "${AREA}" "${CONNECTION}" "${RENDER_UTILS}" "${REACT_PLUGIN}" \
  "${REACT}" "${REACT_DOM}" "${STYLED}" "${ESBUILD}"

./node_modules/.bin/esbuild entry.mjs \
  --bundle --format=esm --minify \
  --target=es2020 \
  --define:process.env.NODE_ENV='"production"' \
  --define:process.env.NODE_DEBUG=false \
  --legal-comments=none \
  --outfile=rete.min.js

mkdir -p "${OUT_DIR}"
cp "${TMP_DIR}/rete.min.js" "${OUT_FILE}"

echo "==> wrote ${OUT_FILE}"
echo "==> sha256:"
if command -v shasum >/dev/null 2>&1; then shasum -a 256 "${OUT_FILE}";
else sha256sum "${OUT_FILE}"; fi
echo "==> done. Review the diff, run 'make test', and commit the blob."
