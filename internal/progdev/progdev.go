// Package progdev is the table of chips the pod's prog_flash command can program and the logic
// around it: finding a chip by its ID, checking the I/O voltage, reading an image (.hex, .elf,
// .bin) and cutting it into the chip's memories. The table is prog_devices.json (schema in
// prog_devices.README.md), copied from the embeddedci-python SDK by scripts/sync-prog-devices.sh;
// this package mirrors the SDK's embeddedci/benchpod/prog.py.
package progdev

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed prog_devices.json
var raw []byte

// Targets maps a `flash --target` name to the pod's prog_flash protocol (the SDK's TARGETS).
var Targets = map[string]string{
	"avr-updi": "updi", "updi": "updi", "pic": "pic", "efm8": "c2", "c2": "c2",
	"attiny-tpi": "tpi", "tpi": "tpi", "xmega": "pdi", "pdi": "pdi",
}

// MemOrder is the order memories are written: flash first, the configuration (fuses, lock) last
// because it can lock the chip or switch off its programming pin.
var MemOrder = []string{"flash", "eeprom", "user", "config"}

// Proto is the protocol a --target value names ("" if it is not one).
func Proto(target string) string { return Targets[strings.ToLower(strings.TrimSpace(target))] }

// Memory is one memory of a chip: Addr is where the pod addresses its first byte, Hex where it
// sits in a .hex/.elf image, Size and Page in bytes, Align the unit a region starts and ends on,
// Stride the image bytes one byte of this memory takes.
type Memory struct {
	Name                                 string
	Size, Page, Addr, Hex, Align, Stride int
}

// Device is one chip of the table.
type Device struct {
	Name, Family, Proto, ID     string
	Mem                         map[string]Memory
	Flags, CPAddr               int
	VMinMV, VMaxMV, EraseVMinMV int
	VNote, Notes                string
	Aliases                     []string
	ChipEraseOnly               bool
}

// ExpectID is the expect_id the pod checks before it erases: the table's id up to its first "??".
func (d Device) ExpectID() string { return strings.SplitN(d.ID, "??", 2)[0] }

// MatchesID: the pod's chip ID starts with the hex bytes of the table's id (?? matches any byte).
func (d Device) MatchesID(chipID string) bool { return idMatches(d.ID, chipID) }

func idMatches(pattern, chipID string) bool {
	pat := strings.ReplaceAll(strings.ToLower(pattern), " ", "")
	got := strings.TrimPrefix(strings.ReplaceAll(strings.ToLower(chipID), " ", ""), "0x")
	if pat == "" || len(got) < len(pat) {
		return false
	}
	for i := 0; i < len(pat); i++ {
		if pat[i] != '?' && pat[i] != got[i] {
			return false
		}
	}
	return true
}

// Ignore is an image region left out without an error (AVR lock bits, the signature row).
type Ignore struct {
	Hex, Size int
	Name      string
}

// Family is a table family: the chips one target writes.
type Family struct {
	Proto, Target, Title string
	Devices              []Device
	Ignore               []Ignore
}

type rawMem struct {
	Size   int `json:"size"`
	Page   int `json:"page"`
	Addr   int `json:"addr"`
	Hex    int `json:"hex"`
	Align  int `json:"align"`
	Stride int `json:"stride"`
}

type rawDev struct {
	Name          string            `json:"name"`
	Aliases       []string          `json:"aliases"`
	ID            string            `json:"id"`
	Family        string            `json:"family"`
	Mem           map[string]rawMem `json:"mem"`
	Opts          map[string]int    `json:"opts"`
	VMin          int               `json:"vmin_mv"`
	VMax          int               `json:"vmax_mv"`
	EraseVMin     int               `json:"erase_vmin_mv"`
	VNote         string            `json:"vnote"`
	Notes         string            `json:"notes"`
	ChipEraseOnly bool              `json:"chip_erase_only"`
}

var families map[string]*Family

