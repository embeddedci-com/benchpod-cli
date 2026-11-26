package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// `benchpod usb-flash <file>`: flash a board over the pod's USB host port (firmware 4.0.3+,
// feature usb_flash). The pod's usb_flash command has a mode per bootloader family; the CLI
// chooses it from the file and from the device that enumerates, so nobody picks a mode (--mode
// overrides). The file is staged into the pod's PSRAM (load_bin) like `flash --target esp32`, the
// flash runs in the background and this polls its state.

const (
	usbFlashDefaultTimeout = 5 * time.Minute
	usbHostExtra           = time.Minute // host mode outlives the command by this much
)

// usbFlashModes are the pod's usb_flash modes this CLI drives.
var usbFlashModes = []string{"uf2", "file", "dfu", "teensy", "avr", "samd", "esp", "picoboot"}

// usbFlashPod is the pod as usb-flash uses it: JSON commands and the PSRAM upload.
type usbFlashPod interface {
	podCommander
	StagePSRAM(ctx context.Context, data []byte) (int, error)
}

var _ usbFlashPod = (*tcpclient.Client)(nil)

type usbFlashOpts struct {
	file       string
	mode       string // "" = choose
	address    string // "" = default
	name       string // "" = the file's 8.3 name
	port       string
	noLeave    bool
	verify     bool // uf2: compare the drive's CURRENT.UF2 with the file (read-back)
	checkFail  bool // file: read FAIL.TXT after the copy (read-back)
	asJSON     bool
	hostSecs   int // timeout_s when this starts host mode
	pollPeriod time.Duration
}

