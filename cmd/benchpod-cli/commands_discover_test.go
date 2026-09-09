package main

import (
	"os"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

func TestCorrelateMatchesSamePodOnBothTransports(t *testing.T) {
	netPods := []discoveredPod{
		{instance: "BenchPod a1b2c3", addr: "192.168.1.213:8080"},
		{instance: "BenchPod d4e5f6", addr: "192.168.1.214:8080"},
	}
	serialPods := []serialconsole.SerialPod{
		{Device: "/dev/cu.usbmodem1103", IP: "192.168.1.213"},
	}
	correlate(netPods, serialPods)

	if netPods[0].sameAs != "/dev/cu.usbmodem1103" {
		t.Fatalf("pod on both transports should be matched, got %q", netPods[0].sameAs)
	}
	if netPods[1].sameAs != "" {
		t.Fatalf("unrelated pod should not be matched, got %q", netPods[1].sameAs)
	}
	// One physical pod on two transports must count once.
	if got := countUnmatched(netPods); got != 1 {
		t.Fatalf("countUnmatched = %d, want 1", got)
	}
}

func TestCorrelateIgnoresUnaddressedSerialPod(t *testing.T) {
	// A pod with no lease reports 0.0.0.0; it must never match a network entry,
	// which would otherwise collapse two distinct pods into one.
	netPods := []discoveredPod{{instance: "BenchPod a1b2c3", addr: "0.0.0.0:8080"}}
	correlate(netPods, []serialconsole.SerialPod{{Device: "/dev/ttyACM0", IP: "0.0.0.0"}})
	if netPods[0].sameAs != "" {
		t.Fatalf("unaddressed pod should not correlate, got %q", netPods[0].sameAs)
	}
}

func TestSaveTargetPrefersNetworkAddress(t *testing.T) {
	got := saveTarget(
		[]discoveredPod{{addr: "192.168.1.213:8080"}},
		[]serialconsole.SerialPod{{Device: "/dev/ttyACM0", IP: "192.168.1.213"}},
	)
	if got != "192.168.1.213:8080" {
		t.Fatalf("saveTarget = %q, want the network address", got)
	}
}

func TestSaveTargetFallsBackToSerialDevice(t *testing.T) {
	got := saveTarget(nil, []serialconsole.SerialPod{{Device: "/dev/ttyACM0"}})
	if got != "/dev/ttyACM0" {
		t.Fatalf("saveTarget = %q, want the serial device", got)
	}
}

func TestSaveTargetRefusesToGuess(t *testing.T) {
	two := []discoveredPod{{addr: "192.168.1.213:8080"}, {addr: "192.168.1.214:8080"}}
	if got := saveTarget(two, nil); got != "" {
		t.Fatalf("saveTarget with two network pods = %q, want empty", got)
	}
	twoSerial := []serialconsole.SerialPod{{Device: "/dev/ttyACM0"}, {Device: "/dev/ttyACM1"}}
	if got := saveTarget(nil, twoSerial); got != "" {
		t.Fatalf("saveTarget with two USB pods = %q, want empty", got)
	}
	if got := saveTarget(nil, nil); got != "" {
		t.Fatalf("saveTarget with nothing = %q, want empty", got)
	}
}

func TestDescribeHealthReportsUnreachableAndUnregistered(t *testing.T) {
	got := describeHealth(discoveredPod{reachable: false, healthErr: "dial tcp: timeout\nmore detail"})
	if !strings.Contains(got, "API unreachable") || !strings.Contains(got, "dial tcp: timeout") {
		t.Fatalf("health = %q", got)
	}
	if strings.Contains(got, "more detail") {
		t.Fatalf("only the first line of the error belongs in a one-line verdict: %q", got)
	}

	got = describeHealth(discoveredPod{
		reachable: true,
		cloud:     cloudState{known: true, Configured: false},
	})
	if !strings.Contains(got, "API ok") || !strings.Contains(got, "not registered") {
		t.Fatalf("health = %q", got)
	}

	got = describeHealth(discoveredPod{
		reachable: true,
		cloud:     cloudState{known: true, Configured: true, State: "CONNECTED"},
		sameAs:    "/dev/ttyACM0",
	})
	for _, want := range []string{"API ok", "registered, cloud connected", "same pod as /dev/ttyACM0"} {
		if !strings.Contains(got, want) {
			t.Fatalf("health %q missing %q", got, want)
		}
	}
}

func TestDescribePodIPNamesTheMissingNetwork(t *testing.T) {
	if got := describePodIP(serialconsole.SerialPod{IP: "192.168.1.213"}); got != "ip 192.168.1.213" {
		t.Fatalf("describePodIP = %q", got)
	}
	for _, ip := range []string{"", "0.0.0.0", "-"} {
		if got := describePodIP(serialconsole.SerialPod{IP: ip}); got != "no network address" {
			t.Fatalf("describePodIP(%q) = %q", ip, got)
		}
	}
}

func TestDescribeBoardFoldsInFirmwareVersion(t *testing.T) {
	if got := describeBoard(serialconsole.SerialPod{Board: "bench-pod", Firmware: "v1.4.2"}); got != "bench-pod  fw v1.4.2" {
		t.Fatalf("describeBoard = %q", got)
	}
	// A firmware too old to print a version still identifies its board.
	if got := describeBoard(serialconsole.SerialPod{Board: "bench-pod"}); got != "bench-pod" {
		t.Fatalf("describeBoard = %q", got)
	}
	if got := describeBoard(serialconsole.SerialPod{}); got != "bench-pod" {
		t.Fatalf("describeBoard = %q", got)
	}
}

// captureRegistrationVerdict renders printRegistrationVerdict to a temp file and returns the text.
// printRegistrationVerdict writes to an *os.File (it is the command's output, not a log), so the
// test gives it a real one rather than reaching for a buffer.
func captureRegistrationVerdict(t *testing.T, in reportInput) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "verdict")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer func() { _ = f.Close() }()
	printRegistrationVerdict(f, in)
	body, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return string(body)
}

