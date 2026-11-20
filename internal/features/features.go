// Package features is the BenchPod features table: benchpod-firmware's features/features.json
// (generated from features/features.toml), copied here by scripts/sync-feature-map.sh. It says
// which firmware and gateware have which feature, so the CLI needs no network to decide.
//
// Firmware after 3.10.10 answers {"cmd":"status","caps":false} with features_rev (the table
// revision it is built from), fpga_image and the hw facts instead of the caps list; older
// firmware ignores the argument and sends the list. Has handles both.
package features

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed features.json
var raw []byte

// StatusRequest is the status request a client with this table sends.
func StatusRequest() map[string]any { return map[string]any{"cmd": "status", "caps": false} }

// Entry is one feature of the table.
type Entry struct {
	Rev        int      `json:"rev"`
	Firmware   string   `json:"firmware"`
	Gateware   int      `json:"gateware"`
	Board      string   `json:"board"`
	Requires   []string `json:"requires"`
	LegacyCaps string   `json:"legacy_caps"`
	Status     string   `json:"status"`
}

// Fact is a hardware or runtime fact the pod still announces.
type Fact struct {
	Status string `json:"status"`
	Frame  string `json:"frame"`
	HW     string `json:"hw"`
}

// Table is the parsed features.json.
type Table struct {
	Schema   int `json:"schema"`
	Revision int `json:"revision"`
	// API is the client API level of the firmware the table is from; MinClientAPI the lowest it
	// takes (benchpod-firmware docs/API.md, Client compatibility).
	API          int              `json:"api"`
	MinClientAPI int              `json:"min_client_api"`
	Facts        map[string]Fact  `json:"facts"`
	Features     map[string]Entry `json:"features"`
}

var table = func() Table {
	var t Table
	if err := json.Unmarshal(raw, &t); err != nil || t.Schema != 1 {
		panic(fmt.Sprintf("features: bad embedded features.json: %v", err))
	}
	return t
}()

// Revision is the highest table revision this CLI knows.
func Revision() int { return table.Revision }

// API is the client API level of the table this CLI embeds: the highest pod API it handles.
func API() int { return table.API }

// Lookup returns the entry for a feature name.
func Lookup(name string) (Entry, bool) {
	e, ok := table.Features[name]
	return e, ok
}

type status struct {
	Caps        []string `json:"caps"`
	FeaturesRev int      `json:"features_rev"`
	Gateware    int      `json:"gateware"`
	FPGAImage   string   `json:"fpga_image"`
	HW          []string `json:"hw"`
	Analog      *bool    `json:"analog"`
}

// Has reports whether the pod whose status reply this is has the feature `name` (a table name,
// for example "spi_master", "sensors", "generate16"). With features_rev the table decides;
// without it the reply's caps list does (the entry's legacy name, else the name itself).
func Has(statusReply json.RawMessage, name string) (bool, error) {
	var st status
	if err := json.Unmarshal(statusReply, &st); err != nil {
		return false, fmt.Errorf("decode status: %w", err)
	}
	var top map[string]any
	_ = json.Unmarshal(statusReply, &top)
	if st.FeaturesRev > 0 {
		return resolve(st, top, name), nil
	}
	want := name
	if e, ok := table.Features[name]; ok && e.LegacyCaps != "" {
		want = e.LegacyCaps
	}
	for _, c := range st.Caps {
		if strings.EqualFold(c, want) {
			return true, nil
		}
	}
	return false, nil
}

// PodIsNewer reports whether the pod's firmware has a newer table than this CLI.
func PodIsNewer(statusReply json.RawMessage) bool {
	var st status
	return json.Unmarshal(statusReply, &st) == nil && st.FeaturesRev > table.Revision
}

func resolve(st status, top map[string]any, name string) bool {
	e, ok := table.Features[name]
	if !ok || e.Status == "retired" || e.Rev > st.FeaturesRev {
		return false
	}
	if e.Gateware > 0 && st.Gateware < e.Gateware {
		return false
	}
	analog := st.Analog == nil || *st.Analog
	switch e.Board {
	case "analog":
		if !analog {
			return false
		}
	case "digital":
		if analog {
			return false
		}
	}
	for _, r := range e.Requires {
		if !factHolds(st, top, r) {
			return false
		}
	}
	return true
}

func factHolds(st status, top map[string]any, req string) bool {
	name, value, _ := strings.Cut(req, ":")
	if name == "fpga_image" {
		return st.FPGAImage == value
	}
	f, ok := table.Facts[name]
	if !ok {
		return false
	}
	if f.Status != "" {
		b, _ := top[f.Status].(bool)
		return b
	}
	for _, h := range st.HW {
		if h == f.HW {
			return true
		}
	}
	return false
}