func init() {
	var t struct {
		Schema   int `json:"schema"`
		Families map[string]struct {
			Target string `json:"target"`
			Title  string `json:"title"`
			Ignore []struct {
				Hex  int    `json:"hex"`
				Size int    `json:"size"`
				Name string `json:"name"`
			} `json:"ignore"`
			Devices []rawDev `json:"devices"`
		} `json:"families"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		panic("progdev: prog_devices.json: " + err.Error())
	}
	if t.Schema != 1 {
		panic(fmt.Sprintf("progdev: prog_devices.json: unknown schema %d", t.Schema))
	}
	families = map[string]*Family{}
	for proto, f := range t.Families {
		fam := &Family{Proto: proto, Target: f.Target, Title: f.Title}
		for _, g := range f.Ignore {
			fam.Ignore = append(fam.Ignore, Ignore{g.Hex, g.Size, g.Name})
		}
		for _, r := range f.Devices {
			d := Device{Name: r.Name, Family: r.Family, Proto: proto, ID: strings.ToLower(r.ID),
				Mem: map[string]Memory{}, Flags: r.Opts["flags"], CPAddr: r.Opts["cp_addr"],
				VMinMV: r.VMin, VMaxMV: r.VMax, EraseVMinMV: r.EraseVMin, VNote: r.VNote,
				Notes: r.Notes, Aliases: r.Aliases, ChipEraseOnly: r.ChipEraseOnly}
			for name, m := range r.Mem {
				page := m.Page
				if page == 0 {
					page = 1
				}
				align := m.Align
				if align == 0 {
					align = 1
					if name == "flash" {
						align = page
					}
				}
				stride := m.Stride
				if stride == 0 {
					stride = 1
				}
				d.Mem[name] = Memory{name, m.Size, page, m.Addr, m.Hex, align, stride}
			}
			fam.Devices = append(fam.Devices, d)
		}
		families[proto] = fam
	}
}

// Fam is the table family of a protocol.
func Fam(proto string) *Family { return families[proto] }

func norm(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// Find looks a device up by name or alias, ignoring case, spaces and dashes.
func (f *Family) Find(name string) *Device {
	k := norm(name)
	for i := range f.Devices {
		d := &f.Devices[i]
		if norm(d.Name) == k {
			return d
		}
		for _, a := range d.Aliases {
			if norm(a) == k {
				return d
			}
		}
	}
	return nil
}

// FindDevice picks the chip for the pod's chipID and/or a name the user gave (the SDK's
// find_device): a name must be in the table and, when chipID is known, have that ID; without a
// name the ID picks the chip, and chips sharing an ID must have the same memories.
func FindDevice(proto, chipID, name string) (*Device, error) {
	fam := families[proto]
	if name != "" {
		d := fam.Find(name)
		if d == nil {
			var ex []string
			for i, x := range fam.Devices {
				if i == 8 {
					break
				}
				ex = append(ex, x.Name)
			}
			return nil, fmt.Errorf("device %q is not in the %s table (it has %d chips, for example %s); check the spelling, or leave --device out to identify the chip by its ID",
				name, fam.Title, len(fam.Devices), strings.Join(ex, ", "))
		}
		if chipID != "" && !d.MatchesID(chipID) {
			return nil, fmt.Errorf("the chip answers with ID %s, which is not a %s (ID %s); check the board, or leave --device out to identify the chip by its ID", chipID, d.Name, d.ID)
		}
		return d, nil
	}
	if chipID == "" {
		return nil, fmt.Errorf("no chip ID to look up: pass --device")
	}
	var found []*Device
	for i := range fam.Devices {
		if fam.Devices[i].MatchesID(chipID) {
			found = append(found, &fam.Devices[i])
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("the chip answers with ID %s, which is not in the %s table (%d chips). If it is a chip this programmer supports, pass --device with its name when the table has it, or report the ID",
			chipID, fam.Title, len(fam.Devices))
	}
	for _, d := range found[1:] {
		if !sameLayout(*found[0], *d) {
			var n []string
			for _, x := range found {
				n = append(n, x.Name)
			}
			return nil, fmt.Errorf("ID %s fits %s, which differ: pass --device with the one on the board", chipID, strings.Join(n, ", "))
		}
	}
	return found[0], nil
}

func sameLayout(a, b Device) bool {
	if a.Flags != b.Flags || a.CPAddr != b.CPAddr || len(a.Mem) != len(b.Mem) {
		return false
	}
	for k, m := range a.Mem {
		if b.Mem[k] != m {
			return false
		}
	}
	return true
}

// Names lists the device names of a protocol, sorted.
func Names(proto string) []string {
	var out []string
	for _, d := range families[proto].Devices {
		out = append(out, d.Name)
	}
	sort.Strings(out)
	return out
}

// CheckErase refuses erase "page" on a chip that can only be chip-erased.
func CheckErase(d *Device, erase string) error {
	if d.ChipEraseOnly && erase == "page" {
		return fmt.Errorf("the %s erases 32-word rows but writes 8 words at a time, so erasing single pages would wipe their neighbours: use --erase chip (the default) or --erase none on an erased chip", d.Name)
	}
	return nil
}

// CheckVoltage refuses a chip the pod's I/O voltage mv (0: not known, no check) cannot program,
// before anything is written. Messages follow the SDK's check_voltage.
func CheckVoltage(d *Device, mv int, erase string) error {
	if mv == 0 {
		return nil
	}
	v := float64(mv) / 1000
	note := ""
	if d.VNote != "" {
		note = " (" + d.VNote + ")"
	}
	if d.VMinMV != 0 && mv < d.VMinMV {
		if d.VMinMV >= 4000 {
			return fmt.Errorf("the %s can only be programmed at %g V or more, and the pod's I/O is %g V: it cannot program this chip. Use the board's own 5 V with a level shifter on the programming pins and a programmer that drives 5 V%s",
				d.Name, float64(d.VMinMV)/1000, v, note)
		}
		return fmt.Errorf("the %s needs at least %g V to be programmed and the pod's I/O is %g V: set the I/O voltage to the chip's supply (benchpod la-voltage)%s",
			d.Name, float64(d.VMinMV)/1000, v, note)
	}
	if d.VMaxMV != 0 && mv > d.VMaxMV {
		return fmt.Errorf("the %s takes at most %g V and the pod's I/O is %g V: set the I/O voltage to the chip's supply (benchpod la-voltage)", d.Name, float64(d.VMaxMV)/1000, v)
	}
	if erase == "chip" && d.EraseVMinMV != 0 && mv < d.EraseVMinMV {
		return fmt.Errorf("the %s's chip erase needs the supply above %g V (its brown-out level) and the pod's I/O is %g V, so the erase can be ignored. Power the chip and set the I/O voltage to 3.3 V, or pass --erase page for an already erased chip",
			d.Name, float64(d.EraseVMinMV)/1000, v)
	}
	return nil
}

// ---- image files ----

// Segment is one run of image bytes at Addr (a .hex / .elf address).
type Segment struct {
	Addr int
	Data []byte
}

func merge(parts []Segment) []Segment {
	cells := map[int]byte{}
	for _, p := range parts {
		for i, b := range p.Data {
			cells[p.Addr+i] = b
		}
	}
	addrs := make([]int, 0, len(cells))
	for a := range cells {
		addrs = append(addrs, a)
	}
	sort.Ints(addrs)
	var out []Segment
	for _, a := range addrs {
		if k := len(out) - 1; k >= 0 && out[k].Addr+len(out[k].Data) == a {
			out[k].Data = append(out[k].Data, cells[a])
		} else {
			out = append(out, Segment{a, []byte{cells[a]}})
		}
	}
	return out
}

// ParseIHex returns the runs of an Intel HEX file.
func ParseIHex(text []byte) ([]Segment, error) {
	var parts []Segment
	base := 0
	for n, line := range strings.Split(string(text), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line[0] != ':' {
			return nil, fmt.Errorf("line %d: not an Intel HEX record", n+1)
		}
		rec, err := hex.DecodeString(line[1:])
		if err != nil {
			return nil, fmt.Errorf("line %d: not hex", n+1)
		}
		if len(rec) < 5 || len(rec) != int(rec[0])+5 {
			return nil, fmt.Errorf("line %d: the record length does not match its bytes", n+1)
		}
		var sum byte
		for _, b := range rec {
			sum += b
		}
		if sum != 0 {
			return nil, fmt.Errorf("line %d: bad checksum", n+1)
		}
		count, off := int(rec[0]), int(rec[1])<<8|int(rec[2])
		body := rec[4 : 4+count]
		switch {
		case rec[3] == 0:
			parts = append(parts, Segment{base + off, append([]byte(nil), body...)})
		case rec[3] == 1:
			return merge(parts), nil
		case rec[3] == 2 && count == 2:
			base = (int(body[0])<<8 | int(body[1])) << 4
		case rec[3] == 4 && count == 2:
			base = (int(body[0])<<8 | int(body[1])) << 16
		}
	}
	return merge(parts), nil
}

// ParseELF returns the loadable segments of a 32-bit little-endian ELF at their load addresses.
func ParseELF(data []byte) ([]Segment, error) {
	if len(data) < 52 || !bytes.HasPrefix(data, []byte("\x7fELF")) {
		return nil, fmt.Errorf("not an ELF file")
	}
	if data[4] != 1 || data[5] != 1 {
		return nil, fmt.Errorf("only 32-bit little-endian ELF files are supported (these chips' toolchains write them); convert with objcopy -O ihex")
	}
	le := binary.LittleEndian
	phoff := int(le.Uint32(data[28:]))
	phentsize, phnum := int(le.Uint16(data[42:])), int(le.Uint16(data[44:]))
	var parts []Segment
	for i := 0; i < phnum; i++ {
		o := phoff + i*phentsize
		if o+32 > len(data) {
			return nil, fmt.Errorf("the ELF program headers run past the end of the file")
		}
		typ, off, paddr, filesz := le.Uint32(data[o:]), int(le.Uint32(data[o+4:])), int(le.Uint32(data[o+12:])), int(le.Uint32(data[o+16:]))
		if typ == 1 && filesz > 0 {
			if off+filesz > len(data) {
				return nil, fmt.Errorf("the ELF segment runs past the end of the file")
			}
			parts = append(parts, Segment{paddr, append([]byte(nil), data[off:off+filesz]...)})
		}
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("the ELF file has no loadable segments")
	}
	return merge(parts), nil
}

// LoadImage reads .hex (Intel HEX), .elf or .bin file contents; a .bin goes at loadAddress.
func LoadImage(name string, data []byte, loadAddress int) ([]Segment, error) {
	ext := strings.ToLower(name[strings.LastIndex(name, ".")+1:])
	if !strings.Contains(name, ".") {
		ext = ""
	}
	var segs []Segment
	var err error
	switch {
	case bytes.HasPrefix(data, []byte("\x7fELF")) || ext == "elf" || ext == "axf" || ext == "out":
		segs, err = ParseELF(data)
	case ext == "hex" || ext == "ihex" || ext == "ihx" || ext == "a43" || bytes.HasPrefix(data, []byte(":")):
		segs, err = ParseIHex(data)
	case len(data) > 0:
		segs = []Segment{{loadAddress, data}}
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("%s holds no image bytes", name)
	}
	return segs, nil
}

// Region is one prog_flash write: Data into memory Mem at the pod's address Addr.
type Region struct {
	Mem        string
	Addr, Page int
	Data       []byte
}

func layout(d *Device) string {
	var parts []string
	for _, n := range MemOrder {
		if m, ok := d.Mem[n]; ok {
			parts = append(parts, fmt.Sprintf("%s 0x%x..0x%x", n, m.Hex, m.Hex+m.Size*m.Stride-1))
		}
	}
	return "it has " + strings.Join(parts, ", ")
}

func alignTo(v, unit int, up bool) int {
	if up {
		return (v + unit - 1) / unit * unit
	}
	return v / unit * unit
}

// SplitRegions cuts segments into the chip's memories (the SDK's split_regions): regions in write
// order (flash, EEPROM, user, config; each ascending), aligned to each memory's unit and padded
// with 0xFF, plus notes about what was left out.
func SplitRegions(segs []Segment, d *Device, ignore []Ignore) ([]Region, []string, error) {
	var notes []string
	placed := map[string]map[int]byte{}
	for n := range d.Mem {
		placed[n] = map[int]byte{}
	}
	find := func(a int) *Memory {
		for _, n := range MemOrder {
			if m, ok := d.Mem[n]; ok && m.Hex <= a && a < m.Hex+m.Size*m.Stride {
				mm := m
				return &mm
			}
		}
		return nil
	}
	for _, s := range segs {
		for i := 0; i < len(s.Data); {
			a := s.Addr + i
			hit := find(a)
			if hit == nil {
				var skip *Ignore
				for k := range ignore {
					if ignore[k].Hex <= a && a < ignore[k].Hex+ignore[k].Size {
						skip = &ignore[k]
						break
					}
				}
				if skip != nil {
					n := skip.Hex + skip.Size - a
					if rest := len(s.Data) - i; rest < n {
						n = rest
					}
					name := skip.Name
					if name == "" {
						name = "region"
					}
					note := fmt.Sprintf("left out the %s at 0x%x (%d bytes): the programmer does not write it", name, a, n)
					if !contains(notes, note) {
						notes = append(notes, note)
					}
					i += n
					continue
				}
				return nil, nil, fmt.Errorf("the image has data at 0x%x that is not in any memory of the %s: %s; build for this chip, or pass --device for the right one", a, d.Name, layout(d))
			}
			end := len(s.Data) - i
			if lim := hit.Hex + hit.Size*hit.Stride - a; lim < end {
				end = lim
			}
			for k := 0; k < end; k++ {
				off := a + k - hit.Hex
				if hit.Stride > 1 {
					if off%hit.Stride != 0 {
						continue // the high byte of a 16-bit word
					}
					placed[hit.Name][off/hit.Stride] = s.Data[i+k]
				} else {
					placed[hit.Name][off] = s.Data[i+k]
				}
			}
			i += end
		}
	}
	var regions []Region
	for _, name := range MemOrder {
		cells := placed[name]
		if len(cells) == 0 {
			continue
		}
		m := d.Mem[name]
		offs := make([]int, 0, len(cells))
		for o := range cells {
			offs = append(offs, o)
		}
		sort.Ints(offs)
		var runs [][2]int
		start, prev := offs[0], offs[0]
		for _, o := range offs[1:] {
			if o != prev+1 {
				runs = append(runs, [2]int{start, prev + 1})
				start = o
			}
			prev = o
		}
		runs = append(runs, [2]int{start, prev + 1})
		var joined [][2]int
		for _, r := range runs {
			lo, hi := alignTo(r[0], m.Align, false), alignTo(r[1], m.Align, true)
			if k := len(joined) - 1; k >= 0 && lo <= joined[k][1] {
				if hi > joined[k][1] {
					joined[k][1] = hi
				}
			} else {
				joined = append(joined, [2]int{lo, hi})
			}
		}
		for _, r := range joined {
			lo, hi := r[0], r[1]
			if hi > m.Size {
				hi = m.Size
			}
			buf := make([]byte, hi-lo)
			for o := lo; o < hi; o++ {
				if b, ok := cells[o]; ok {
					buf[o-lo] = b
				} else {
					buf[o-lo] = 0xFF
				}
			}
			regions = append(regions, Region{Mem: name, Addr: m.Addr + lo, Page: m.Page, Data: buf})
		}
	}
	if len(regions) == 0 {
		return nil, nil, fmt.Errorf("the image has no bytes for the %s: %s", d.Name, layout(d))
	}
	return regions, notes, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