// A pod that arrived already set up must be told it is done, not sent to `benchpod register`.
func TestRegistrationVerdict_RegisteredTellsUserNothingToDo(t *testing.T) {
	got := captureRegistrationVerdict(t, reportInput{
		netPods: []discoveredPod{{
			instance:  "BenchPod a1b2c3",
			reachable: true,
			cloud:     cloudState{known: true, Configured: true, State: "connected"},
		}},
	})
	if !strings.Contains(got, "Already registered") {
		t.Fatalf("verdict does not say the pod is registered:\n%s", got)
	}
	if strings.Contains(got, "benchpod register") {
		t.Fatalf("a registered pod must not be sent to `benchpod register`:\n%s", got)
	}
}

func TestRegistrationVerdict_UnregisteredGivesTheTwoCommands(t *testing.T) {
	got := captureRegistrationVerdict(t, reportInput{
		netPods: []discoveredPod{{
			instance:  "BenchPod a1b2c3",
			reachable: true,
			cloud:     cloudState{known: true, Configured: false},
		}},
	})
	for _, want := range []string{"benchpod login", "benchpod register"} {
		if !strings.Contains(got, want) {
			t.Fatalf("verdict is missing %q:\n%s", want, got)
		}
	}
}

// A pod found on USB alone — no network yet — must still report its registration, which is the
// whole point of reading it off the console.
func TestRegistrationVerdict_UsbOnlyPodStillReports(t *testing.T) {
	got := captureRegistrationVerdict(t, reportInput{
		serialPods: []serialconsole.SerialPod{{
			Device:     "/dev/cu.usbmodem1103",
			CloudKnown: true,
			Registered: true,
			CloudState: "waiting-wifi",
		}},
	})
	if !strings.Contains(got, "Already registered") {
		t.Fatalf("a USB-only registered pod was not reported:\n%s", got)
	}
}

// One physical pod seen over both transports must be counted once, not as one registered plus
// one unregistered (which would produce the nonsensical mixed verdict).
func TestRegistrationVerdict_SamePodOnBothTransportsCountsOnce(t *testing.T) {
	got := captureRegistrationVerdict(t, reportInput{
		serialPods: []serialconsole.SerialPod{{
			Device:     "/dev/cu.usbmodem1103",
			CloudKnown: true,
			Registered: true,
			CloudState: "connected",
		}},
		netPods: []discoveredPod{{
			instance:  "BenchPod a1b2c3",
			reachable: true,
			sameAs:    "/dev/cu.usbmodem1103",
			cloud:     cloudState{known: true, Configured: true, State: "connected"},
		}},
	})
	if !strings.Contains(got, "Already registered") {
		t.Fatalf("expected a single registered verdict:\n%s", got)
	}
	if strings.Contains(got, "1 registered, 1 not") {
		t.Fatalf("one physical pod was counted twice:\n%s", got)
	}
}

// Firmware too old to report registration must produce no verdict at all — a guess here would
// send somebody to re-register a working pod.
func TestRegistrationVerdict_UnknownStaysSilent(t *testing.T) {
	got := captureRegistrationVerdict(t, reportInput{
		serialPods: []serialconsole.SerialPod{{Device: "/dev/ttyACM0"}},
		netPods:    []discoveredPod{{instance: "BenchPod a1b2c3", reachable: true}},
	})
	if strings.TrimSpace(got) != "" {
		t.Fatalf("expected no verdict when registration is unknown, got:\n%s", got)
	}
}

func TestDescribeSerialRegistration(t *testing.T) {
	tests := []struct {
		name string
		pod  serialconsole.SerialPod
		want string
	}{
		{"unknown", serialconsole.SerialPod{}, ""},
		{"not registered", serialconsole.SerialPod{CloudKnown: true}, "not registered"},
		{
			"registered with state",
			serialconsole.SerialPod{CloudKnown: true, Registered: true, CloudState: "connected"},
			"registered, cloud connected",
		},
		{
			"registered without state",
			serialconsole.SerialPod{CloudKnown: true, Registered: true},
			"registered",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := describeSerialRegistration(tt.pod); got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}
