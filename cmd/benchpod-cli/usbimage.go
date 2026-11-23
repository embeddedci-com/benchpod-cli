package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// File formats for `usb-flash`: Intel HEX and DFU files are converted to a binary with its start
// address (the pod's usb_flash takes a binary and an address); .uf2 files and the files copied to a
// drive go as they are.

// usbImageMaxSpan is the largest binary a .hex or .dfu file may expand to (gaps are filled with
// 0xFF). A file that spans more is almost certainly two images in one (code and a far-away
// bootloader), which a single write cannot take.
const usbImageMaxSpan = 64 << 20

type usbSegment struct {
	addr uint32
	data []byte
}

// flattenSegments joins segments into one binary starting at the lowest address, with 0xFF in the
// gaps. It returns the binary and its start address.
func flattenSegments(segs []usbSegment) ([]byte, uint32, error) {
	var live []usbSegment
	for _, s := range segs {
		if len(s.data) > 0 {
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		return nil, 0, fmt.Errorf("the file holds no data")
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].addr < live[j].addr })
	base := live[0].addr
	end := uint64(base)
	for _, s := range live {
		if e := uint64(s.addr) + uint64(len(s.data)); e > end {
			end = e
		}
	}
	span := end - uint64(base)
	if span > usbImageMaxSpan {
		return nil, 0, fmt.Errorf("the file spans %d MiB from 0x%08x (more than %d MiB): it holds regions far apart; split it", span>>20, base, usbImageMaxSpan>>20)
	}
	out := bytes.Repeat([]byte{0xFF}, int(span))
	var prevEnd uint64
	for i, s := range live {
		off := uint64(s.addr) - uint64(base)
		if i > 0 && off < prevEnd {
			return nil, 0, fmt.Errorf("the file has overlapping data at 0x%08x", s.addr)
		}
		copy(out[off:], s.data)
		prevEnd = off + uint64(len(s.data))
	}
	return out, base, nil
}

// parseIntelHex reads an Intel HEX file into a binary and its start address.
func parseIntelHex(raw []byte) ([]byte, uint32, error) {
	var segs []usbSegment
	var upper uint32 // extended segment or linear base
	eof := false
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line[0] != ':' {
			return nil, 0, fmt.Errorf("hex line %d: does not start with ':'", n+1)
		}
		rec, err := hex.DecodeString(line[1:])
		if err != nil || len(rec) < 5 || int(rec[0]) != len(rec)-5 {
			return nil, 0, fmt.Errorf("hex line %d: malformed record", n+1)
		}
		var sum byte
		for _, b := range rec {
			sum += b
		}
		if sum != 0 {
			return nil, 0, fmt.Errorf("hex line %d: bad checksum", n+1)
		}
		count, off, typ := int(rec[0]), uint32(rec[1])<<8|uint32(rec[2]), rec[3]
		data := rec[4 : 4+count]
		switch typ {
		case 0x00:
			segs = append(segs, usbSegment{addr: upper + off, data: append([]byte(nil), data...)})
		case 0x01:
			eof = true
		case 0x02:
			if count != 2 {
				return nil, 0, fmt.Errorf("hex line %d: bad segment record", n+1)
			}
			upper = (uint32(data[0])<<8 | uint32(data[1])) << 4
		case 0x04:
			if count != 2 {
				return nil, 0, fmt.Errorf("hex line %d: bad address record", n+1)
			}
			upper = (uint32(data[0])<<8 | uint32(data[1])) << 16
		case 0x03, 0x05: // start address: not a flash address
		default:
			return nil, 0, fmt.Errorf("hex line %d: unknown record type %02x", n+1, typ)
		}
		if eof {
			break
		}
	}
	if !eof {
		return nil, 0, fmt.Errorf("hex file has no end-of-file record (truncated?)")
	}
	return flattenSegments(segs)
}

// dfuSuffixLen is the 16-byte suffix of a DFU file.
const dfuSuffixLen = 16

