# benchpod-cli

`benchpod` is the EmbeddedCI **bench pod** command-line tool. It talks to a
bench pod either over its TCP/JSON API (port 8080) or over its USB console
(CDC-ACM), and drives the pod's hardware: signal generation, ADC capture,
logic-analyzer pins, Wi-Fi configuration, firmware flashing over SWD, SPI NOR
flash programming, and the pod's own firmware, policies and cloud link. Every
command and flag is listed in the [command reference](#command-reference).

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

Firmware from 3.5.0 on keeps the iCE40 gateware and the ESP32-C3 Wi-Fi image in the pod's
W25Q flash rather than in its image. After writing the firmware, `flash-self` waits for the
pod's USB console and installs the ones it lacks from the same release (or from `blobs/`
next to a local `.bin`); `install-blobs` does that step on its own, and `set-wifi` runs it
first when the ESP32-C3 image is missing. `flash-self` also refuses an image that does not
fit the pod's flash (a 1 MB STM32H563 cannot take firmware built for the 2 MB part).

Releases can publish a signed manifest, `<asset>.sig`, next to `bench_pod_stm32.bin` and each
`blob-*.bin`. `flash-self` and the blob install check it against the release keys (plus your
developer key, if `~/.config/benchpod/fw-signing-dev.key` exists) and print one line, for
example `signature: ok (release-1 key 9b5cc58445e62d88, release 3.6.0)` or `signature: none (this
release is not signed)`. A local file's signature is read from `<file>.sig`. Firmware that
checks signatures itself also gets the manifest with each USB upload, and its verdict is
printed as `pod signature check: ...`. For now this only reports: a missing or failing
signature never stops a flash or an install.

## Connection

The global `--connection` flag is the single place that says where and how to
reach the pod; the transport is inferred from its value:

| `--connection` value             | Transport                                              |
|----------------------------------|--------------------------------------------------------|
| `192.168.1.5[:8080]`             | TCP/JSON API (an address ⇒ Wi-Fi).                     |
| `/dev/tty...`, `COM3`            | USB console, explicit device path.                     |
| `usb`                            | USB console, auto-detected by probing the ports.       |
| `embeddedci:<name>`              | The pod named `<name>` on embeddedci.com (see below).  |
| *(omitted)*                      | the default saved by `benchpod set-connection`.        |

The global flags are also settable via a `BENCHPOD_*` environment variable
(`BENCHPOD_CONNECTION=usb`, `BENCHPOD_TIMEOUT=2m`, `BENCHPOD_CONFIG_FILE`,
`BENCHPOD_OUTPUT_FILENAME`), with precedence flag > env > default. Command flags
are flags only.

The firmware itself is unauthenticated; `benchpod login` is independent of the
device path and authenticates with `embeddedci-server` (device-login flow) for
cloud features. Direct firmware commands do not send tokens.

Over the USB console, `flash` (SWD), `status` and `la voltage` work; the other
TCP/JSON commands reject a USB connection with a clear message. The
`set-wifi` / `show-network` / `clear-wifi` and `bootsel` subcommands always use
the USB console regardless of `--connection` (a device path still selects
the port). `lan-policy`, `sig-policy`, `cloud ca` and `cloud proxy` work over
USB as well; `identity`, `identity wipe`, `install-blobs` and `dfu` work only over
USB.

`embeddedci:<name>` (the form the Python SDK and the MCP server take) works where a
command can go through embeddedci.com: `lan-policy`, `sig-policy` and `deregister`,
where it is the same as `--device-name <name>`. The other commands talk to the pod
itself and say so when given a cloud target.

## CLI usage

```sh
# Save a default connection so later commands can omit --connection:
benchpod set-connection 192.168.1.5
benchpod set-connection usb

# Find every pod on this machine and this LAN, and check each one works:
benchpod discover
benchpod discover --save     # save it as the default when exactly one is found

# Reachability / state:
benchpod ping
benchpod status

# Hardware (network):
benchpod la voltage 3.3V                          # the DUT's I/O voltage; LA operations need it (network or USB)
benchpod generate --waveform sine --freq 1kHz --duration 2s   # DAC waveform
benchpod measure --waveform sine --samples 1024   # DAC + ADC loopback
benchpod capture --samples 1024 --output csv      # ADC snapshot
benchpod stream ...
benchpod test ...
benchpod la pullup 3 on                           # LA pin pull-up
benchpod la status
benchpod la step --la 6 --steps 200 --delay 500us --dir-la 7 --direction 1

# Network (always over the USB console):
benchpod set-wifi
benchpod show-network     # the pod's IP (wired lease included), SSID, state, RSSI
benchpod clear-wifi

# Firmware:
benchpod flash ...        # flash a TARGET/DUT wired to the pod (SWD via OpenOCD CMSIS-DAP)
benchpod spi-flash ...    # read/erase/program an SPI NOR flash on four LA pins (network)
benchpod bootsel          # reboot a (retired) RP2350 pod into its UF2 bootloader (USB console only)
benchpod dfu              # reboot an STM32 pod into its USB DFU bootloader (USB console only)
benchpod flash-self                  # fetch latest firmware + flash the POD over USB DFU (STM32)
benchpod flash-self --enter-dfu      # …rebooting a running pod into DFU first
benchpod flash-self ./fw.bin         # …flashing a specific local build instead
benchpod install-blobs               # gateware + ESP32-C3 images the pod's firmware goes with (USB)
benchpod install-blobs --dir stm32h563/build/blobs   # …from a local firmware build

# Cloud auth (optional):
benchpod login [--server-url https://www.embeddedci.com]
benchpod register --connection 192.168.1.5
benchpod deregister ...
benchpod logout

# Pod policies (cloud with --device-name, or USB):
benchpod lan-policy --device-name benchpod-baea06          # show
benchpod lan-policy set locked --device-name benchpod-baea06
benchpod sig-policy --connection usb                       # show
benchpod sig-policy set permissive --connection usb

# Device identity (USB console only):
benchpod identity --connection usb                         # show
benchpod identity wipe --connection usb                    # erase the key, make a new one

# Cloud link behind a company network (USB or LAN):
benchpod cloud ca set acme-root.pem --connection usb
benchpod cloud ca show
benchpod cloud proxy set proxy.corp:3128 --user alice   # prompts for the password
benchpod cloud proxy clear
```

