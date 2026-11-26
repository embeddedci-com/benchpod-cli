#!/usr/bin/env bash
# Regenerate the INA chip table (internal/currentmonitor/chips.json) from the embeddedci-python SDK's
# CURRENT_MONITOR_CHIPS, which the CLI embeds.
#
#   scripts/sync-current-monitor-chips.sh [path/to/embeddedci-python]   # default: ../embeddedci-python
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
sdk="${1:-$here/../embeddedci-python}"
[ -d "$sdk/packages/embeddedci/src" ] || { echo "sync-current-monitor-chips: no SDK at $sdk" >&2; exit 1; }
PYTHONPATH="$sdk/packages/embeddedci/src" python3 -c "
import json, dataclasses
from embeddedci.benchpod import CURRENT_MONITOR_CHIPS
print(json.dumps({'_source': 'embeddedci-python: scripts/sync-current-monitor-chips.sh', 'chips': [dataclasses.asdict(c) for c in CURRENT_MONITOR_CHIPS]}, indent=2))
" > "$here/internal/currentmonitor/chips.json"
echo "sync-current-monitor-chips: wrote internal/currentmonitor/chips.json"
