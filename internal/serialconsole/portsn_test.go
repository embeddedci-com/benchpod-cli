package serialconsole

import (
	"testing"

	"go.bug.st/serial/enumerator"
)

// Firmware 3.8 (0483:5740) to 3.9 (1209:0001): the port may be renamed, the serial number stays.
func TestFindPodPortBySerialAcrossUSBIDs(t *testing.T) {
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		return []*enumerator.PortDetails{
			{Name: "/dev/cu.usbmodem3464395E34351", IsUSB: true, VID: "0483", PID: "5740", SerialNumber: "3464395E3435"},
		}, nil
	})
	sn := PortSerialNumber("/dev/tty.usbmodem3464395E34351")
	if sn != "3464395E3435" {
		t.Fatalf("serial number %q", sn)
	}
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		return []*enumerator.PortDetails{
			{Name: "COM3", IsUSB: true, VID: "0483", PID: "374B", SerialNumber: "066DFF"}, // an ST-Link
			{Name: "COM7", IsUSB: true, VID: "1209", PID: "0001", SerialNumber: "3464395e3435"},
		}, nil
	})
	if got := FindPodPortBySerial(sn); got != "COM7" {
		t.Fatalf("found %q, want the pod's new COM7", got)
	}
	if got := FindPodPortBySerial("FFFFFFFFFFFF"); got != "" {
		t.Fatalf("an absent pod was found on %q", got)
	}
}