Flags that take a physical quantity accept it with its unit: `--freq 2kHz`,
`--sample-rate 10MHz`, `--duration 2s`, `--delay 50us`, and `benchpod la voltage 3.3V`.
The older flags with the unit in their name (`--sample-rate-mhz`, `--duration-ms`,
`--delay-us`) keep working; pass one or the other, not both.

`deregister` is the inverse of `register`: it detaches the pod from the
logged-in account and clears the pod's cloud configuration so it stops
connecting. The server **keeps** everything recorded for that pod (captures,
waveforms, wiring) and only marks the device disabled — registering the same pod
again for the same account brings the device and its data back. Registering it
for a *different* account gives that account a fresh device and leaves the
previous owner's history untouched, which is how a pod changes hands:

```bash
benchpod deregister --connection <pod-ip>       # identifies the pod by its own key
benchpod deregister --device-name <name>        # …for a pod you can no longer reach
```

Running `register` again on the same pod is always safe: it refreshes the pod's
cloud settings and keeps its device, name and history, even after a deregister.
A pod registered in an organization you are not in cannot be claimed until
someone there deregisters it, on the BenchPod page or with `benchpod deregister
--device-name <name>`. The pod does not need to be online for that. Deleting or
disabling an account deregisters its pods automatically.

`register` names the pod after its mDNS name (`benchpod-a1b2c3`) unless you pass
`--device-name`. Names identify pods, so each is unique within an organization:
if another pod already has it, pick another name. It then waits up to `--wait` (default 30s) for the pod to connect, and if it does
not, prints the pod's last connection error and exits non-zero.

#### Pod policies: LAN and signatures

Two settings the pod keeps in its config flash:

| `lan-policy` | What the LAN TCP port allows                                              |
|--------------|---------------------------------------------------------------------------|
| `open`       | everything (the default)                                                   |
| `locked`     | read and instrument control only; config and firmware need cloud or USB   |
| `off`        | nothing: no LAN listener, no mDNS. The cloud and USB keep working          |

| `sig-policy` | How firmware and blob signatures are treated                              |
|--------------|---------------------------------------------------------------------------|
| `audit`      | every image is accepted; signatures are only reported                     |
| `permissive` | unsigned images are accepted, a bad signature is refused                  |
| `required`   | unsigned images are refused                                               |

`benchpod lan-policy` and `benchpod sig-policy` show the policy; `... set <value>`
changes it. Pick the pod with `--device-name` / `--device-id` (through
embeddedci.com, like `deregister`) or with `--connection usb`. Over the LAN the
commands can only show the policy: the pod refuses changes from the LAN.

```
$ benchpod lan-policy --device-name benchpod-baea06
LAN policy of benchpod-baea06: locked (read and instrument control only on the LAN)
```

- Over the cloud a change needs an organization owner or admin (an API key needs
  the `benchpod:admin` scope).
- The cloud can only make `sig-policy` stricter. Only the USB console can loosen
  it (`benchpod sig-policy set permissive --connection usb`); the CLI says so when
  the pod refuses.
- With `lan-policy off`, LAN tools stop working until it is set back: SCPI/VISA,
  hwe2e `TestHW` and LAN `discover`. Set it back over the cloud or USB.
- Firmware without these commands gets "this firmware has no LAN policy; update
  the pod's firmware".

#### Device identity: show and wipe

The pod's identity is an Ed25519 key in its own flash sector; its name
`benchpod-a1b2c3` is the first three bytes of the public key, and embeddedci.com
knows the pod by that key. The firmware makes the key once, on a blank sector,
and never writes over a record it does not recognize (an older or newer layout,
or damage): such a pod stays offline and says why, rather than lose the key it
is registered with.

