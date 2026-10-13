package serialconsole

import (
	"path/filepath"
	"strings"
)

// The pod's USB serial number (12 hex digits from the MCU's unique ID) is the same on every
// firmware, while the console's port name may change (a new COM port number on Windows when 3.9.0
// adds the network adapter; pre-release 3.9.0 builds also had another USB ID, 1209:0001). After a firmware update the
// serial number is how the same pod is found again.

// PortSerialNumber is the USB serial number of the port at path ("" when the enumerator does not
// know it). macOS's /dev/tty.* twin of a /dev/cu.* port matches too.
func PortSerialNumber(path string) string {
	ports, err := portLister()
	if err != nil {
		return ""
	}
	want := twinName(path)
	for _, p := range ports {
		if p != nil && p.IsUSB && twinName(p.Name) == want {
			return strings.TrimSpace(p.SerialNumber)
		}
	}
	return ""
}

// FindPodPortBySerial is the console port of the pod with USB serial number sn, under any pod
// USB ID, or "" when it is not (yet) enumerated.
func FindPodPortBySerial(sn string) string {
	sn = strings.TrimSpace(sn)
	if sn == "" {
		return ""
	}
	ports, err := portLister()
	if err != nil {
		return ""
	}
	for _, p := range ports {
		if p != nil && p.IsUSB && isBenchPodVID(p.VID) && strings.EqualFold(strings.TrimSpace(p.SerialNumber), sn) {
			return p.Name
		}
	}
	return ""
}

// twinName folds macOS's /dev/tty.X and /dev/cu.X into one key.
func twinName(path string) string {
	b := filepath.Base(path)
	for _, pre := range []string{"tty.", "cu."} {
		b = strings.TrimPrefix(b, pre)
	}
	return b
}
