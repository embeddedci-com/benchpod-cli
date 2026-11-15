package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// ESP32-family targets have no SWD: `flash --target esp32` has the pod write them through the
// chip's UART ROM bootloader (the firmware's esp_flash command), the way esptool does. Each region
// is uploaded into the pod's PSRAM (load_bin), then the pod writes it in the background and this
// polls its status.

var espTargetRe = regexp.MustCompile(`^esp32(-?(s2|s3|c2|c3|c5|c6|h2|p4))?$`)

// isESPTarget: --target names an ESP32-family chip (esp32, esp32s3, ESP32-C3).
func isESPTarget(t string) bool {
	return espTargetRe.MatchString(strings.ToLower(strings.TrimSpace(t)))
}

const (
	espDefaultBaud      = 460800
	espDefaultFlashSize = 4 << 20
	espPollInterval     = 250 * time.Millisecond
)

// espBootSignals / espENSignals are the wiring-profile signal names used when --boot / --en are
// not given (case-insensitive), the same as the SDK.
var (
	espBootSignals = []string{"BOOT", "IO0", "GPIO0", "IO9", "GPIO9", "ESP_BOOT"}
	espENSignals   = []string{"EN", "CHIP_EN", "ESP_EN", "CHIP_PU"}
)

type espRegion struct {
	offset int
	data   []byte
	name   string
}

var espSizes = map[string]int{"256KB": 256 << 10, "512KB": 512 << 10, "1MB": 1 << 20, "2MB": 2 << 20,
	"4MB": 4 << 20, "8MB": 8 << 20, "16MB": 16 << 20, "32MB": 32 << 20, "64MB": 64 << 20, "128MB": 128 << 20}

// parseFlashSize: "4MB", "4194304" or "" (4 MB).
func parseFlashSize(s string) (int, error) {
	v := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	if v == "" {
		return espDefaultFlashSize, nil
	}
	if n, ok := espSizes[v]; ok {
		return n, nil
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n, nil
	}
	return 0, fmt.Errorf("--flash-size must be like 4MB or a byte count, got %q", s)
}