```
$ benchpod identity --connection usb
Identity of the pod on /dev/cu.usbmodem1101: none: unknown identity record (magic 0xc0ffee02 v2), not replaced
The pod stays offline without an identity. Recover it with: benchpod identity wipe --connection usb
```

`benchpod identity wipe` erases the key and makes a new one. It works only over
the pod's USB console (`--connection usb` or a serial device path): the pod
refuses it over the LAN and the cloud, so it needs someone at the pod. The CLI
shows the current identity and asks before it wipes (`--yes` skips the question
for scripts); the pod also checks a confirmation token (the current short id, or
`unknown`), so a stray command cannot wipe a key.

After a wipe the pod is a new pod to embeddedci.com, and its cloud login is
refused until it is registered again:

1. `benchpod deregister --device-name <old name>`: the old record keeps its
   captures, waveforms and wiring, and the name is freed. Works while the pod is
   offline; an organization owner or admin can also do it on the BenchPod page.
2. Power-cycle the pod, so it advertises its new name on the LAN.
3. `benchpod register --connection <pod address> --device-name <old name>`.
4. Set the wiring profile again on the BenchPod page.

No server-side login reset is needed: the host-bound login state (`ws_auth.v2`)
belongs to the old record.

#### Cloud link: company CA and HTTP proxy

On a network whose proxy inspects TLS, the pod's cloud link fails: the proxy
re-signs embeddedci.com with a company root the pod does not trust. Some networks
also only reach the internet through an HTTP proxy. `benchpod cloud` sets both
over the USB console (`--connection usb`). The LAN (`--connection <address>`) can
show them; current firmware refuses changes from the LAN:

```
$ benchpod cloud ca set acme-root.pem --connection usb
acme-root.pem: 1 certificate
  CN=Acme Root CA,O=Acme Corp
    SHA-256 3b1f...c2d9
Uploading 652 bytes to the pod on /dev/cu.usbmodem1101 ...
Company CA of the pod on /dev/cu.usbmodem1101 is now 1 certificate
  CN=Acme Root CA,O=Acme Corp
    SHA-256 3b1f...c2d9
The pod reconnects to the cloud, trusting this CA in addition to its built-in roots.

$ benchpod cloud proxy set proxy.corp:3128 --user alice --connection usb
Proxy password:
HTTP proxy of the pod on /dev/cu.usbmodem1101 is now proxy.corp:3128 (with a user and password)
The pod reconnects to the cloud through proxy.corp:3128.
```

- `cloud ca set` takes a PEM file with one or more certificates (at most 16 KB
  as PEM). It checks them first, prints each subject and SHA-256, warns about a
  certificate that is not marked as a CA, refuses anything that is not a
  certificate (a private key, for example), and uploads only the certificate
  blocks. The CA is used in addition to the built-in roots; host name and chain
  checks stay on. `cloud ca clear` removes it.
- `cloud proxy set <host:port>` with `--user` takes the password from
  `--password`, `--password-stdin` or a prompt. The pod never reports the
  password back and the CLI never prints it. `cloud proxy clear` removes the proxy.
  With a LAN address as the target, the CLI first warns that the LAN API is plain
  TCP and the password would cross the network unencrypted.
- `show` (the default) reports what the pod holds. The pod reconnects to the
  cloud after every change.
- Current firmware refuses `set` and `clear` from the LAN, whatever its LAN
  policy: only USB or the cloud link may change the company CA or the proxy. The
  CLI then prints the pod's reason and a hint to rerun with `--connection usb`:

  ```
  $ benchpod cloud proxy clear --connection 192.168.1.50
  [benchpod] cloud proxy clear: the pod refused: cloud_proxy: change it from the cloud or the USB console (the pod only takes this change over USB or from the cloud, never from the LAN; run it over the pod's USB console with --connection usb)
  ```

  Older firmware with its LAN policy `locked` refuses the same way, with its own
  message.
- Firmware without them gets "this firmware has no company CA support; update the
  pod's firmware" (or the same for the HTTP proxy).

Run `benchpod <command> -h` for the flags of any subcommand.

#### Flashing: CMSIS-DAP

`flash` always drives **host-side OpenOCD** — the pod holds no flash
intelligence and OpenOCD's exit code is the verdict. The pod runs a CMSIS-DAP
processor locally and OpenOCD's `cmsis-dap` **TCP backend** ships whole DAP
transfers (`DAP_Transfer`/`DAP_TransferBlock`), so a flash is one round-trip per
DAP command instead of thousands of per-bit round-trips — fast even over the
network. Works on **TCP** (`dap_start`) and **USB** (the console `dap-start`
command). It needs a **recent OpenOCD with the `cmsis_dap_tcp` backend**
(post-0.12.0; e.g. `brew install --HEAD open-ocd`); the CLI checks for the
backend up front and fails with clear advice if it is missing.

The legacy per-bit `remote_bitbang` path has been retired — the pod no longer
speaks it.

#### SPI flash

