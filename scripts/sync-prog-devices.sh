#!/usr/bin/env bash
# Copy the prog_flash device table (prog_devices.json) from the embeddedci-python SDK into the CLI,
# which embeds it (internal/progdev).
#
#   scripts/sync-prog-devices.sh path/to/prog_devices.json
#   scripts/sync-prog-devices.sh [path/to/embeddedci-python]   # looks for prog_devices.json inside
# TODO: pin the SDK path once the embeddedci-python PR that adds the table has merged.
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
arg="${1:-$here/../embeddedci-python}"
if [ -d "$arg" ]; then
  src="$(find "$arg" -name prog_devices.json -not -path '*/node_modules/*' | head -1)"
else
  src="$arg"
fi
[ -f "${src:-}" ] || { echo "sync-prog-devices: no prog_devices.json (pass the file or the embeddedci-python checkout)" >&2; exit 1; }
cp "$src" "$here/internal/progdev/prog_devices.json"
echo "sync-prog-devices: copied $src"
