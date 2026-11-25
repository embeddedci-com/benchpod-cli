// Package detect is the ID table and the mapping behind `benchpod detect`: it turns what the pod's
// firmware reads off the target (an SWD debug-port reply, a JTAG chain, a USB device) into a chip
// name. The table is detect_ids.json. The canonical copy is embeddedci-python's
// packages/embeddedci/src/embeddedci/benchpod/detect_ids.json; this one is a copy, refreshed by
// scripts/sync-detect-ids.sh. The pod only reads IDs (dap_start op "id"); every naming decision is
// made here, so the SDK, the MCP server, the web app and the CLI share one table.
package detect

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
)

//go:embed detect_ids.json
var raw []byte

// Confidence levels of a Finding.
const (
	High   = "high"   // an exact table match (DEV_ID at its family's address, prog table ID, ESP chip, known VID:PID)
	Medium = "medium" // vendor and core known, the part is not
	Low    = "low"    // only a debug port (or an unknown device) answered
)

// Finding is one thing a method identified.
type Finding struct {
	Method     string         `json:"method"`
	Chip       string         `json:"chip"`
	Vendor     string         `json:"vendor"`
	Core       string         `json:"core"`
	ID         string         `json:"id"`
	Raw        any            `json:"raw"`
	Confidence string         `json:"confidence"`
	Pins       map[string]any `json:"pins"`
	Invasive   bool           `json:"invasive"`
}

// Skipped is a method that was not probed, and why.
type Skipped struct {
	Method string `json:"method"`
	Reason string `json:"reason"`
}

// Result is what detect reports (the JSON of `benchpod detect --json`).
type Result struct {
	Powered  bool      `json:"powered"`
	Findings []Finding `json:"findings"`
	Skipped  []Skipped `json:"skipped"`
	Notes    []string  `json:"notes"`
}

// SWDReply is the pod's reply to dap_start op "id" on SWD. Hex strings, lower case.
type SWDReply struct {
	Port     string    `json:"port"`
	Found    bool      `json:"found"`
	DPIDR    string    `json:"dpidr"`
	TargetID *string   `json:"targetid"`
	APIDR    *string   `json:"apidr"`
	CPUID    *string   `json:"cpuid"`
	Reads    []SWDRead `json:"reads"`
}

// SWDRead is one memory read of the reply; a nil Value is a read that faulted.
type SWDRead struct {
	Addr  string  `json:"addr"`
	Value *string `json:"value"`
}

// JTAGReply is the pod's reply to dap_start op "id" on JTAG: IDCODEs, TDO-nearest first.
type JTAGReply struct {
	Port    string   `json:"port"`
	Found   bool     `json:"found"`
	IDCodes []string `json:"idcodes"`
}

type idRegister struct {
	Vendor string            `json:"vendor"`
	STM32  bool              `json:"stm32"`
	What   string            `json:"what"`
	Addr   string            `json:"addr"`
	Mask   string            `json:"mask"`
	Cores  []string          `json:"cores"`
	Parts  map[string]string `json:"parts"`
}

type jtagRule struct {
	Mask      string `json:"mask"`
	Value     string `json:"value"`
	Name      string `json:"name"`
	Vendor    string `json:"vendor"`
	DebugPort bool   `json:"debug_port"`
}

type table struct {
	Version   int               `json:"version"`
	JEP106    map[string]string `json:"jep106"`
	ArmCores  map[string]string `json:"arm_cores"`
	Registers []idRegister      `json:"id_registers"`
	JTAG      []jtagRule        `json:"jtag"`
	USB       map[string]string `json:"usb"`
}

var ids = func() table {
	var t table
	if err := json.Unmarshal(raw, &t); err != nil || t.Version != 1 {
		panic(fmt.Sprintf("detect: bad embedded detect_ids.json: %v", err))
	}
	return t
}()

// MaxReads is how many addresses one dap_start op "id" reads.
const MaxReads = 8

func parseHex(s string) (uint32, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 32)
	return uint32(v), err == nil
}

func hex32(v uint32) string { return fmt.Sprintf("0x%08x", v) }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// partKey is how the table writes a masked ID: "0x" and as many hex digits as the mask has.
func partKey(v uint32, mask uint32) string {
	w := (bits.Len32(mask) + 3) / 4
	return fmt.Sprintf("0x%0*x", w, v&mask)
}

func designer(idcode uint32) string { return fmt.Sprintf("0x%03x", (idcode>>1)&0x7ff) }

// Core is the Arm core a CPUID value names ("" when it is not an Arm core in the table).
func Core(cpuid string) string {
	v, ok := parseHex(cpuid)
	if !ok || v>>24 != 0x41 {
		return ""
	}
	return ids.ArmCores[fmt.Sprintf("0x%x", (v>>4)&0xfff)]
}