`spi-flash` reads, erases and programs a 25-series SPI NOR flash (W25Q, MX25,
GD25, IS25, ...) wired to four LA pins, through the pod's SPI master. It needs
gateware v45 or newer (`spi_master` in the `status` caps; the CLI checks first)
and the LA voltage set. Network (TCP) connection only for now.

```bash
PINS="--sck 3 --mosi 4 --miso 5 --cs 6"
benchpod spi-flash id $PINS                          # JEDEC ID, size, status register
benchpod spi-flash write fw.bin $PINS --nreset       # erase, program, verify
benchpod spi-flash write app.bin $PINS --addr 0x100000 --hz 4000000
benchpod spi-flash read dump.bin $PINS               # whole part (or --addr/--len, e.g. --len 1M)
benchpod spi-flash erase $PINS --addr 0 --len 64K    # or --chip
```

`write` erases the range the file covers (whole 4 KB sectors, 1 MB per command),
programs it in 768-byte chunks, and has the pod read every chunk back
(`--no-erase`, `--no-verify` skip those steps). `--hz` picks the SCK rate (the
pod rounds down, 190 kHz to 6 MHz) and `--mode` is 0 or 3. 3-byte addresses
reach the first 16 MB.

With `--nreset` the DUT is held in reset (its NRST wired to the pod's reset
pin) for the whole run, so its own controller stays off the SPI bus. At the end
the CLI always releases the SPI pins and then the reset, also after an error or
Ctrl-C. The pod does not clear block-protect bits; a part that ships protected
fails the write with "write enable did not stick".

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

## Command reference

