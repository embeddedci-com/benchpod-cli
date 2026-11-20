package serialconsole

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func netResetConsole(reply string) *Console {
	return newConsole(&fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString(line + "\r\n")
		out.WriteString("[eth] link down\r\n")
		out.WriteString(reply)
		out.WriteString("> ")
	}})
}

func TestNetReset(t *testing.T) {
	c := netResetConsole("net-reset eth autonegotiation, link and DHCP again\r\n" +
		"net-reset wifi cleared (pod flash and the ESP32-C3's NVS)\r\n")
	lines, err := c.NetReset(testContext(t))
	want := []string{"net-reset eth autonegotiation, link and DHCP again", "net-reset wifi cleared (pod flash and the ESP32-C3's NVS)"}
	if err != nil || !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines %q, err %v", lines, err)
	}
	c = netResetConsole("  unknown command 'net-reset' (try 'help')\r\n")
	if _, err := c.NetReset(testContext(t)); !errors.Is(err, ErrNoNetReset) {
		t.Fatalf("old firmware: %v", err)
	}
	c = netResetConsole("net-reset error busy: a capture is running\r\n")
	if _, err := c.NetReset(testContext(t)); err == nil || err.Error() != "net-reset: busy: a capture is running" {
		t.Fatalf("refusal: %v", err)
	}
}
