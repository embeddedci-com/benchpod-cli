package sensors

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	// MaxCommandBytes is the largest JSON command a pod takes over the cloud.
	MaxCommandBytes = 1280
	// CommandReserveBytes is kept free for the transport's envelope around a command.
	CommandReserveBytes = 128
	// MaxChunkBytes is the most raw bytes per upload chunk: 1024 base64 characters, a command
	// of at most 1152 bytes (the size the firmware documents).
	MaxChunkBytes = 768
	// MaxLABytes is the most packed bytes one sensor_la capture takes (4 samples per byte).
	MaxLABytes = 4096
)

// ChunkSize is the most raw bytes per chunk (a multiple of 3, so only the last chunk is padded)
// for which base plus "data_b64" stays within limit bytes of JSON.
func ChunkSize(base map[string]any, limit int) (int, error) {
	probe := make(map[string]any, len(base)+1)
	for k, v := range base {
		probe[k] = v
	}
	probe["data_b64"] = ""
	b, err := json.Marshal(probe)
	if err != nil {
		return 0, err
	}
	room := limit - len(b)
	if room < 4 {
		return 0, fmt.Errorf("the command leaves no room for data")
	}
	return min(room/4*3, MaxChunkBytes), nil
}

// UploadRequests are the sensor_define or sensor_stimulus commands that carry data, in order.
// instance is "" for a stimulus; dryRun goes on the last chunk.
func UploadRequests(cmd string, data []byte, instance string, dryRun bool) ([]map[string]any, error) {
	total := len(data)
	base := map[string]any{"cmd": cmd}
	if instance != "" {
		base["instance"] = instance
	}
	probe := map[string]any{"offset": total, "total": total}
	for k, v := range base {
		probe[k] = v
	}
	if dryRun {
		probe["dry_run"] = true
	}
	size, err := ChunkSize(probe, MaxCommandBytes-CommandReserveBytes)
	if err != nil {
		return nil, err
	}
	var reqs []map[string]any
	for offset := 0; ; {
		end := offset + size
		if end > total {
			end = total
		}
		req := map[string]any{"offset": offset, "total": total,
			"data_b64": base64.StdEncoding.EncodeToString(data[offset:end])}
		for k, v := range base {
			req[k] = v
		}
		offset = end
		last := offset >= total
		if last && dryRun {
			req["dry_run"] = true
		}
		reqs = append(reqs, req)
		if last {
			return reqs, nil
		}
	}
}

// PinRoles are the pin roles per kind.
var PinRoles = map[string][]string{
	"i2c":    {"sda", "scl", "drdy"},
	"spi":    {"sclk", "mosi", "miso", "cs"}, // the emulated SPI target has no data-ready pin
	"uart":   {"tx", "rx"},
	"analog": {"output"},
}

// RequiredPins are the roles a kind cannot start without.
var RequiredPins = map[string][]string{
	"i2c":    {"sda", "scl"},
	"spi":    {"sclk", "mosi", "miso", "cs"},
	"uart":   {"tx"},
	"analog": {"output"},
}

// PinValue is an LA channel (1-14, also "la4", "pin4") as a number, or another word (a wiring
// name) as text. The pod takes channels only: the caller resolves a wiring name (ResolvePins).
func PinValue(role, v string) (any, error) {
	s := strings.TrimSpace(v)
	if role == "output" {
		switch strings.ToLower(s) {
		case "dac", "current":
			return strings.ToLower(s), nil
		}
		return nil, fmt.Errorf("an analog sensor's output is dac or current, got %q", v)
	}
	low := strings.ToLower(s)
	for _, p := range []string{"la", "pin"} {
		low = strings.TrimPrefix(low, p)
	}
	if n, err := strconv.Atoi(low); err == nil {
		if n < 1 || n > 14 {
			return nil, fmt.Errorf("pin %s: LA channels are 1-14, got %q", role, v)
		}
		return n, nil
	}
	if s == "" {
		return nil, fmt.Errorf("pin %s: empty", role)
	}
	return s, nil
}

// Pins checks roles against kind: unknown roles and missing required ones are errors.
func Pins(kind string, pins map[string]any) error {
	roles, ok := PinRoles[kind]
	if !ok {
		return fmt.Errorf("unknown sensor kind %q", kind)
	}
	for r := range pins {
		if kind == "spi" && r == "drdy" {
			return fmt.Errorf("the emulated SPI target has no data-ready pin")
		}
		if !contains(roles, r) {
			return fmt.Errorf("a %s sensor takes pins %s, not %s", kind, strings.Join(roles, ", "), r)
		}
	}
	var missing []string
	for _, r := range RequiredPins[kind] {
		if _, ok := pins[r]; !ok {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("a %s sensor needs pins %s (--pin %s=<LA channel>)", kind, strings.Join(missing, ", "), missing[0])
	}
	return nil
}

// ResolvePins replaces the wiring names in pins with their LA channels (lookup gives a name's
// channel). A name lookup does not know is an error: the pod takes channels only.
func ResolvePins(pins map[string]any, lookup func(name string) (int, bool)) error {
	for role, v := range pins {
		name, ok := v.(string)
		if !ok || role == "output" {
			continue
		}
		la, found := 0, false
		if lookup != nil {
			la, found = lookup(name)
		}
		if !found {
			return fmt.Errorf("pin %s: %q is not an LA channel (1-14) nor a name in the wiring profile", role, name)
		}
		pins[role] = la
	}
	return nil
}

// Address is an I2C address ("0x77", "119") as the pod takes it: "0x77".
func Address(v string) (string, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 0, 32)
	if err != nil || n < 0 || n > 0x3FF {
		return "", fmt.Errorf("an I2C address is 0x00..0x3FF, got %q", v)
	}
	return fmt.Sprintf("0x%02X", n), nil
}

// Values parses key=value pairs into param values.
func Values(pairs []string) (map[string]any, error) {
	out := map[string]any{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("a value is key=number, got %q", p)
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return nil, fmt.Errorf("%s: not a number: %q", k, v)
		}
		out[k] = f
	}
	return out, nil
}

// HasCapability reports whether a status reply lists cap in its caps.
func HasCapability(status json.RawMessage, cap string) bool {
	var s struct {
		Caps []string `json:"caps"`
	}
	if json.Unmarshal(status, &s) != nil {
		return false
	}
	for _, c := range s.Caps {
		if strings.EqualFold(c, cap) {
			return true
		}
	}
	return false
}