Every command and flag. "Network" means the pod's TCP/JSON API (`--connection
<address>`), "USB" its USB console (`--connection usb` or a device path). Run
`benchpod <command> -h` for the same list in the terminal. `benchpod completion
bash|zsh|fish|powershell` prints a shell completion script, and `benchpod --version`
the CLI version.

### `benchpod` (global flags)

These apply to every command. The CLI keeps its files in
`~/.config/benchpod-cli/` (or `$XDG_CONFIG_HOME/benchpod-cli/`): `config.json`
for the saved connection and `token.json` for the embeddedci.com session.

| Flag                | Default                          | Purpose                                                           |
|---------------------|----------------------------------|-------------------------------------------------------------------|
| `--connection`      | (saved `set-connection` target)  | Address, device path, `usb`, or `embeddedci:<name>` where a command supports it; see [Connection](#connection). |
| `--config-file`     | `~/.config/benchpod-cli/config.json` | The CLI's config file, where `set-connection` saves the default. |
| `--output-filename` | (stdout)                         | Write command output to this file instead of stdout.              |
| `--timeout`         | `0` (per-command default)        | Overall command deadline; `0` uses each command's own default.    |

### Exit codes

Scripts can tell why a command failed from its exit code. The error is printed on
stderr with a `[benchpod]` prefix.

| Code | Meaning                                                                                   |
|------|-------------------------------------------------------------------------------------------|
| `0`  | Success.                                                                                  |
| `1`  | Any other error.                                                                          |
| `2`  | Usage: a bad flag, argument or subcommand.                                                |
| `3`  | Refused: the pod refused the command (a locked LAN, a missing role, a bad request).       |
| `4`  | Busy: the pod or one of its engines is in use (a cloud job's lease, another session).     |
| `5`  | Unreachable: nothing answered on the pod's address, or no pod was found on USB.           |

A refusal is printed word for word as the pod sent it. For a `locked:` (LAN policy),
`busy:` (cloud lease) or `forbidden:` (missing role) refusal the CLI adds what to do next.

### `benchpod set-connection <addr|device|usb>`

Save the default connection (an address, a device path or `usb`) in the config
file, so later commands can leave out `--connection`. No flags.

### `benchpod discover`

Find every pod on USB and on the LAN (mDNS), check each one answers its API, and
report whether it is registered.

| Flag              | Default | Purpose                                                         |
|-------------------|---------|-----------------------------------------------------------------|
| `--save`          | off     | Save the pod as the default connection when exactly one is found (the network address first). |
| `--no-usb`        | off     | Skip probing USB ports.                                          |
| `--no-network`    | off     | Skip mDNS browsing of the LAN.                                   |
| `--wait`          | `3s`    | How long to browse the LAN for mDNS replies.                     |
| `--probe-timeout` | `2s`    | How long each USB port gets to answer `status`.                  |

### `benchpod ping`

Connectivity check over the network. No flags.

### `benchpod status`

Firmware, gateware, capabilities and network details: JSON over the network, the
console's `status` text over USB. No flags.

### `benchpod generate`

Start DAC waveform output (network).

| Flag                | Default | Purpose                                                   |
|---------------------|---------|-----------------------------------------------------------|
| `--waveform`        | (required) | `sine`, `square` or `sawtooth`.                        |
| `--freq`            | `1kHz`  | Output frequency with its unit, e.g. `2kHz`; a bare number is Hz. |
| `--amplitude`       | `127`   | Half-scale amplitude code, 0-127.                         |
| `--offset`          | `128`   | DC offset code, 0-255.                                    |
| `--duration`        | `0`     | How long to play, e.g. `2s` or `500ms`; `0` runs until the next command. |
| `--duration-ms`     | `0`     | The same in ms (older form of `--duration`).              |
| `--sample-rate`     | auto    | FPGA sample-clock rate, e.g. `10MHz`; leave it out to auto-pick. |
| `--sample-rate-mhz` | auto    | The same in MHz (older form of `--sample-rate`).          |

### `benchpod capture`

Blocking ADC snapshot (network). Prints the samples.

| Flag                | Default | Purpose                                              |
|---------------------|---------|------------------------------------------------------|
| `--samples`         | `256`   | Number of ADC samples, 1-4096.                       |
| `--output`          | `json`  | Output format: `json`, `csv` or `ndjson`.            |
| `--sample-rate`     | max     | ADC sample rate, e.g. `100kHz`; leave it out for the maximum (400 kSPS). |
| `--sample-rate-mhz` | max     | The same in MHz (older form of `--sample-rate`).     |

### `benchpod stream`

Asynchronous ADC capture (network). Same flags and output as `capture`.

| Flag                | Default | Purpose                                              |
|---------------------|---------|------------------------------------------------------|
| `--samples`         | `256`   | Number of ADC samples, 1-4096.                       |
| `--output`          | `json`  | Output format: `json`, `csv` or `ndjson`.            |
| `--sample-rate`     | max     | ADC sample rate, e.g. `100kHz`; leave it out for the maximum (400 kSPS). |
| `--sample-rate-mhz` | max     | The same in MHz (older form of `--sample-rate`).     |

### `benchpod measure`

DAC plus ADC loopback capture (network): play a waveform and capture the ADC.

| Flag                | Default | Purpose                                              |
|---------------------|---------|------------------------------------------------------|
| `--waveform`        | (required) | `sine`, `square` or `sawtooth`.                   |
| `--freq`            | `1kHz`  | Output frequency with its unit, e.g. `2kHz`; a bare number is Hz. |
| `--amplitude`       | `127`   | Half-scale amplitude code, 0-127.                    |
| `--offset`          | `128`   | DC offset code, 0-255.                               |
| `--samples`         | `256`   | Number of ADC samples, 1-4096.                       |
| `--output`          | `json`  | Output format: `json`, `csv` or `ndjson`.            |
| `--sample-rate`     | auto    | Sample-clock rate, e.g. `10MHz`; leave it out to auto-pick. |
| `--sample-rate-mhz` | auto    | The same in MHz (older form of `--sample-rate`).     |

### `benchpod test`

A diagnostic sample pattern made by the pod's MCU, without the FPGA (network).

| Flag        | Default | Purpose                                                                  |
|-------------|---------|--------------------------------------------------------------------------|
| `--pattern` | `sine`  | `sine`, `counter`, `ramp` or `const` (`const` when only `--value` is set). |
| `--value`   | `255`   | Constant byte value, 0-255, for the `const` pattern.                      |
| `--samples` | `256`   | Number of samples, 1-4096.                                               |
| `--output`  | `json`  | Output format: `json`, `csv` or `ndjson`.                                |

### `benchpod la`

Logic-analyzer pin control: the bank voltage, pull-ups and step pulses. Pins are
`1`-`14` or `la1`-`la14`. Everything but `la voltage` needs the network.

### `benchpod la voltage [VOLTAGE]`

Show or set the LA bank's I/O voltage, which must match the DUT's: `1.8V` or `3.3V`
(also `1800mV`, `3.3` or `3300`). Leave it out to show the setting. The pod refuses
LA capture, UART, SWD flash, pull-ups and I2C-sensor emulation until it is set;
1.8 V needs a v3 pod, and the pod refuses a change while an LA pin is in use. Works
over the network and over USB. No flags.

### `benchpod la pullup PIN STATE`

Switch an LA pin's pull-up: `PIN` is 1-8 (or `la1`), `STATE` is `on` or `off`. No flags.

### `benchpod la status [PIN]`

Report the pull-up state of one pin, or the bitmask of all of them. No flags.

### `benchpod la step`

Pulse an LA pin N times, for step/dir stepper drivers. The FPGA times the train.

| Flag          | Default | Purpose                                                      |
|---------------|---------|--------------------------------------------------------------|
| `--la`        | (required) | LA pin to pulse, e.g. `6` or `la6`.                       |
| `--steps`     | (required) | Number of pulses, positive.                               |
| `--delay`     | (required) | High and low half-period, e.g. `50us` or `1ms` (this or `--delay-us`). |
| `--delay-us`  | (required) | The same in microseconds (older form of `--delay`).       |
| `--dir-la`    | (none)  | Direction pin, driven before stepping.                       |
| `--direction` | `0`     | Level for `--dir-la`: `0` or `1`.                            |

### `benchpod flash`

Flash an SWD target wired to the pod, through host-side OpenOCD and the pod's
CMSIS-DAP backend (network or USB). See [Flashing: CMSIS-DAP](#flashing-cmsis-dap).

| Flag                       | Default | Purpose                                                                 |
|----------------------------|---------|-------------------------------------------------------------------------|
| `--swclk`                  | (required) | LA pin for SWCLK, 1-14 (`1` or `la1`).                               |
| `--swdio`                  | (required) | LA pin for SWDIO, 1-14.                                              |
| `--target`                 | (none)  | OpenOCD target config, passed as `-f`, e.g. `target/stm32f1x.cfg`.      |
| `--file`                   | (none)  | Firmware image to flash, used with `--target`.                          |
| `--load-address`           | (none)  | Load address for a raw `.bin` image.                                    |
| `--nreset`                 | off     | The target's NRST is wired to the pod's reset pin (DUT header J1 pin 22); turns on connect-under-reset. |
| `--no-connect-under-reset` | off     | Do not hold the target in reset while connecting.                       |
| `--no-verify`              | off     | Do not verify after programming.                                        |
| `--no-reset`               | off     | Do not reset the target after programming.                              |
| `--keep-reset-init`        | off     | Keep the target config's reset-init clock boost (cleared by default because it glitches the link). |
| `--target-power`           | (none)  | Turn on a target power eFuse first: `1` (internal 5 V) or `2` (external). |
| `--openocd`                | (`PATH`) | Path to the `openocd` binary.                                          |
| `--command`, `-c`          | (none)  | Extra OpenOCD command, repeatable, appended last.                       |
| `--openocd-arg`            | (none)  | Extra raw OpenOCD argument, repeatable, appended last.                  |

Pass `--target` (with `--file`) or at least one `-c`/`--openocd-arg`.

### `benchpod spi-flash`

Read, erase and program a 25-series SPI NOR flash on four LA pins (network). See
[SPI flash](#spi-flash). These flags apply to every subcommand.

| Flag       | Default   | Purpose                                                              |
|------------|-----------|----------------------------------------------------------------------|
| `--sck`    | (required) | LA pin for SCK, 1-14.                                               |
| `--mosi`   | (required) | LA pin for MOSI, 1-14.                                              |
| `--miso`   | (required) | LA pin for MISO, 1-14.                                              |
| `--cs`     | (required) | LA pin for CS, 1-14.                                                |
| `--hz`     | `1000000` | SCK rate in Hz; the pod uses the nearest rate at or below it (190 kHz to 6 MHz). |
| `--mode`   | `0`       | SPI mode, `0` or `3`.                                                |
| `--nreset` | off       | Hold the DUT in reset through the pod's reset pin for the whole run.  |

### `benchpod spi-flash id`

Read the JEDEC ID, size and status register. No flags of its own.

### `benchpod spi-flash read OUT`

Read flash contents into a file (`-` for stdout).

| Flag     | Default | Purpose                                                      |
|----------|---------|--------------------------------------------------------------|
| `--addr` | `0`     | Flash address to read from.                                  |
| `--len`  | (to the end) | Bytes to read, e.g. `4096` or `1M`.                     |

### `benchpod spi-flash write FILE`

Erase, program and verify a raw image.

| Flag          | Default | Purpose                                                   |
|---------------|---------|-----------------------------------------------------------|
| `--addr`      | `0`     | Flash address to write at, e.g. `0x10000`.                |
| `--no-erase`  | off     | Do not erase first (the range must already be erased).    |
| `--no-verify` | off     | Do not read each chunk back.                              |

### `benchpod spi-flash erase`

Erase a range or the whole part.

| Flag     | Default | Purpose                                  |
|----------|---------|------------------------------------------|
| `--addr` | `0`     | First address to erase.                  |
| `--len`  | (none)  | Bytes to erase, e.g. `65536` or `1M`.    |
| `--chip` | off     | Erase the whole part instead of a range. |

### `benchpod flash-self [firmware.bin | URL]`

Flash the pod's own firmware over USB DFU (STM32) with dfu-util, then install its
blobs. With no argument it fetches the latest release. See
[Flashing the pod itself](#flashing-the-pod-itself-stm32-usb-dfu).

| Flag                 | Default      | Purpose                                                       |
|----------------------|--------------|---------------------------------------------------------------|
| `--enter-dfu`        | off          | First reboot a running pod into DFU over its USB console.     |
| `--wait`             | `1m` (60s)   | How long to wait for the pod to show up in DFU mode.          |
| `--firmware-version` | (latest)     | Fetch this release tag instead of the latest.                 |
| `--firmware-url`     | (latest)     | Download the firmware from this URL instead.                  |
| `--address`          | `0x08000000` | Flash base address (STM32 main flash).                        |
| `--dfu-util`         | (`PATH`)     | Path to the `dfu-util` binary.                                |
| `--no-leave`         | off          | Stay in DFU after flashing instead of starting the firmware.  |
| `--skip-blobs`       | off          | Do not install the gateware and ESP32-C3 images afterwards.   |
| `--blobs-dir`        | (next to the firmware) | Take the blobs from this directory.                 |

It prints a `signature:` line for the release's `.sig` (report only, see
[dfu-util](#dfu-util-for-flash-self)) and exits non-zero when the firmware does not
fit the pod's flash, the download's checksum does not match, or dfu-util fails.

### `benchpod install-blobs`

Install the gateware and ESP32-C3 images the pod's firmware goes with (USB). By
default they come from the release that matches the firmware the pod runs. Each
blob's signature is reported; for now a missing or failing one never stops it.

| Flag        | Default         | Purpose                                                        |
|-------------|-----------------|----------------------------------------------------------------|
| `--release` | (the pod's)     | Take the blobs from this firmware release tag.                 |
| `--dir`     | (none)          | Take the blobs from this directory (holding `blobs-manifest.json`), e.g. `stm32h563/build/blobs`. |
| `--only`    | (all)           | Comma-separated slots to install: `gw0`, `gw1`, `esp`.         |
| `--force`   | off             | Install even the blobs the pod already holds.                  |

### `benchpod dfu`

Reboot an STM32 pod into its USB DFU bootloader (USB). Then flash it with
`flash-self` or dfu-util.

| Flag    | Default | Purpose                         |
|---------|---------|---------------------------------|
| `--yes` | off     | Skip the confirmation prompt.   |

### `benchpod bootsel`

Reboot a (retired) RP2350 pod into its UF2 bootloader (USB).

| Flag    | Default | Purpose                         |
|---------|---------|---------------------------------|
| `--yes` | off     | Skip the confirmation prompt.   |

### `benchpod set-wifi`

Save Wi-Fi credentials and join (USB). Installs the ESP32-C3 image first when the
pod lacks it; if the pod's image slots cannot be read, it stops and says so
(`--skip-blobs` sets Wi-Fi without the check).

| Flag               | Default    | Purpose                                                   |
|--------------------|------------|-----------------------------------------------------------|
| `--ssid`           | (required) | Wi-Fi SSID.                                               |
| `--password`       | (prompt)   | Wi-Fi password (visible in shell history; prefer the prompt or `--password-stdin`). |
| `--password-stdin` | off        | Read the password from the first line of stdin.           |
| `--skip-blobs`     | off        | Do not install a missing ESP32-C3 image first.            |

### `benchpod show-network`

The pod's IP (the wired lease first) plus the stored Wi-Fi SSID, state and RSSI
(USB). No flags.

### `benchpod clear-wifi`

Erase the stored Wi-Fi credentials (USB). Wi-Fi goes down at once, no reboot needed. Firmware after 3.7.0 also erases the copy older firmware left on the ESP32-C3 Wi-Fi chip, which takes a few seconds more. No flags.

### `benchpod login`

Authenticate with embeddedci.com (device-login flow). A no-op when a usable
session exists.

| Flag           | Default                      | Purpose                                                  |
|----------------|------------------------------|----------------------------------------------------------|
| `--server-url` | `https://www.embeddedci.com` | embeddedci-server base URL.                              |
| `--token-file` | `~/.config/benchpod-cli/token.json` | Token cache.                                      |
| `--no-browser` | off                          | Print the approval URL instead of opening a browser.     |
| `--force`      | off                          | Authenticate again even when a session exists.           |