// hasDFUSuffix reports whether raw ends in a DFU suffix ("UFD" and its length).
func hasDFUSuffix(raw []byte) bool {
	n := len(raw)
	return n >= dfuSuffixLen && string(raw[n-8:n-5]) == "UFD" && raw[n-5] == dfuSuffixLen
}

// dfuCRC is the DFU file CRC: CRC-32 without the final inversion.
func dfuCRC(b []byte) uint32 { return ^crc32.ChecksumIEEE(b) }

// parseDFUFile reads a .dfu file: a DfuSe container (with its addresses) or a plain DFU payload
// (suffix only, no address). hasAddr says whether the file names a start address.
func parseDFUFile(raw []byte) (data []byte, addr uint32, hasAddr bool, err error) {
	if !hasDFUSuffix(raw) {
		return nil, 0, false, fmt.Errorf("not a DFU file: no DFU suffix at its end")
	}
	n := len(raw)
	if want := binary.LittleEndian.Uint32(raw[n-4:]); dfuCRC(raw[:n-4]) != want {
		return nil, 0, false, fmt.Errorf("DFU file: bad CRC (damaged file?)")
	}
	body := raw[:n-dfuSuffixLen]
	if len(body) < 11 || string(body[:5]) != "DfuSe" {
		return body, 0, false, nil
	}
	targets := int(body[10])
	pos := 11
	var segs []usbSegment
	for t := 0; t < targets; t++ {
		// Target prefix: "Target", alt, named flag, name[255], size, element count (274 bytes).
		if pos+274 > len(body) || string(body[pos:pos+6]) != "Target" {
			return nil, 0, false, fmt.Errorf("DfuSe file: bad target %d", t)
		}
		size := int(binary.LittleEndian.Uint32(body[pos+266:]))
		elems := int(binary.LittleEndian.Uint32(body[pos+270:]))
		pos += 274
		end := pos + size
		if end > len(body) {
			return nil, 0, false, fmt.Errorf("DfuSe file: target %d is longer than the file", t)
		}
		for e := 0; e < elems; e++ {
			if pos+8 > end {
				return nil, 0, false, fmt.Errorf("DfuSe file: bad element in target %d", t)
			}
			a := binary.LittleEndian.Uint32(body[pos:])
			l := int(binary.LittleEndian.Uint32(body[pos+4:]))
			pos += 8
			if pos+l > end {
				return nil, 0, false, fmt.Errorf("DfuSe file: element at 0x%08x is longer than its target", a)
			}
			segs = append(segs, usbSegment{addr: a, data: body[pos : pos+l]})
			pos += l
		}
		pos = end
	}
	data, addr, err = flattenSegments(segs)
	return data, addr, true, err
}

var fat83Char = regexp.MustCompile("[^A-Z0-9!#$%&'()@^_`{}~-]")

// fat83Name turns a file name into an 8.3 name for a drive: upper case, characters FAT allows,
// the base cut to 8 and the extension to 3 characters.
func fat83Name(file string) string {
	base := strings.ToUpper(filepath.Base(file))
	ext := ""
	if i := strings.LastIndex(base, "."); i > 0 {
		base, ext = base[:i], base[i+1:]
	}
	base = fat83Char.ReplaceAllString(base, "")
	ext = fat83Char.ReplaceAllString(ext, "")
	if base == "" {
		base = "FIRMWARE"
	}
	if len(base) > 8 {
		base = base[:8]
	}
	if len(ext) > 3 {
		ext = ext[:3]
	}
	if ext == "" {
		return base
	}
	return base + "." + ext
}

var fat83Valid = regexp.MustCompile("^[A-Z0-9!#$%&'()@^_`{}~-]{1,8}(\\.[A-Z0-9!#$%&'()@^_`{}~-]{1,3})?$")

// validFAT83 says name is an 8.3 name (upper case).
func validFAT83(name string) bool { return fat83Valid.MatchString(name) }

// parseUSBAddress reads a flash address: decimal or 0x hex.
func parseUSBAddress(s string) (uint32, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 0, 32)
	if err != nil {
		return 0, fmt.Errorf("bad address %q (use a number such as 0x08000000)", s)
	}
	return uint32(v), nil
}
