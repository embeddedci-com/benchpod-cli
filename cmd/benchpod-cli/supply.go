package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/embeddedci-com/benchpod-cli/internal/features"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// The target power outputs ("supplies"): "5v" (the pod's 5 V through eFuse 1), "external" (the
// user's supply through eFuse 2), and on a BenchPod v4 the switched "3v3" and "1v8" of J1.
// Profiles and firmware before the supply key send efuse 1/2 instead.

func isSupply(s string) bool {
	switch s {
	case "5v", "external", "3v3", "1v8":
		return true
	}
	return false
}

// supplyLabel is a supply as the output shows it.
func supplyLabel(s string) string {
	switch s {
	case "5v":
		return "5 V"
	case "3v3":
		return "3.3 V"
	case "1v8":
		return "1.8 V"
	}
	return s
}

// legacySupply maps the efuse number of older profiles to a supply ("" for anything else).
func legacySupply(efuse int) string {
	switch efuse {
	case 1:
		return "5v"
	case 2:
		return "external"
	}
	return ""
}

// supplyEfuse is the eFuse number of a metered supply (0 for 3v3 and 1v8, which have none).
func supplyEfuse(s string) int {
	switch s {
	case "5v":
		return 1
	case "external":
		return 2
	}
	return 0
}

// supplyKeyRevFallback is the features table revision that introduced the `supply` key, used
// when the embedded table does not know the supply_3v3_1v8 feature that came with it.
const supplyKeyRevFallback = 22

// supplyCaps is what a pod's firmware does with power commands.
type supplyCaps struct {
	Key      bool // reads the supply key (features table revision of 4.0.7); else only efuse 1/2
	Switched bool // has the switched 3v3 and 1v8 supplies (feature supply_3v3_1v8, BenchPod v4)
}

func podSupplyCaps(statusReply json.RawMessage) supplyCaps {
	rev := supplyKeyRevFallback
	if e, ok := features.Lookup("supply_3v3_1v8"); ok {
		rev = e.Rev
	}
	sw, err := features.Has(statusReply, "supply_3v3_1v8")
	return supplyCaps{Key: features.PodRevisionAtLeast(statusReply, rev), Switched: err == nil && sw}
}

// supplyRequest sets the supply of a power command: the supply key for firmware that reads it,
// else efuse 1/2. 3v3 and 1v8 need a pod that has them.
func supplyRequest(req map[string]any, supply string, caps supplyCaps) error {
	n := supplyEfuse(supply)
	if n == 0 && !caps.Switched {
		if !caps.Key {
			return fmt.Errorf("the %s supply needs pod firmware 4.0.7 or later on a BenchPod v4 (update the pod with `benchpod flash-self`)", supplyLabel(supply))
		}
		return fmt.Errorf("this pod has no %s supply (only the BenchPod v4 does)", supplyLabel(supply))
	}
	if caps.Key {
		req["supply"] = supply
		return nil
	}
	req["efuse"] = n
	return nil
}

// supplyState is one supply in target_status.
type supplyState struct {
	Enabled flexBool `json:"enabled"`
	Fault   flexBool `json:"fault"`
}

// supplyState is the state of a supply from target_status: the supplies map, else the
// efuse1/efuse2 keys of older firmware. ok is false when the pod does not report that supply.
func (p targetPower) supplyState(name string) (supplyState, bool) {
	if s, ok := p.Supplies[name]; ok {
		return s, true
	}
	switch name {
	case "5v":
		return supplyState(p.Efuse1), true
	case "external":
		return supplyState(p.Efuse2), true
	}
	return supplyState{}, false
}

// supplyPower is one supply in power_status.
type supplyPower = powerRail

// supplyPower is the monitor reading of a supply: the supplies map, else the internal/external
// keys of older firmware.
func (d powerDraw) supplyPower(name string) supplyPower {
	if s, ok := d.Supplies[name]; ok {
		return s
	}
	switch name {
	case "5v":
		return d.Internal
	case "external":
		return d.External
	}
	return supplyPower{}
}

// supplyIsOn reads target_status for one supply.
func supplyIsOn(ctx context.Context, pod ciPod, name string) bool {
	raw, err := pod.Command(ctx, map[string]any{"cmd": "target_status"})
	if err != nil {
		return false
	}
	var tp targetPower
	if json.Unmarshal(raw, &tp) != nil {
		return false
	}
	s, ok := tp.supplyState(name)
	return ok && bool(s.Enabled)
}

// switchSupplyOn switches a supply on over a direct connection: supply for a pod that has the
// supply feature, else efuse 1/2.
func switchSupplyOn(ctx context.Context, c *tcpclient.Client, supply string) error {
	var caps supplyCaps
	if raw, err := c.Command(ctx, features.StatusRequest()); err == nil {
		caps = podSupplyCaps(raw)
	}
	req := map[string]any{"cmd": "target_power", "enabled": true}
	if err := supplyRequest(req, supply, caps); err != nil {
		return err
	}
	_, err := c.Command(ctx, req)
	return err
}
