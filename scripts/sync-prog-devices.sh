#!/usr/bin/env bash
# Copy the prog_flash device table (prog_devices.json and its README) from the embeddedci-python SDK
# into the CLI, which embeds it (internal/progdev).
#
#   scripts/sync-prog-devices.sh [path/to/embeddedci-python]   # default: ../embeddedci-python
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
sdk="${1:-$here/../embeddedci-python}"
dir="$sdk/packages/embeddedci/src/embeddedci/benchpod"
[ -f "$dir/prog_devices.json" ] || { echo "sync-prog-devices: no $dir/prog_devices.json (pass the embeddedci-python checkout)" >&2; exit 1; }
cp "$dir/prog_devices.json" "$here/internal/progdev/prog_devices.json"
cp "$dir/prog_devices.README.md" "$here/internal/progdev/prog_devices.README.md"
echo "sync-prog-devices: copied $(grep -c '"name"' "$dir/prog_devices.json") lines with a name from $dir"