// SWDReads is the list of ID addresses worth reading on a core (at most MaxReads), for the second
// dap_start op "id" call.
func SWDReads(core string) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range ids.Registers {
		has := false
		for _, c := range r.Cores {
			has = has || c == core
		}
		if has && !seen[r.Addr] && len(out) < MaxReads {
			seen[r.Addr] = true
			out = append(out, r.Addr)
		}
	}
	return out
}

func readValue(r SWDReply, addr string) (uint32, bool) {
	a, _ := parseHex(addr)
	for _, rd := range r.Reads {
		if x, ok := parseHex(rd.Addr); ok && x == a && rd.Value != nil {
			return parseHex(*rd.Value)
		}
	}
	return 0, false
}

func stPart(devID uint32) (name string, ok bool) {
	for _, r := range ids.Registers {
		if !r.STM32 {
			continue
		}
		if n, ok := r.Parts[partKey(devID, 0xfff)]; ok {
			return n, true
		}
	}
	return "", false
}

// MapSWD names the chip from an SWD reply that carries the ID reads (and the DPIDR, TARGETID and
// CPUID of the first call). It returns nil when nothing answered.
func MapSWD(r SWDReply, pins map[string]any) *Finding {
	if !r.Found {
		return nil
	}
	core := Core(deref(r.CPUID))
	f := &Finding{Method: "swd", Core: core, Raw: r, Pins: pins}
	// 1. A table register that read back a known part.
	for _, reg := range ids.Registers {
		v, ok := readValue(r, reg.Addr)
		mask, _ := parseHex(reg.Mask)
		if !ok || !coreIn(reg.Cores, core) {
			continue
		}
		if name, ok := reg.Parts[partKey(v, mask)]; ok {
			f.Chip, f.Vendor, f.ID, f.Confidence = name, reg.Vendor, hex32(v), High
			return f
		}
	}
	// 2. TARGETID (DP version 2) of an ST part is the same DEV_ID.
	if t, ok := parseHex(deref(r.TargetID)); ok && designer(t) == "0x020" {
		if name, ok := stPart((t >> 12) & 0xfff); ok {
			f.Chip, f.Vendor, f.ID, f.Confidence = name, "STMicroelectronics", hex32(t), High
			return f
		}
	}
	// 3. A vendor register answered with a value but the part is not in the table.
	for _, reg := range ids.Registers {
		v, ok := readValue(r, reg.Addr)
		mask, _ := parseHex(reg.Mask)
		if ok && reg.STM32 && coreIn(reg.Cores, core) && v&mask != 0 && v != 0xffffffff {
			f.Chip = fmt.Sprintf("unknown %s part 0x%03x", reg.Vendor, v&mask)
			f.Vendor, f.ID, f.Confidence = reg.Vendor, hex32(v), Medium
			return f
		}
	}
	// 4. Only the debug port answered.
	f.Chip, f.Confidence = "unknown", Low
	f.ID = r.DPIDR
	if d, ok := parseHex(r.DPIDR); ok {
		f.Vendor = ids.JEP106[designer(d)]
	}
	return f
}

func coreIn(cores []string, core string) bool {
	for _, c := range cores {
		if c == core {
			return true
		}
	}
	return false
}

// MapJTAG names every device of a JTAG chain: the table's rules in order, then the ST
// boundary-scan rule (an IDCODE whose designer is ST and that no rule matches carries the STM32
// DEV_ID in bits 12 to 23). An IDCODE nothing names is a low finding.
func MapJTAG(r JTAGReply, pins map[string]any) []Finding {
	var out []Finding
	if !r.Found {
		return nil
	}
	for _, s := range r.IDCodes {
		v, ok := parseHex(s)
		if !ok {
			continue
		}
		f := Finding{Method: "jtag", ID: hex32(v), Raw: r, Pins: pins}
		matched := false
		for _, rule := range ids.JTAG {
			m, _ := parseHex(rule.Mask)
			want, _ := parseHex(rule.Value)
			if v&m != want {
				continue
			}
			f.Chip, f.Vendor, matched = rule.Name, rule.Vendor, true
			f.Confidence = High
			if rule.DebugPort {
				f.Confidence = Low // a debug port says a Cortex-M is there, not which
			}
			break
		}
		if !matched && designer(v) == "0x020" {
			if name, ok := stPart((v >> 12) & 0xfff); ok {
				f.Chip, f.Vendor, f.Confidence, matched = name, "STMicroelectronics", High, true
			}
		}
		if !matched {
			f.Chip, f.Vendor, f.Confidence = "unknown", ids.JEP106[designer(v)], Low
		}
		out = append(out, f)
	}
	return out
}

// USBName is the table's name for a VID:PID ("" when it has none). vid and pid are numbers.
func USBName(vid, pid int) string { return ids.USB[USBID(vid, pid)] }

// USBID is "vvvv:pppp", lower case.
func USBID(vid, pid int) string { return fmt.Sprintf("%04x:%04x", vid, pid) }
