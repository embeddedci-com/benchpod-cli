#!/usr/bin/env bash
# Copy the BenchPod features table (features/features.json, generated in benchpod-firmware from
# features/features.toml) into the CLI, which embeds it (internal/features).
#
#   scripts/sync-feature-map.sh [path/to/benchpod-firmware]   # default: ../benchpod-firmware
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
fw="${1:-$here/../benchpod-firmware}"
src="$fw/features/features.json"
[ -f "$src" ] || { echo "sync-feature-map: no $src (pass the benchpod-firmware checkout)" >&2; exit 1; }
cp "$src" "$here/internal/features/features.json"
echo "sync-feature-map: copied $(grep -c '"rev"' "$src") features"
