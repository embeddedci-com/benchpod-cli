#!/usr/bin/env bash
# Hardware end-to-end check of the benchpod CLI against a real pod, over any connection.
#
#   scripts/hw-e2e.sh <connection> [options]
#
#   <connection>     what --connection gets: usb, usb:<name>, a device path, 192.168.1.5, ...
#   --bin PATH       the benchpod binary to test (default: build ./cmd/benchpod-cli)
#   --flash ELF      also flash this DUT image over SWD (--target, pins from the wiring profile)
#   --target CFG     OpenOCD target config for --flash (default target/stm32f4x.cfg)
#   --power N        power the target from eFuse N (1 internal 5V, 2 external) for --flash; the
#                    DUT stays powered afterwards
#   --flash-args "A" extra flags for --flash, e.g. "--swclk 11 --swdio 12" when the computer is not
#                    signed in (the wiring profile on embeddedci.com names them otherwise)
#   --blobs          also reinstall the gw1 blob (install-blobs --only gw1 --force)
#   --cloudcfg       also set and clear a test cloud proxy and company CA on the pod
#   --reattach       also run usb-reattach and check the pod answers again (USB connections)
#
# Every step prints PASS, FAIL or SKIP; the last line is the summary and the exit code is the
# number of failures (0 = all passed). It changes nothing it does not put back: the proxy and the
# CA are cleared again, and gw1 is rewritten with the release's own image. Hold the bench lock.
set -u
CONN=${1:?usage: scripts/hw-e2e.sh <connection> [--bin PATH] [--flash ELF] [--target CFG] [--power N] [--flash-args "..."] [--blobs] [--cloudcfg] [--reattach]}
shift
BIN="" FLASH="" TARGET="target/stm32f4x.cfg" POWER="" FLASHARGS="" BLOBS=0 CLOUDCFG=0 REATTACH=0
while [ $# -gt 0 ]; do
	case "$1" in
	--bin) BIN=$2; shift ;;
	--flash) FLASH=$2; shift ;;
	--target) TARGET=$2; shift ;;
	--power) POWER=$2; shift ;;
	--flash-args) FLASHARGS=$2; shift ;;
	--blobs) BLOBS=1 ;;
	--cloudcfg) CLOUDCFG=1 ;;
	--reattach) REATTACH=1 ;;
	*) echo "unknown option $1" >&2; exit 2 ;;
	esac
	shift
done
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
if [ -z "$BIN" ]; then
	BIN=$TMP/benchpod
	(cd "$(dirname "$0")/.." && go build -o "$BIN" ./cmd/benchpod-cli) || exit 1
fi

PASS=0 FAIL=0 SKIP=0
# step NAME REGEX -- command args: runs `benchpod args --connection CONN`, passes when it exits 0
# and its output matches REGEX (extended, case-insensitive).
step() {
	local name=$1 re=$2
	shift 3
	local out rc
	out=$("$BIN" "$@" --connection "$CONN" 2>&1)
	rc=$?
	if [ $rc -eq 0 ] && grep -Eiq -- "$re" <<<"$out"; then
		PASS=$((PASS + 1)); echo "PASS  $name"
	else
		FAIL=$((FAIL + 1)); echo "FAIL  $name (exit $rc)"; grep -v '\[serial\]' <<<"$out" | tail -5 | sed 's/^/      /'
	fi
}
skip() { SKIP=$((SKIP + 1)); echo "SKIP  $1 ($2)"; }

step "status"            'BenchPod|device : benchpod'          -- status
step "ping"              'pong'                                -- ping
step "la voltage"        'LA voltage'                          -- la voltage
step "la status"         '.'                                   -- la status
step "capture 256"       '^255,'                               -- capture --samples 256 --output csv
step "show-network"      'IP:'                                 -- show-network
step "lan-policy"        'LAN policy'                          -- lan-policy
step "sig-policy"        'Signature policy'                    -- sig-policy
step "identity"          'benchpod-[0-9a-f]{6}'                -- identity
step "cloud proxy show"  'proxy'                               -- cloud proxy show
step "cloud ca show"     'CA|certificate|none'                 -- cloud ca show

if [ $CLOUDCFG -eq 1 ]; then
	step "cloud proxy set"   '10\.255\.255\.1:3128'            -- cloud proxy set 10.255.255.1:3128
	step "cloud proxy clear" 'none|without a proxy|directly'   -- cloud proxy clear
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=benchpod hw-e2e test CA" \
		-addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign" \
		-keyout "$TMP/ca.key" -out "$TMP/ca.pem" >/dev/null 2>&1
	step "cloud ca set"      'hw-e2e test CA'                  -- cloud ca set "$TMP/ca.pem"
	step "cloud ca clear"    'none|built-in'                   -- cloud ca clear
else
	skip "cloud proxy/ca set+clear" "pass --cloudcfg"
fi

if [ $BLOBS -eq 1 ]; then
	step "install-blobs gw1" 'gw1 installed'                   -- install-blobs --only gw1 --force
else
	skip "install-blobs" "pass --blobs"
fi

if [ -n "$FLASH" ]; then
	step "flash (SWD)"       'flash: done|Verified OK|verified' -- flash --file "$FLASH" --target "$TARGET" ${POWER:+--target-power "$POWER"} $FLASHARGS
else
	skip "flash" "pass --flash ELF"
fi

if [ $REATTACH -eq 1 ]; then
	step "usb-reattach"      'reconnected'                     -- usb-reattach
	sleep 10
	step "ping after reattach" 'pong'                          -- ping
else
	skip "usb-reattach" "pass --reattach"
fi

echo "hw-e2e $CONN: $PASS passed, $FAIL failed, $SKIP skipped"
exit $FAIL
