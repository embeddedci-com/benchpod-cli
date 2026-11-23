package main

import (
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/ci"
)

// The UART steps read the board's USB serial port with `port: usb`.

func uartReq(t *testing.T, f *ciFixture) map[string]any {
	t.Helper()
	f.pod.mu.Lock()
	defer f.pod.mu.Unlock()
	for _, r := range f.pod.reqs {
		if r["cmd"] == "uart_proxy_start" {
			return r
		}
	}
	t.Fatalf("no uart_proxy_start in %v", f.pod.reqs)
	return nil
}

func TestCIUARTExpectOnTheUSBSerialPort(t *testing.T) {
	f := newCIFixture(t, `version: 1
suite: s
dut: d
steps:
  - id: console
    uart_expect: {port: usb, dtr: false, rts: true, baud: 921600, until: "APP_OK", timeout: 2s}
`)
	f.pod.featRev = 15
	f.pod.profile = map[string]any{"la_mv": 3300, "efuse": 1} // no uart_rx: the USB port needs no LA pin
	go func() {
		// the DUT prints once the UART is open
		for i := 0; i < 200; i++ {
			f.pod.mu.Lock()
			w := f.pod.uart
			f.pod.mu.Unlock()
			if w != nil {
				_, _ = w.Write([]byte("hello\r\nAPP_OK\r\n"))
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	exit, run := f.run()
	if exit.code != ci.ExitPassed || caseByID(run, "console").Outcome != ci.Passed {
		t.Fatalf("exit %d: %+v\n%s", exit.code, caseByID(run, "console"), f.stderr.String())
	}
	req := uartReq(t, f)
	if req["port"] != "usb" || req["baud"] != 921600 || req["dtr"] != false || req["rts"] != true || req["rx"] != nil || req["tx"] != nil {
		t.Errorf("request %v", req)
	}
}

func TestCIUARTUSBBaudDefaultsToTheProfileAndKeepsPodDefaultLines(t *testing.T) {
	f := newCIFixture(t, `version: 1
suite: s
dut: d
steps:
  - id: boot
    power_cycle: {port: usb, until: "APP_OK", timeout: 2s, off_ms: 10}
`)
	f.pod.featRev = 15
	f.pod.profile["uart_baud"] = 57600
	exit, run := f.run()
	if exit.code != ci.ExitPassed || caseByID(run, "boot").Outcome != ci.Passed {
		t.Fatalf("exit %d: %+v\n%s", exit.code, caseByID(run, "boot"), f.stderr.String())
	}
	req := uartReq(t, f)
	if req["port"] != "usb" || req["baud"] != 57600 || req["dtr"] != nil || req["rts"] != nil {
		t.Errorf("request %v", req)
	}
}

func TestCIUARTUSBNeedsFirmware403(t *testing.T) {
	f := newCIFixture(t, `version: 1
suite: s
dut: d
steps:
  - id: console
    uart_expect: {port: usb, until: "x", timeout: 100ms}
`)
	f.pod.featRev = 14
	exit, run := f.run()
	c := caseByID(run, "console")
	if exit.code == ci.ExitPassed || !strings.Contains(c.Message, "port: usb needs pod firmware 4.0.3") {
		t.Fatalf("exit %d, %+v", exit.code, c)
	}
	for _, r := range f.pod.reqs {
		if r["cmd"] == "uart_proxy_start" {
			t.Error("must not open the UART on an old pod")
		}
	}
}

func TestCIUARTPinsStillNeedTheProfilePins(t *testing.T) {
	f := newCIFixture(t, "version: 1\nsuite: s\ndut: d\nsteps:\n  - id: c\n    uart_expect: {until: x, timeout: 100ms}\n")
	f.pod.profile = map[string]any{"la_mv": 3300}
	_, run := f.run()
	if c := caseByID(run, "c"); !strings.Contains(c.Message, "sets no uart_rx") {
		t.Errorf("%+v", c)
	}
}
