package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
)

// busMastersSplitMinGateware is the first gateware whose emulation image has no bus masters
// (SWD, JTAG, the SPI master, the I2C master). Up to v64 every image had them.
const busMastersSplitMinGateware = 65

// fpgaFeatureBusMasters is the FPGA_FEATURES bit set by the images that carry the bus masters.
const fpgaFeatureBusMasters = 0x20

// ensureBusMasterImage runs before an operation that needs a bus master (SWD or JTAG flash,
// the SPI master, the I2C master, the GPIO expanders). From gateware v65 the emulation image has
// none: with no digital sensor running it switches the pod to the loop image (the firmware does
// the same on its own; this gives a clear log line and works with older dev firmware), and with
// a digital sensor running it fails, since leaving the emulation image would stop the sensor.
// feature names what needs the image, for example "SWD" or "the I2C master".
func ensureBusMasterImage(ctx context.Context, c podCommander, feature string) error {
	raw, err := c.Command(ctx, map[string]any{"cmd": "fpga_image"})
	if err != nil {
		return nil // an older pod without fpga_image: every image had the bus masters
	}
	var cur struct {
		Name     string `json:"name"`
		Version  int    `json:"version"`
		Features *int   `json:"features"`
	}
	if json.Unmarshal(raw, &cur) != nil || cur.Name != "emulation" && cur.Name != "emu" {
		return nil
	}
	if cur.Version < busMastersSplitMinGateware {
		return nil
	}
	if cur.Features != nil && *cur.Features&fpgaFeatureBusMasters != 0 {
		return nil
	}
	if running := digitalSensorsRunning(ctx, c); len(running) > 0 {
		return fmt.Errorf("%s is not in the emulation FPGA image, and the digital sensors %v are running. "+
			"Stop them first with `benchpod sensor stop`, or run this before `benchpod sensor start`", feature, running)
	}
	log.Printf("%s needs the loop FPGA image: switching the FPGA image to loop (~3 s)", feature)
	if _, err := c.Command(ctx, map[string]any{"cmd": "fpga_image", "image": "loop"}); err != nil {
		return fmt.Errorf("switch the FPGA image to loop for %s: %w", feature, err)
	}
	return nil
}

// digitalSensorsRunning lists the running I2C, SPI and UART sensor instances (sensor_status).
// A pod that cannot answer has none.
func digitalSensorsRunning(ctx context.Context, c podCommander) []string {
	var names []string
	first := 0
	for page := 0; page < 20; page++ {
		req := map[string]any{"cmd": "sensor_status"}
		if first > 0 {
			req["first"] = first
		}
		raw, err := c.Command(ctx, req)
		if err != nil {
			return names
		}
		var st struct {
			Instances []struct {
				Instance string `json:"instance"`
				Kind     string `json:"kind"`
				Running  bool   `json:"running"`
			} `json:"instances"`
			Next int `json:"next"`
		}
		if json.Unmarshal(raw, &st) != nil {
			return names
		}
		for _, in := range st.Instances {
			if in.Running && (in.Kind == "i2c" || in.Kind == "spi" || in.Kind == "uart") {
				names = append(names, in.Instance)
			}
		}
		if st.Next <= 0 || st.Next == first {
			break
		}
		first = st.Next
	}
	return names
}
