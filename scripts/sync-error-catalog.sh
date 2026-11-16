#!/usr/bin/env bash
# Copy the BenchPod error catalog (errors/errors.json, generated in benchpod-firmware from
# errors/catalog.toml) into the CLI (internal/errcatalog), which renders refusals from it.
#
#   scripts/sync-error-catalog.sh [path/to/benchpod-firmware]   # default: ../benchpod-firmware
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
fw="${1:-$here/../benchpod-firmware}"
src="$fw/errors/errors.json"
[ -f "$src" ] || { echo "sync-error-catalog: no $src (pass the benchpod-firmware checkout)" >&2; exit 1; }
cp "$src" "$here/internal/errcatalog/errors.json"
echo "sync-error-catalog: copied $(grep -c '"name"' "$src") codes"