func newUSBFlashCmd(g *globalFlags) *cobra.Command {
	var o usbFlashOpts
	cmd := &cobra.Command{
		Use:   "usb-flash <file>",
		Short: "Flash a board over the pod's USB host port (UF2, PICOBOOT, DFU, drag-and-drop drives, Teensy, Arduino, ESP32)",
		Long: "Flash the board on the pod's USB host port from a file (firmware 4.0.3 and later). The CLI chooses\n" +
			"how from the file and the USB device that shows up; --mode overrides it.\n\n" +
			"  .uf2 file                      uf2     UF2 bootloaders (Pico, Adafruit, tinyuf2): written to the drive\n" +
			"  Pico or Pico 2 in BOOTSEL      picoboot  RP2040 and RP2350 (2E8A:0003, 2E8A:000F) with a .bin or .hex file:\n" +
			"                                         erased, written and read back page by page; --address default\n" +
			"                                         0x10000000, a multiple of 4096\n" +
			"  Teensy loader (16C0:0478)      teensy  Teensy 3.x, 4.0, 4.1\n" +
			"  a DFU interface                dfu     USB DFU and STM32 DfuSe (BOOT0 high); --address, default\n" +
			"                                         0x08000000 for a .bin\n" +
			"  Arduino AVR / SAMD bootloader  avr, samd\n" +
			"  Espressif or an ESP image      esp     ESP32 behind a USB-serial chip or its own USB (image starts 0xE9)\n" +
			"  a mass-storage drive           file    DAPLink (NUCLEO, nRF DK, micro:bit): the file is copied as an\n" +
			"                                         8.3 name (--name, default the file name)\n\n" +
			"A .hex or .dfu file is converted to a binary with its address for dfu, teensy, avr, samd, esp and picoboot;\n" +
			"uf2 and file send it as it is. The flash starts the new firmware at the end (--no-leave keeps\n" +
			"the board in its bootloader; with picoboot, in BOOTSEL).\n\n" +
			"Read-back: picoboot always reads every page back and fails on a difference. --verify (uf2) compares\n" +
			"the file with the loader's CURRENT.UF2 when the board is still in its loader; --check-fail-txt (file)\n" +
			"reads FAIL.TXT after the copy, so a DAPLink that rejected the file fails the flash. The pod's note\n" +
			"(\"verified 12 blocks\") is printed. For a file copy the CLI sends the current time for the new file's dates.\n\n" +
			"If USB host mode is off, usb-flash starts it (and says so) on --port: usb_a on a pod with the\n" +
			"USB-A host port (board v3r4 and later), else usb_c. It then waits for a device, shows the\n" +
			"progress and exits non-zero with the pod's reason when the flash fails. Host mode stays on until\n" +
			"its timeout; `benchpod usb-host off` stops it. While the pod hosts it is off the computer's USB,\n" +
			"so use the pod's network address (not --connection usb). The global --timeout is the whole\n" +
			"deadline (waiting for the device and the flash; default 5m).\n\n" +
			"The file goes to the pod's PSRAM first, so this needs a direct link to the pod (its network\n" +
			"address), not embeddedci.com.",
		Example: "  benchpod usb-flash pico-app.uf2\n" +
			"  benchpod usb-flash firmware.bin --address 0x08000000\n" +
			"  benchpod usb-flash app.hex --port usb_c --timeout 3m\n" +
			"  benchpod usb-flash firmware.bin --mode file --name APP.BIN --check-fail-txt\n" +
			"  benchpod usb-flash app.bin --mode picoboot --address 0x10000000\n" +
			"  benchpod usb-flash pico-app.uf2 --verify",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.file = args[0]
			if err := validateUSBFlashFlags(&o); err != nil {
				return err
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return err
			}
			defer closeOut()
			data, err := os.ReadFile(o.file)
			if err != nil {
				return fmt.Errorf("usb-flash: %w", err)
			}
			spec, err := g.resolveTarget()
			if err != nil {
				return err
			}
			if spec.IsCloud() {
				return fmt.Errorf("usb-flash: the file is uploaded into the pod first, so it needs a direct link to the pod (its network address); over embeddedci.com use the Python SDK or the MCP server")
			}
			if !spec.IsNetwork() {
				spec, err = g.networkTarget("usb-flash")
				if err != nil {
					return err
				}
			}
			total := g.effectiveTimeout(usbFlashDefaultTimeout)
			o.hostSecs = int((total + usbHostExtra) / time.Second)
			if o.hostSecs > 3600 {
				o.hostSecs = 3600
			}
			ctx, cancel := context.WithTimeout(context.Background(), total)
			defer cancel()
			defer installSignalHandler(ctx, cancel)()
			c := &tcpclient.Client{Addr: spec.Addr}
			return runUSBFlash(ctx, c, o, data, out, os.Stderr)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.mode, "mode", "", "override the automatic choice: uf2, file, dfu, teensy, avr, samd, esp or picoboot")
	f.StringVar(&o.address, "address", "", "flash address of the image for dfu, teensy, avr, samd, esp and picoboot (default: from a .hex or .dfu file, else 0x08000000 for dfu, 0x2000 for samd, 0x10000000 for picoboot, 0)")
	f.StringVar(&o.name, "name", "", "file mode: the file name on the drive, 8.3 (default: the file's name, shortened)")
	f.StringVar(&o.port, "port", "", "usb_a or usb_c when host mode has to be started (default: usb_a on a pod with the USB-A host port, board v3r4 or later; else usb_c)")
	f.BoolVar(&o.noLeave, "no-leave", false, "keep the board in its bootloader at the end instead of starting the new firmware")
	f.BoolVar(&o.verify, "verify", false, "uf2: after the write, compare the file with the loader's CURRENT.UF2 (only when the board is still in its loader)")
	f.BoolVar(&o.checkFail, "check-fail-txt", false, "file: after the copy, wait for the drive to come back and fail the flash when it holds a FAIL.TXT")
	f.BoolVar(&o.asJSON, "json", false, "print the pod's final flash report as JSON")
	return cmd
}

