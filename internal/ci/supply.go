package ci

import "strings"

// NormalizeSupply reads a target power supply: "5v", "external", "3v3" or "1v8". The older
// values (1 and internal for 5v, 2 for external) are accepted too; legacy says one was used
// (external is also a current name, so it is never legacy). ok is false for anything else.
func NormalizeSupply(s string) (supply string, legacy, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "5v":
		return "5v", false, true
	case "external":
		return "external", false, true
	case "3v3":
		return "3v3", false, true
	case "1v8":
		return "1v8", false, true
	case "1", "internal":
		return "5v", true, true
	case "2":
		return "external", true, true
	}
	return "", false, false
}

// SupplyValues is the list of supplies for messages.
const SupplyValues = "5v, external, 3v3 or 1v8"
