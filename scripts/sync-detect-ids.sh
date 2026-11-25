#!/usr/bin/env bash
# Copy the detect ID table (detect_ids.json) from the embeddedci-python SDK into the CLI, which
# embeds it (internal/detect).
#
#   scripts/sync-detect-ids.sh [path/to/embeddedci-python]   # default: ../embeddedci-python
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
sdk="${1:-$here/../embeddedci-python}"
src="$sdk/packages/embeddedci/src/embeddedci/benchpod/detect_ids.json"
[ -f "$src" ] || { echo "sync-detect-ids: no $src (pass the embeddedci-python checkout)" >&2; exit 1; }
cp "$src" "$here/internal/detect/detect_ids.json"
echo "sync-detect-ids: copied $src"