func validateUSBFlashFlags(o *usbFlashOpts) error {
	o.mode = strings.ToLower(strings.TrimSpace(o.mode))
	if o.mode != "" {
		ok := false
		for _, m := range usbFlashModes {
			ok = ok || m == o.mode
		}
		if !ok {
			return usageErrorf("usb-flash: --mode must be one of %s (got %q)", strings.Join(usbFlashModes, ", "), o.mode)
		}
	}
	if o.address != "" {
		if _, err := parseUSBAddress(o.address); err != nil {
			return usageErrorf("usb-flash: --address: %v", err)
		}
	}
	if o.name != "" {
		o.name = strings.ToUpper(strings.TrimSpace(o.name))
		if !validFAT83(o.name) {
			return usageErrorf("usb-flash: --name must be an 8.3 file name such as FIRMWARE.BIN (got %q)", o.name)
		}
	}
	if o.verify && o.mode != "" && o.mode != "uf2" {
		return usageErrorf("usb-flash: --verify is for uf2 mode (picoboot always reads back)")
	}
	if o.checkFail && o.mode != "" && o.mode != "file" {
		return usageErrorf("usb-flash: --check-fail-txt is for file mode")
	}
	if p := strings.ToLower(strings.TrimSpace(o.port)); p != "" && p != "usb_a" && p != "usb_c" {
		return usageErrorf("usb-flash: --port must be usb_a or usb_c (got %q)", o.port)
	}
	return nil
}

// ── what to flash with ──────────────────────────────────────────────────────

// usbFlashPlan is how a file goes to the device.
type usbFlashPlan struct {
	Mode    string
	Why     string // "UF2 file", "Teensy loader", ...
	Address uint32
	HasAddr bool // address is sent (dfu, teensy, avr, samd, esp)
	Name    string
	Data    []byte
	Note    string // shown after the plan: an assumption the user may want to change
}

// Arduino AVR bootloader USB IDs (Caterina, the Uno and Mega 2560 USB firmware, SparkFun and
// Adafruit 32u4 boards). Other AVR boards: --mode avr.
var usbAVRIDs = map[[2]int]bool{
	{0x2341, 0x0036}: true, {0x2341, 0x8036}: true, {0x2341, 0x0037}: true, {0x2341, 0x8037}: true, // Leonardo, Micro
	{0x2341, 0x0001}: true, {0x2341, 0x0043}: true, {0x2341, 0x0010}: true, {0x2341, 0x0042}: true, // Uno, Mega 2560
	{0x2A03, 0x0036}: true, {0x2A03, 0x0043}: true, {0x2A03, 0x0010}: true, {0x2A03, 0x0042}: true,
	{0x1B4F, 0x9203}: true, {0x1B4F, 0x9204}: true, {0x1B4F, 0x9205}: true, {0x1B4F, 0x9206}: true, // Pro Micro
	{0x239A, 0x000C}: true, {0x239A, 0x800C}: true, // Feather 32u4
}

// Arduino and Adafruit SAMD bootloader USB IDs (the sketch IDs set bit 15).
var usbSAMDIDs = map[[2]int]bool{
	{0x2341, 0x004D}: true, {0x2341, 0x004E}: true, {0x2341, 0x0054}: true, {0x2341, 0x0057}: true, // Zero, MKR1000, MKR WiFi 1010, Nano 33 IoT
	{0x2341, 0x0050}: true, {0x2341, 0x0052}: true, {0x2341, 0x0053}: true, {0x2341, 0x0055}: true,
	{0x239A, 0x000B}: true, {0x239A, 0x0015}: true, // Feather M0, Metro M0
}

func idIn(set map[[2]int]bool, d *usbDevice) bool {
	return set[[2]int{d.VID, d.PID}] || set[[2]int{d.VID, d.PID &^ 0x8000}]
}

// usbSerialChipVIDs are the USB-serial chips the pod's esp mode drives: FTDI, CP210x, CH34x.
var usbSerialChipVIDs = map[int]bool{0x0403: true, 0x10C4: true, 0x1A86: true}

const (
	usbClassMSC     = 0x08
	usbClassApp     = 0xFE // application specific: DFU is subclass 1
	espVID          = 0x303A
	teensyVID       = 0x16C0
	teensyLoaderPID = 0x0478
	stmDfuVID       = 0x0483
	stmDfuPID       = 0xDF11
	piVID           = 0x2E8A // Raspberry Pi
	piBootRP2040    = 0x0003
	piBootRP2350    = 0x000F
)

func looksLikeESPImage(data []byte) bool { return len(data) > 0 && data[0] == 0xE9 }

