#!/usr/bin/env bash
# Write the prog_flash device table (prog_devices.json and its README) into the CLI, which embeds
# it (internal/progdev). The table is generated from the one YAML file per chip in
# embeddedci-python's prog-devices/ by its scripts/gen_prog_devices.py; never edit the copy.
#
#   scripts/sync-prog-devices.sh [path/to/embeddedci-python]   # default: ../embeddedci-python
#   scripts/sync-prog-devices.sh --check [path]                # CI: fail when the copy is stale
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
check=""
if [ "${1:-}" = "--check" ]; then check="--check"; shift; fi
sdk="${1:-$here/../embeddedci-python}"
gen="$sdk/scripts/gen_prog_devices.py"
[ -f "$gen" ] || { echo "sync-prog-devices: no $gen (pass the embeddedci-python checkout)" >&2; exit 1; }
python3 "$gen" --src "$sdk/prog-devices" --cli "$here" $check
