package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBridgeServiceArgs(t *testing.T) {
	got := strings.Join(bridgeServiceArgs("/opt/homebrew/bin/benchpod", "usb:benchpod-baea06", 3129), " ")
	if got != "/opt/homebrew/bin/benchpod usb-bridge --watch --connection usb:benchpod-baea06 --port 3129" {
		t.Fatalf("args = %q", got)
	}
	if got := bridgeServiceArgs("/b", "", usbBridgeDefaultPort); len(got) != 3 {
		t.Fatalf("defaults add no flags: %v", got)
	}
}

func TestServiceDefinitions(t *testing.T) {
	args := []string{"/Applications/My Tools/benchpod", "usb-bridge", "--watch"}
	plist := launchdPlist(args, "/Users/me/Library/Logs/b.log")
	for _, want := range []string{"<string>" + usbBridgeLabel + "</string>", "<string>/Applications/My Tools/benchpod</string>",
		"<string>--watch</string>", "<key>KeepAlive</key>", "<key>RunAtLoad</key>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	unit := systemdUnit(args)
	if !strings.Contains(unit, `ExecStart="/Applications/My Tools/benchpod" "usb-bridge" "--watch"`) || !strings.Contains(unit, "Restart=always") {
		t.Errorf("unit:\n%s", unit)
	}
	if got := windowsTaskRun([]string{`C:\Program Files\benchpod.exe`, "usb-bridge", "--watch"}); got != `conhost.exe --headless "C:\Program Files\benchpod.exe" usb-bridge --watch` {
		t.Errorf("task run = %q", got)
	}
}

func TestInstallBridgeServiceLinux(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var ran []string
	oldRun, oldOS := runCommand, serviceOS
	runCommand = func(name string, args ...string) (string, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		if len(args) > 1 && args[1] == "is-active" {
			return "active", nil
		}
		return "", nil
	}
	serviceOS = "linux"
	t.Cleanup(func() { runCommand, serviceOS = oldRun, oldOS })

	if _, err := installBridgeService([]string{"/usr/bin/benchpod", "usb-bridge", "--watch"}); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(home, ".config", "systemd", "user", usbBridgeUnit)
	if _, err := os.Stat(unit); err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	if !strings.Contains(strings.Join(ran, "\n"), "systemctl --user enable --now "+usbBridgeUnit) {
		t.Fatalf("ran %v", ran)
	}
	installed, running, _, err := bridgeServiceStatus()
	if err != nil || !installed || !running {
		t.Fatalf("status installed=%v running=%v err=%v", installed, running, err)
	}
	if _, err := uninstallBridgeService(); err != nil {
		t.Fatal(err)
	}
	if installed, _, _, _ := bridgeServiceStatus(); installed {
		t.Fatal("still installed after uninstall")
	}
}