// chooseUSBFlashMode picks the mode from the file and the device. The order matters: a .uf2 file
// is uf2; the Teensy loader and a DFU interface are unambiguous; the Arduino IDs; an ESP chip; a
// mass-storage drive is the fallback for a file the drive takes.
func chooseUSBFlashMode(file string, data []byte, d *usbDevice) (mode, why string, err error) {
	if strings.EqualFold(filepath.Ext(file), ".uf2") {
		return "uf2", "a .uf2 file", nil
	}
	if d == nil {
		return "", "", fmt.Errorf("no USB device to choose a mode from")
	}
	switch {
	case d.VID == teensyVID && d.PID == teensyLoaderPID:
		return "teensy", "the Teensy loader", nil
	case d.hasInterface(usbClassApp, 0x01) || (d.VID == stmDfuVID && d.PID == stmDfuPID):
		return "dfu", "a DFU interface", nil
	case idIn(usbAVRIDs, d):
		return "avr", "an Arduino AVR bootloader", nil
	case idIn(usbSAMDIDs, d):
		return "samd", "an Arduino SAMD bootloader", nil
	case d.VID == espVID:
		return "esp", "an Espressif chip", nil
	case usbSerialChipVIDs[d.VID] && looksLikeESPImage(data):
		return "esp", "an ESP image behind a USB-serial chip", nil
	case d.VID == piVID && (d.PID == piBootRP2040 || d.PID == piBootRP2350):
		return "picoboot", "a Pico in BOOTSEL", nil
	case d.hasInterface(usbClassMSC, -1):
		return "file", "a mass-storage drive", nil
	}
	hint := ""
	if usbSerialChipVIDs[d.VID] {
		hint = "; a USB-serial chip is flashed as an ESP32 only for an ESP image (it starts with 0xE9): for an AVR or SAMD board pass --mode avr or --mode samd"
	}
	return "", "", fmt.Errorf("cannot tell how to flash %s %s (%s)%s: pass --mode (%s)",
		d.ids(), usbNames(d), usbInterfaceSummary(d), hint, strings.Join(usbFlashModes, ", "))
}

func usbInterfaceSummary(d *usbDevice) string {
	if d.Config == nil || len(d.Config.Interfaces) == 0 {
		return "no interfaces"
	}
	var names []string
	for _, i := range d.Config.Interfaces {
		n := i.ClassName
		if n == "" {
			n = fmt.Sprintf("class %02x", i.Class)
		}
		names = append(names, n)
	}
	return "interfaces: " + strings.Join(names, ", ")
}

// planUSBFlash decides the mode and prepares the payload and address for it.
func planUSBFlash(o usbFlashOpts, data []byte, d *usbDevice) (*usbFlashPlan, error) {
	mode, why := o.mode, "--mode"
	if mode == "" {
		var err error
		if mode, why, err = chooseUSBFlashMode(o.file, data, d); err != nil {
			return nil, err
		}
	}
	p := &usbFlashPlan{Mode: mode, Why: why, Data: data}
	ext := strings.ToLower(filepath.Ext(o.file))
	var fileAddr uint32
	hasFileAddr := false
	switch mode {
	case "uf2":
		return p, nil
	case "file":
		p.Name = o.name
		if p.Name == "" {
			p.Name = fat83Name(o.file)
		}
		return p, nil
	}
	// The other modes take a binary and an address.
	switch ext {
	case ".hex", ".ihex":
		bin, addr, err := parseIntelHex(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", o.file, err)
		}
		p.Data, fileAddr, hasFileAddr = bin, addr, true
		if mode == "teensy" && fileAddr >= 0x60000000 {
			fileAddr -= 0x60000000 // Teensy 4 flash is mapped there; the loader takes the offset
		}
	case ".dfu":
		bin, addr, has, err := parseDFUFile(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", o.file, err)
		}
		p.Data, fileAddr, hasFileAddr = bin, addr, has
	}
	p.HasAddr = true
	switch {
	case o.address != "":
		a, _ := parseUSBAddress(o.address)
		p.Address = a
	case hasFileAddr:
		p.Address = fileAddr
	default:
		p.Address, p.Note = defaultUSBAddress(mode, d)
	}
	if mode == "dfu" && o.address == "" && !hasFileAddr && d != nil && !isDfuSe(d) {
		p.HasAddr = false // a plain DFU loader has no addresses
		p.Note = ""
	}
	if mode == "picoboot" && p.Address%4096 != 0 {
		return nil, fmt.Errorf("picoboot: the address 0x%08x is not a multiple of 4096 (the flash sector size)", p.Address)
	}
	return p, nil
}

