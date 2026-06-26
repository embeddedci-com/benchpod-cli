# benchpod-cli

`benchpod` is the EmbeddedCI **bench pod** command-line tool. It talks to a
bench pod either over its TCP/JSON API (port 8080) or over its USB CDC-ACM
serial console, and drives the pod's hardware: GPIO, signal generation,
measurement, scope capture/streaming, Wi-Fi configuration, BOOTSEL, and
firmware flashing over SWD.

## Installation

Pre-built binaries for **macOS, Linux and Windows** (amd64 + arm64) are attached
to every [GitHub release](https://github.com/embeddedci-com/benchpod-cli/releases).

### Homebrew (macOS / Linux)

```sh
brew install --cask embeddedci-com/tap/benchpod
```

This taps `embeddedci-com/homebrew-tap` and installs the `benchpod` binary;
`brew upgrade --cask benchpod` later picks up new releases. The macOS binary is
not Apple-notarized, so the cask's post-install hook strips the
`com.apple.quarantine` attribute that Homebrew sets — otherwise macOS Gatekeeper
would refuse to run the unsigned binary (even from the terminal). This
deliberately bypasses Gatekeeper for our own binary; if you'd rather not, use a
notarized build (not currently produced) or build from source.

### Direct download

Grab the archive for your OS/arch and put the binary on your `PATH`. The
`releases/latest/download/…` URLs always point at the newest release. For
example, macOS arm64:

```sh
curl -fsSL -o benchpod.tar.gz \
  https://github.com/embeddedci-com/benchpod-cli/releases/latest/download/benchpod_Darwin_arm64.tar.gz
tar -xzf benchpod.tar.gz benchpod
sudo mv benchpod /usr/local/bin/
benchpod --version
```

Swap `Darwin_arm64` for `Darwin_x86_64`, `Linux_x86_64`, `Linux_arm64`, etc. On
Windows download `benchpod_Windows_x86_64.zip` and extract `benchpod.exe`.
`releases/latest/download/checksums.txt` verifies the download.

> **macOS, direct download only:** if you fetch the archive with a *browser*
> (not curl/Homebrew) it gets the quarantine flag and double-clicking is
> blocked. Run it from the terminal, or clear the flag once:
> `xattr -d com.apple.quarantine /usr/local/bin/benchpod`.

### From source

You need Go (see `go.mod` for the version), then `make build`. On macOS the
build requires cgo (`CGO_ENABLED=1`, the default for a native build) because USB
serial-port enumeration uses the IOKit framework; Linux and Windows are pure Go.

### OpenOCD (for `flash`)

The `flash` command shells out to **OpenOCD**, which must be installed
separately and on your `PATH`. It needs a build with the `cmsis_dap_tcp`
backend (post-0.12.0) — on macOS that currently means
`brew install --HEAD open-ocd`. See [Flashing: CMSIS-DAP](#flashing-cmsis-dap)
below.

### dfu-util (for `flash-self`)

`flash-self` reflashes a **bench pod's own firmware** over USB DFU (STM32) and
shells out to **dfu-util**. The Homebrew cask declares it as a dependency, so
`brew install --cask embeddedci-com/tap/benchpod` pulls it in automatically;
otherwise `brew install dfu-util` (or your distro's package). See
[Flashing the pod itself](#flashing-the-pod-itself-stm32-usb-dfu) below.

## Connection

The global `--connection` flag is the single place that says where and how to
reach the pod; the transport is inferred from its value:

| `--connection` value             | Transport                                              |
|----------------------------------|--------------------------------------------------------|
| `192.168.1.5[:8080]`             | TCP/JSON API (an address ⇒ Wi-Fi).                     |
| `/dev/tty...`, `COM3`            | USB serial console, explicit device path.              |
| `serial`                         | USB serial console, auto-detected (USB VID `2E8A`).    |
| *(omitted)*                      | the default saved by `benchpod set-connection`.        |

Every flag is also settable via a `BENCHPOD_*` environment variable
(e.g. `BENCHPOD_CONNECTION=serial`), with precedence flag > env > default.

The firmware itself is unauthenticated; `benchpod login` is independent of the
device path and authenticates with `embeddedci-server` (device-login flow) for
cloud features. Direct firmware commands do not send tokens.

Only `flash` (SWD) is implemented over the serial console today; the other
TCP/JSON commands reject a serial connection with a clear message. The
`set-wifi` / `show-wifi` / `clear-wifi` and `bootsel` subcommands always use
the serial console regardless of `--connection` (a device path still selects
the port).

## CLI usage

```sh
# Save a default connection so later commands can omit --connection:
benchpod set-connection 192.168.1.5
benchpod set-connection serial

# Reachability / state:
benchpod ping
benchpod status

# Hardware:
benchpod set-gpio PIN STATE
benchpod step-gpio
benchpod generate ...
benchpod measure ...
benchpod capture ...
benchpod stream ...
benchpod test ...

# Wi-Fi (always over the serial console):
benchpod set-wifi
benchpod show-wifi
benchpod clear-wifi

# Firmware:
benchpod flash ...        # flash a TARGET/DUT wired to the pod (SWD via OpenOCD CMSIS-DAP)
benchpod bootsel          # reboot an RP2350 pod into its UF2 bootloader (serial console only)
benchpod dfu              # reboot an STM32 pod into its USB DFU bootloader (serial console only)
benchpod flash-self                  # fetch latest firmware + flash the POD over USB DFU (STM32)
benchpod flash-self --enter-dfu      # …rebooting a running pod into DFU first
benchpod flash-self ./fw.bin         # …flashing a specific local build instead

# Cloud auth (optional):
benchpod login [--server-url https://www.embeddedci.com]
benchpod register ...
```

Run `benchpod <command> -h` for the flags of any subcommand.

#### Flashing: CMSIS-DAP

`flash` always drives **host-side OpenOCD** — the pod holds no flash
intelligence and OpenOCD's exit code is the verdict. The pod runs a CMSIS-DAP
processor locally and OpenOCD's `cmsis-dap` **TCP backend** ships whole DAP
transfers (`DAP_Transfer`/`DAP_TransferBlock`), so a flash is one round-trip per
DAP command instead of thousands of per-bit round-trips — fast even over the
network. Works on **TCP** (`dap_start`) and **serial** (the console `dap-start`
command). It needs a **recent OpenOCD with the `cmsis_dap_tcp` backend**
(post-0.12.0; e.g. `brew install --HEAD open-ocd`); the CLI checks for the
backend up front and fails with clear advice if it is missing.

The legacy per-bit `remote_bitbang` path has been retired — the pod no longer
speaks it.

#### Flashing the pod itself (STM32, USB DFU)

`flash` programs a *target* wired to the pod. To flash the **pod's own
firmware** there are two paths, mirroring how the two pod MCUs boot:

- **RP2350 pod** — drag-and-drop UF2. `benchpod bootsel` reboots a running pod
  into the UF2 drive; for a blank board hold the physical BOOT0 button. Drop
  `firmware.uf2` onto the `RPI-RP2` drive (or use `picotool`).
- **STM32 pod** — USB DFU via `dfu-util`, no SWD probe needed. A first-time user
  just runs `benchpod flash-self` with **no arguments**: the latest prebuilt
  firmware is fetched automatically (no toolchain, no manual download), the pod
  is detected in DFU mode — with on-screen guidance and a wait if it isn't yet —
  and dfu-util writes it.

  ```
  benchpod flash-self
  ```

  The device must be in its ROM DFU bootloader; `flash-self` waits (up to
  `--wait`, default 60s) and tells you how to get there:
  - **first flash / blank board:** hold **BOOT0 high at reset** (hardware only —
    a board with no firmware can't be driven over USB yet), then run the command.
  - **re-flash a running pod:** add `--enter-dfu` to reboot it into DFU for you
    (equivalent to `benchpod dfu` first):
    ```
    benchpod flash-self --enter-dfu
    ```

  Overrides: a local path (`benchpod flash-self ./bench_pod_stm32.bin`), an
  explicit `--firmware-url`, or a pinned `--firmware-version v0.0.5`.

The prebuilt firmware is fetched from the public
[`embeddedci-com/benchpod-firmware`](https://github.com/embeddedci-com/benchpod-firmware)
releases (the firmware source repo is private); a published `.sha256` is
verified when present. Under the hood `flash-self` runs
`dfu-util -a 0 --dfuse-address 0x08000000:leave -D <firmware.bin>`; `:leave`
starts the new firmware once the write verifies. The firmware side is the `dfu`
console command, which jumps to the STM32 system-memory bootloader (the same
flow the firmware `make flash-dfu` target uses).

### Global flags

| Flag                | Default                          | Purpose                                                           |
|---------------------|----------------------------------|-------------------------------------------------------------------|
| `--connection`      | (saved `set-connection` target)  | Address, device path, or `serial` — see the table above.          |
| `--config-file`     | (none)                           | Path to a config file.                                            |
| `--output-filename` | (stdout)                         | Write command output to this file instead of stdout.             |
| `--timeout`         | `0` (per-command default)        | Overall command deadline; `0` uses each command's own default.    |

### Makefile targets

| Target        | What it does                                                 |
|---------------|--------------------------------------------------------------|
| `make build`  | Build `bin/benchpod` for the host.                           |
| `make run`    | Build and run the binary (pass args via `ARGS="..."`).       |
| `make test`   | `go test ./...`.                                             |
| `make vet`    | `go vet ./...`.                                              |
| `make fmt`    | `go fmt ./...`.                                              |
| `make tidy`   | `go mod tidy`.                                               |
| `make clean`  | Remove `bin/`.                                               |

## Releasing

Releases are cut by [GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`)
from the `release` workflow, triggered by pushing a `v*` tag:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

The job runs on a **macOS** runner (the macOS binaries need cgo/IOKit; Linux and
Windows cross-compile from there with cgo off) and produces:

- the **GitHub Release** in this repo, with all six archives + `checksums.txt` —
  created with the workflow's default `GITHUB_TOKEN`;
- the updated `benchpod` cask in **`embeddedci-com/homebrew-tap`**. That tap is a
  separate repo, so the default token can't push to it; the cask step uses a
  **`TAP_GITHUB_TOKEN`** secret (a PAT with write access to the tap repo).

Every push/PR also runs a GoReleaser `--snapshot` build (`ci` workflow) so a
cross-platform break is caught before tagging.

## Layout

| Path                       | Purpose                                                                       |
|----------------------------|-------------------------------------------------------------------------------|
| `cmd/benchpod-cli/`        | CLI entry point and all subcommands (Cobra + Viper).                          |
| `internal/tcpclient/`      | TCP/JSON API client (commands, identity, SWD).                                |
| `internal/serialconsole/`  | USB CDC-ACM serial console client and port auto-detection.                    |
| `internal/openocd/`        | OpenOCD wrapper used by the `flash` command.                                  |
| `internal/benchpodconfig/` | Local config (the saved `set-connection` target).                            |
| `internal/serverapi/`      | HTTP client for `embeddedci-server` (device-login, refresh).                  |
| `internal/authstore/`      | Token cache (`~/.config/benchpod-cli/token.json`).                            |