### `benchpod logout`

Delete the cached tokens. Local only: no pod is deregistered.

| Flag           | Default                      | Purpose      |
|----------------|------------------------------|--------------|
| `--token-file` | `~/.config/benchpod-cli/token.json` | Token cache. |

### `benchpod register`

Register the pod reached with `--connection` to your account and provision it to
connect to embeddedci.com. Needs `benchpod login` first. Exits non-zero, with the
pod's last connection error, when the pod does not connect within `--wait`.

| Flag                     | Default                      | Purpose                                                    |
|--------------------------|------------------------------|------------------------------------------------------------|
| `--device-name`          | (the pod's own name)         | Device name, URL-safe and unique in the organization, e.g. `bench-01`. |
| `--wait`                 | `30s`                        | How long to wait for the pod to connect; `0` does not wait. |
| `--pod-host`             | `api.embeddedci.com`         | Host the pod connects to (`api.embeddedci.com` for embeddedci.com, else the `--server-url` host). |
| `--insecure-skip-verify` | off                          | Provision the pod to skip TLS certificate checks (bring-up only). |
| `--server-url`           | `https://www.embeddedci.com` | embeddedci-server base URL.                                |
| `--token-file`           | `~/.config/benchpod-cli/token.json` | Token cache.                                        |

### `benchpod deregister`

Detach the pod from your account (its data is kept) and clear its cloud
configuration. By default the pod reached with `--connection` is identified by its
key; `--connection embeddedci:<name>` is the same as `--device-name <name>`.

| Flag                | Default                      | Purpose                                                       |
|---------------------|------------------------------|---------------------------------------------------------------|
| `--device-name`     | (none)                       | Deregister the device with this name instead (the pod may be offline). |
| `--device-id`       | (none)                       | Deregister the device with this id instead.                    |
| `--keep-pod-config` | off                          | Leave the pod's cloud configuration in place.                  |
| `--server-url`      | `https://www.embeddedci.com` | embeddedci-server base URL.                                   |
| `--token-file`      | `~/.config/benchpod-cli/token.json` | Token cache.                                           |

### `benchpod lan-policy`

Show the pod's LAN policy: `open`, `locked` or `off`. See
[Pod policies](#pod-policies-lan-and-signatures). Over the cloud with
`--device-name`/`--device-id` or `--connection embeddedci:<name>`, or over USB; the
LAN can only show it. These flags
apply to `show` and `set` too.

| Flag            | Default                      | Purpose                                                  |
|-----------------|------------------------------|----------------------------------------------------------|
| `--device-name` | (none)                       | Go through embeddedci.com to the registered pod with this name. |
| `--device-id`   | (none)                       | Go through embeddedci.com to the registered pod with this id. |
| `--server-url`  | `https://www.embeddedci.com` | embeddedci-server base URL.                              |
| `--token-file`  | `~/.config/benchpod-cli/token.json` | Token cache.                                      |

### `benchpod lan-policy show`

Same as `benchpod lan-policy`.

### `benchpod lan-policy set <open|locked|off>`

Set the LAN policy (cloud with an owner or admin role, or USB).

### `benchpod sig-policy`

Show the pod's signature policy: `audit`, `permissive` or `required`. Same
transports as `lan-policy`; the cloud can only make it stricter. These flags apply
to `show` and `set` too.

| Flag            | Default                      | Purpose                                                  |
|-----------------|------------------------------|----------------------------------------------------------|
| `--device-name` | (none)                       | Go through embeddedci.com to the registered pod with this name. |
| `--device-id`   | (none)                       | Go through embeddedci.com to the registered pod with this id. |
| `--server-url`  | `https://www.embeddedci.com` | embeddedci-server base URL.                              |
| `--token-file`  | `~/.config/benchpod-cli/token.json` | Token cache.                                      |

### `benchpod sig-policy show`

Same as `benchpod sig-policy`.

### `benchpod sig-policy set <audit|permissive|required>`

Set the signature policy. Loosening it needs the USB console.

### `benchpod identity`

Show the pod's device identity (USB). See
[Device identity](#device-identity-show-and-wipe). No flags.

### `benchpod identity show`

Same as `benchpod identity`.

### `benchpod identity wipe`

Erase the pod's device key and make a new one (USB only). The pod then needs to be
registered again.

| Flag    | Default | Purpose                      |
|---------|---------|------------------------------|
| `--yes` | off     | Do not ask for confirmation. |

### `benchpod cloud`

How the pod reaches embeddedci.com: a company CA and an HTTP proxy. See
[Cloud link](#cloud-link-company-ca-and-http-proxy). Changes need USB (or the
cloud link); the LAN can show them.

### `benchpod cloud ca`

Show the company CA certificates the pod holds (same as `cloud ca show`).

### `benchpod cloud ca show`

Show the company CA certificates the pod holds. No flags.

### `benchpod cloud ca set <file.pem>`

Check a PEM file (at most 16 KB) and store its CA certificates on the pod. No flags.

### `benchpod cloud ca clear`

Remove the company CA; the pod trusts only its built-in roots again. No flags.

### `benchpod cloud proxy`

Show the pod's HTTP proxy (same as `cloud proxy show`).

### `benchpod cloud proxy show`

Show the pod's HTTP proxy. The password is never reported. No flags.

### `benchpod cloud proxy set <host:port>`

Set the pod's HTTP proxy.

| Flag               | Default  | Purpose                                                         |
|--------------------|----------|-----------------------------------------------------------------|
| `--user`           | (none)   | Proxy user name (Basic authentication).                         |
| `--password`       | (prompt) | Proxy password (visible in shell history; prefer the prompt or `--password-stdin`). |
| `--password-stdin` | off      | Read the password from the first line of stdin.                 |

### `benchpod cloud proxy clear`

Remove the HTTP proxy; the pod connects to the cloud directly. No flags.

## Development

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

The tag is the version (GoReleaser sets `main.version` from it). Before anything
is published, the workflow checks that the tag is `vX.Y.Z` on a commit in `main`
and runs the full `ci` workflow (vet, tests, snapshot build).

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
| `internal/tcpclient/`      | TCP/JSON API client (commands, sessions, identity, SWD).                      |
| `internal/serialconsole/`  | USB CDC-ACM serial console client and port auto-detection.                    |
| `internal/openocd/`        | OpenOCD wrapper used by the `flash` command.                                  |
| `internal/spiflash/`       | SPI NOR flash driver over the pod's `spi_*` JSON commands (`spi-flash`).      |
| `internal/benchpodconfig/` | Local config (the saved `set-connection` target).                            |
| `internal/serverapi/`      | HTTP client for `embeddedci-server` (device-login, refresh).                  |
| `internal/authstore/`      | Token cache (`~/.config/benchpod-cli/token.json`).                            |