// isDfuSe says the device is an STM32 DfuSe loader: ROM ID, or a DFU interface with protocol 2.
func isDfuSe(d *usbDevice) bool {
	if d.VID == stmDfuVID && d.PID == stmDfuPID {
		return true
	}
	if d.Config != nil {
		for _, i := range d.Config.Interfaces {
			if i.Class == usbClassApp && i.Subclass == 0x01 && i.Protocol == 0x02 {
				return true
			}
		}
	}
	return false
}

func defaultUSBAddress(mode string, d *usbDevice) (uint32, string) {
	switch mode {
	case "dfu":
		return 0x08000000, ""
	case "samd":
		return 0x2000, "writing at 0x2000 (a SAMD21 sketch); a SAMD51 starts at 0x4000: pass --address 0x4000"
	case "picoboot":
		return 0x10000000, ""
	case "esp":
		return 0, "writing at flash offset 0 (a merged image); an application image goes at --address 0x10000"
	}
	return 0, ""
}

// usbFlashNow is the time sent as the new file's dates (tests replace it).
var usbFlashNow = time.Now

// ── the run ─────────────────────────────────────────────────────────────────

// usbFlashStatus is the usb_flash state reply.
type usbFlashStatus struct {
	Running    bool   `json:"running"`
	HaveResult bool   `json:"have_result"`
	Mode       string `json:"mode"`
	Name       string `json:"name"`
	Len        int    `json:"len"`
	Address    int64  `json:"address"`
	Protocol   string `json:"protocol"`
	Phase      string `json:"phase"`
	Done       int    `json:"done"`
	Total      int    `json:"total"`
	Result     string `json:"result"`
	OK         bool   `json:"ok"`
	MS         int    `json:"ms"`
	Note       string `json:"note"`
}

func usbFlashErr(what string, err error) error {
	return usbHostErr(what, err)
}