// espImages reads the regions to write: --file (a .bin at 0, or an ESP-IDF flasher_args.json)
// and --image OFFSET=FILE pairs. It also returns the flash size a flasher_args.json names.
func espImages(file string, images []string) ([]espRegion, string, error) {
	var regions []espRegion
	flashSize := ""
	if f := strings.TrimSpace(file); f != "" {
		if strings.HasSuffix(f, ".json") {
			raw, err := os.ReadFile(f)
			if err != nil {
				return nil, "", err
			}
			var args struct {
				FlashFiles    map[string]string `json:"flash_files"`
				FlashSettings struct {
					FlashSize string `json:"flash_size"`
				} `json:"flash_settings"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, "", fmt.Errorf("%s: %w", f, err)
			}
			if len(args.FlashFiles) == 0 {
				return nil, "", fmt.Errorf("%s: no flash_files (is this an ESP-IDF flasher_args.json?)", f)
			}
			base := filepath.Dir(f)
			for off, name := range args.FlashFiles {
				r, err := espRegionFrom(off, filepath.Join(base, name))
				if err != nil {
					return nil, "", err
				}
				regions = append(regions, r)
			}
			if fs := args.FlashSettings.FlashSize; fs != "" && fs != "keep" && fs != "detect" {
				flashSize = fs
			}
		} else {
			r, err := espRegionFrom("0", f)
			if err != nil {
				return nil, "", err
			}
			regions = append(regions, r)
		}
	}
	for _, pair := range images {
		off, path, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, "", fmt.Errorf("--image wants OFFSET=FILE, e.g. 0x10000=build/app.bin, got %q", pair)
		}
		r, err := espRegionFrom(off, path)
		if err != nil {
			return nil, "", err
		}
		regions = append(regions, r)
	}
	if len(regions) == 0 {
		return nil, "", fmt.Errorf("an ESP32 target needs --file (a .bin written at 0, or an ESP-IDF flasher_args.json) or --image OFFSET=FILE")
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i].offset < regions[j].offset })
	for i := 1; i < len(regions); i++ {
		a := regions[i-1]
		if a.offset+len(a.data) > regions[i].offset {
			return nil, "", fmt.Errorf("%s at 0x%x overlaps %s at 0x%x", a.name, a.offset, regions[i].name, regions[i].offset)
		}
	}
	return regions, flashSize, nil
}

func espRegionFrom(off, path string) (espRegion, error) {
	o, err := strconv.ParseInt(strings.TrimSpace(off), 0, 64)
	if err != nil || o < 0 {
		return espRegion{}, fmt.Errorf("bad flash offset %q for %s", off, path)
	}
	if o%4096 != 0 {
		return espRegion{}, fmt.Errorf("offset 0x%x for %s is not a multiple of 4096 (a flash sector)", o, path)
	}
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return espRegion{}, err
	}
	if len(data) == 0 {
		return espRegion{}, fmt.Errorf("%s is empty", path)
	}
	return espRegion{offset: int(o), data: data, name: filepath.Base(path)}, nil
}

// espPins resolves --rx/--tx/--boot/--en, falling back to the wiring profile.
func (f *flashFlags) espPins(w *podWiring) (map[string]any, error) {
	pin := func(flag, val, role string, signals []string) (int, error) {
		if strings.TrimSpace(val) != "" {
			n, err := parseLAPin(val)
			if err != nil {
				return 0, fmt.Errorf("--%s: %w", flag, err)
			}
			return n, nil
		}
		if role != "" {
			if n, ok := w.channel(role); ok {
				return n, nil
			}
		}
		for _, s := range signals {
			if n, ok := w.channel(s); ok {
				return n, nil
			}
		}
		return 0, nil
	}
	rx, err := pin("rx", f.espRx, "uart_rx", nil)
	if err != nil {
		return nil, err
	}
	tx, err := pin("tx", f.espTx, "uart_tx", nil)
	if err != nil {
		return nil, err
	}
	if rx == 0 || tx == 0 {
		return nil, fmt.Errorf("--rx and --tx are required (the LA pins on the target's TXD0 and RXD0), or set uart_rx and uart_tx in the pod's wiring profile")
	}
	boot, err := pin("boot", f.espBoot, "", espBootSignals)
	if err != nil {
		return nil, err
	}
	if boot == 0 {
		return nil, fmt.Errorf("--boot is required: the LA pin on the target's IO0 (IO9 on the C3/C6), or a wiring-profile signal named BOOT, IO0 or IO9")
	}
	pins := map[string]any{"rx": rx, "tx": tx, "boot": boot, "en": "nrst"}
	switch v := strings.ToLower(strings.TrimSpace(f.espEN)); v {
	case "nrst":
	case "":
		if n, _ := pin("en", "", "", espENSignals); n != 0 {
			pins["en"] = n
		}
	default:
		n, err := parseLAPin(v)
		if err != nil {
			return nil, fmt.Errorf("--en: an LA pin or nrst: %w", err)
		}
		pins["en"] = n
	}
	return pins, nil
}

type espStatus struct {
	Running bool    `json:"running"`
	Phase   string  `json:"phase"`
	Done    int     `json:"done"`
	Total   int     `json:"total"`
	Chip    string  `json:"chip"`
	Baud    int     `json:"baud"`
	MD5     string  `json:"md5"`
	Resends int     `json:"resends"`
	Result  *string `json:"result"`
}

func runFlashESP(g *globalFlags, f *flashFlags, spec ConnSpec) error {
	if spec.IsCloud() {
		return fmt.Errorf("flashing an ESP32 from the CLI needs a direct link to the pod (its network address or `usb`); over embeddedci.com use the Python SDK (bp.flash_esp) or the MCP server")
	}
	if !spec.IsNetwork() {
		return fmt.Errorf("flashing an ESP32 needs the pod's network: pass its address, or `usb` on firmware 3.9.0+ with USB networking (the image is uploaded into the pod first)")
	}
	regions, argsSize, err := espImages(f.file, f.espImages)
	if err != nil {
		return err
	}
	sizeFlag := f.espFlashSize
	if strings.TrimSpace(sizeFlag) == "" {
		sizeFlag = argsSize
	}
	flashSize, err := parseFlashSize(sizeFlag)
	if err != nil {
		return err
	}
	for _, r := range regions {
		if r.offset+len(r.data) > flashSize {
			return fmt.Errorf("%s at 0x%x (%d bytes) does not fit a %d KB flash; pass --flash-size", r.name, r.offset, len(r.data), flashSize>>10)
		}
	}
	if f.espBaud < 9600 || f.espBaud > 2000000 {
		return fmt.Errorf("--esp-baud must be 9600..2000000")
	}
	pins, err := f.espPins(loadPodWiring(g, os.Stderr))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(flashTimeout))
	defer cancel()
	defer installSignalHandler(ctx, cancel)()
	c := &tcpclient.Client{Addr: spec.Addr}
	t0 := time.Now()
	var chip string
	var baud int
	for i, r := range regions {
		if _, err := c.StagePSRAM(ctx, r.data); err != nil {
			return fmt.Errorf("flash: uploading %s: %w", r.name, err)
		}
		req := map[string]any{"cmd": "esp_flash", "op": "start", "offset": r.offset, "len": len(r.data),
			"baud": f.espBaud, "flash_size": flashSize, "run": !f.noReset && i == len(regions)-1}
		for k, v := range pins {
			req[k] = v
		}
		if _, err := c.Command(ctx, req); err != nil {
			if pe, ok := tcpclient.AsPodError(err); ok && strings.Contains(pe.Error(), "unknown cmd") {
				return fmt.Errorf("flash: this pod's firmware cannot flash an ESP32 (no esp_flash command): update it with `benchpod flash-self`")
			}
			return fmt.Errorf("flash: %w", err)
		}
		st, err := espWait(ctx, c, r)
		if err != nil {
			return err
		}
		chip, baud = st.Chip, st.Baud
		res := ""
		if st.Result != nil {
			res = *st.Result
		}
		if res != "ok" && res != "unverified" {
			msg := res
			if res == "no answer from the ROM loader" {
				msg += ": check that the target is powered, its TXD0 goes to --rx and RXD0 to --tx, IO0 (IO9 on the C3/C6) to --boot, and EN to --en"
			}
			return fmt.Errorf("flash: %s at 0x%x failed in %s: %s", r.name, r.offset, st.Phase, msg)
		}
		fmt.Printf("0x%06x  %-24s %8d bytes  %s  md5 %s\n", r.offset, r.name, len(r.data), res, st.MD5)
	}
	fmt.Printf("%s: %d region(s) written at %d baud in %.1f s\n", chip, len(regions), baud, time.Since(t0).Seconds())
	return nil
}

// espWait polls esp_flash status until the write ends, logging progress every 10%.
func espWait(ctx context.Context, c *tcpclient.Client, r espRegion) (*espStatus, error) {
	lastPct := -1
	for {
		select {
		case <-ctx.Done():
			_, _ = c.Command(context.Background(), map[string]any{"cmd": "esp_flash", "op": "abort"})
			return nil, fmt.Errorf("flash: %s: %w (the write was stopped)", r.name, ctx.Err())
		case <-time.After(espPollInterval):
		}
		raw, err := c.Command(ctx, map[string]any{"cmd": "esp_flash", "op": "status"})
		if err != nil {
			return nil, fmt.Errorf("flash: status: %w", err)
		}
		var st espStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, fmt.Errorf("flash: status: %w", err)
		}
		if !st.Running {
			return &st, nil
		}
		if st.Total > 0 {
			if pct := st.Done * 100 / st.Total / 10 * 10; pct != lastPct {
				lastPct = pct
				log.Printf("flash: %s %d%% (%s)", strings.TrimSpace(st.Chip+" "+r.name), pct, st.Phase)
			}
		}
	}
}