// runUSBFlash is the whole command against a pod: features, host mode, device, plan, stage, start,
// poll. Progress goes to progress, the result to out.
func runUSBFlash(ctx context.Context, pod usbFlashPod, o usbFlashOpts, data []byte, out, progress io.Writer) error {
	const what = "usb-flash"
	poll := o.pollPeriod
	if poll <= 0 {
		poll = usbHostPoll
	}
	say := func(format string, a ...any) { fmt.Fprintf(progress, "usb-flash: "+format+"\n", a...) }
	need := []string{"usb_host_mode", "usb_flash"}
	if o.mode == "picoboot" {
		need = append(need, "usb_flash_picoboot")
	}
	if o.verify || o.checkFail {
		need = append(need, "usb_flash_readback")
	}
	info, err := requireUSBFeatures(ctx, pod, what, need...)
	if err != nil {
		return err
	}

	// Host mode.
	host, err := usbEnsureHost(ctx, pod, what, info, o.port, o.hostSecs, int(usbFlashDefaultTimeout/time.Second)+60, poll, say)
	if err != nil {
		return err
	}

	// The device.
	dev, err := usbWaitDevice(ctx, pod, host, poll, say)
	if err != nil {
		return usbFlashErr(what, err)
	}

	plan, err := planUSBFlash(o, data, dev)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if plan.Mode == "picoboot" && o.mode != "picoboot" {
		if _, err := requireUSBFeatures(ctx, pod, what, "usb_flash_picoboot"); err != nil {
			return err
		}
	}
	if (o.verify && plan.Mode != "uf2") || (o.checkFail && plan.Mode != "file") {
		return usageErrorf("usb-flash: --verify is for uf2 mode and --check-fail-txt for file mode; this flash uses %s mode", plan.Mode)
	}
	say("%s %s: %s mode (%s), %d bytes%s", dev.ids(), usbNames(dev), plan.Mode, plan.Why, len(plan.Data), planAddr(plan))
	if plan.Note != "" {
		say("%s", plan.Note)
	}

	// Stage and start.
	t0 := time.Now()
	if _, err := pod.StagePSRAM(ctx, plan.Data); err != nil {
		return usbFlashErr(what, fmt.Errorf("uploading %s: %w", filepath.Base(o.file), err))
	}
	req := map[string]any{"cmd": "usb_flash", "mode": plan.Mode, "len": len(plan.Data)}
	if plan.HasAddr {
		req["address"] = fmt.Sprintf("0x%08x", plan.Address)
	}
	if plan.Mode == "file" {
		req["name"] = plan.Name
	}
	if o.noLeave && plan.Mode != "uf2" && plan.Mode != "file" {
		req["leave"] = false
	}
	if plan.Mode == "file" {
		// The pod has no clock: the file's dates come from this computer.
		req["time"] = usbFlashNow().Unix()
		if o.checkFail {
			req["check_fail_txt"] = true
		}
	}
	if plan.Mode == "uf2" && o.verify {
		req["verify"] = true
	}
	if _, err := pod.Command(ctx, req); err != nil {
		return usbFlashErr(what, err)
	}
	st, raw, err := usbFlashWait(ctx, pod, poll, say)
	if err != nil {
		return usbFlashErr(what, err)
	}
	if !st.OK {
		return fmt.Errorf("%s", usbFlashFailure(what, filepath.Base(o.file), st))
	}
	if o.asJSON {
		fmt.Fprintln(out, strings.TrimSpace(string(raw)))
		return nil
	}
	end := "The board runs the new firmware."
	switch {
	case o.noLeave && plan.Mode != "uf2" && plan.Mode != "file":
		end = "The board stays in its bootloader (--no-leave)."
	case plan.Mode == "file":
		end = "The loader takes the file from the drive and restarts the board."
	case plan.Mode == "uf2":
		end = "The bootloader restarts the board."
	}
	proto := ""
	if st.Protocol != "" {
		proto = " (" + st.Protocol + ")"
	}
	fmt.Fprintf(out, "Flashed %s to %s %s over USB, %s mode%s, in %.1f s: ok. %s\n",
		filepath.Base(o.file), dev.ids(), usbNames(dev), plan.Mode, proto, time.Since(t0).Seconds(), end)
	if n := strings.TrimSpace(st.Note); n != "" {
		fmt.Fprintf(out, "Read-back: %s\n", n)
	}
	return nil
}

func planAddr(p *usbFlashPlan) string {
	switch {
	case p.Mode == "file":
		return ", as " + p.Name
	case p.HasAddr:
		return fmt.Sprintf(", at 0x%08x", p.Address)
	}
	return ""
}

// usbWaitDevice polls usb_host until a device has enumerated (it returns the device report), or
// the context ends or the enumeration is stuck.
func usbWaitDevice(ctx context.Context, c podCommander, first *usbHostReport, poll time.Duration, say func(string, ...any)) (*usbDevice, error) {
	announced := false
	r := first
	noDevice := func() error {
		msg := "no USB device enumerated"
		if h := usbLineHint(r); h != "" {
			msg += ": " + h
		} else if r.Connected {
			msg += " (a device is connected but did not enumerate)"
		}
		return fmt.Errorf("%s: %w", msg, ctx.Err())
	}
	for {
		if r.Enumeration == "stuck" {
			return nil, fmt.Errorf("a USB device is attached but did not finish enumerating: %s", usbLineHint(r))
		}
		if r.Attached && r.Enumeration == "done" && r.Device != nil {
			return r.Device, nil
		}
		if !announced {
			say("waiting for a USB device on %s...", r.Port)
			announced = true
		}
		select {
		case <-ctx.Done():
			return nil, noDevice()
		case <-time.After(poll):
		}
		next, _, err := usbHostRead(ctx, c)
		if err != nil {
			if ctx.Err() != nil { // the deadline hit during the read: same outcome as hitting it while idle
				return nil, noDevice()
			}
			return nil, err
		}
		r = next
		if r.Mode != "host" {
			return nil, fmt.Errorf("USB host mode ended while waiting for the device (its timeout ran out, or `usb-host off`)")
		}
	}
}

// usbFlashWait polls usb_flash until the flash has ended and returns its final status. Progress
// is printed when the phase changes and at each 10 percent. On cancel it aborts the flash.
func usbFlashWait(ctx context.Context, c podCommander, poll time.Duration, say func(string, ...any)) (*usbFlashStatus, json.RawMessage, error) {
	lastPhase, lastPct := "", -1
	abort := func() (*usbFlashStatus, json.RawMessage, error) {
		actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = c.Command(actx, map[string]any{"cmd": "usb_flash", "abort": true})
		cancel()
		return nil, nil, fmt.Errorf("flash: %w (the flash was aborted)", ctx.Err())
	}
	for {
		select {
		case <-ctx.Done():
			return abort()
		case <-time.After(poll):
		}
		raw, err := c.Command(ctx, map[string]any{"cmd": "usb_flash"})
		if err != nil {
			if ctx.Err() != nil { // cancelled during the poll: still abort the flash on the pod
				return abort()
			}
			return nil, nil, err
		}
		var st usbFlashStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, nil, fmt.Errorf("unexpected usb_flash reply %s", strings.TrimSpace(string(raw)))
		}
		if !st.Running && st.HaveResult {
			return &st, raw, nil
		}
		pct := -1
		if st.Total > 0 {
			pct = st.Done * 100 / st.Total / 10 * 10
		}
		if st.Phase != lastPhase || pct != lastPct {
			lastPhase, lastPct = st.Phase, pct
			if st.Total > 0 {
				say("%s: %d of %d (%d%%)", orDash(st.Phase), st.Done, st.Total, st.Done*100/st.Total)
			} else {
				say("%s", orDash(st.Phase))
			}
		}
	}
}

// usbFlashFailure renders a failed flash: the phase, the pod's own reason, and a hint for the
// reasons that have a usual fix.
func usbFlashFailure(what, file string, st *usbFlashStatus) string {
	reason := strings.TrimSpace(st.Result)
	if reason == "" {
		reason = "no reason given"
	}
	msg := fmt.Sprintf("%s: flashing %s failed in the %s phase (%s mode): %s", what, file, orDash(st.Phase), orDash(st.Mode), reason)
	low := strings.ToLower(reason)
	if n := strings.TrimSpace(st.Note); n != "" {
		msg += " [" + n + "]"
	}
	hint := ""
	switch {
	case reason == "aborted":
		msg = fmt.Sprintf("%s: flashing %s was aborted in the %s phase", what, file, orDash(st.Phase))
	case strings.Contains(low, "not a uf2"):
		hint = "the file is not a UF2 file: pass --mode file to copy it to the drive as it is, or convert it to .uf2"
	case strings.Contains(low, "no dfu interface"):
		hint = "the board is not in its DFU bootloader: for an STM32 set BOOT0 high and reset it"
	case strings.Contains(low, "file system"):
		hint = "the drive has no FAT12 or FAT16 file system the pod can write; re-enter the bootloader"
	case strings.Contains(low, "no room"):
		hint = "the drive is too small for the file"
	case strings.Contains(low, "no bootloader answered"):
		hint = "put the board in its bootloader (double-tap reset) and check --mode"
	case strings.Contains(low, "not a teensy"):
		hint = "press the Teensy's button to start its loader"
	case strings.Contains(low, "overwrite the bootloader"):
		hint = "choose a start address above the bootloader with --address"
	case strings.Contains(low, "read back different"):
		hint = "a page did not stick: check the board's power and cable, then flash again"
	case strings.Contains(low, "not an rp2040"):
		hint = "hold BOOTSEL while plugging the Pico in (or double-tap reset on a board with a reset button)"
	case strings.Contains(low, "loader reported a failure"):
		hint = "the loader rejected the file (its FAIL.TXT is quoted above): check the file is built for this board"
	case strings.Contains(low, "does not fit"):
		hint = "check --address against the board's flash layout"
	}
	if hint != "" {
		msg += " (" + hint + ")"
	}
	return msg
}
