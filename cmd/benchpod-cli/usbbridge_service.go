package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The usb-bridge background service: `usb-bridge --watch` started at login, per user, so a pod
// that has only USB keeps its cloud link (and so the web app) after this computer restarts.
//
//	macOS    a launchd agent:     ~/Library/LaunchAgents/com.embeddedci.benchpod-usb-bridge.plist
//	Linux    a systemd user unit: ~/.config/systemd/user/benchpod-usb-bridge.service
//	Windows  a scheduled task "BenchPod usb-bridge" that runs at logon
//
// Only the --watch mode runs there: it waits for the pod and picks it up again after an unplug.

const (
	usbBridgeLabel    = "com.embeddedci.benchpod-usb-bridge"
	usbBridgeUnit     = "benchpod-usb-bridge.service"
	usbBridgeTaskName = "BenchPod usb-bridge"
)

// runCommand runs a service manager command. A variable so tests can replace it.
var runCommand = func(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// serviceOS is runtime.GOOS; a variable for tests.
var serviceOS = runtime.GOOS

// bridgeServiceArgs is the command line the service runs.
func bridgeServiceArgs(exe, conn string, port int) []string {
	args := []string{exe, "usb-bridge", "--watch"}
	if conn != "" {
		args = append(args, "--connection", conn)
	}
	if port != usbBridgeDefaultPort {
		args = append(args, "--port", strconv.Itoa(port))
	}
	return args
}

// benchpodExecutable is the path the service starts: the one on PATH when it is this binary
// (Homebrew's link survives upgrades; the Cellar path behind it does not), else this binary.
func benchpodExecutable() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if onPath, err := exec.LookPath("benchpod"); err == nil {
		a, _ := filepath.EvalSymlinks(onPath)
		b, _ := filepath.EvalSymlinks(self)
		if a != "" && a == b {
			return filepath.Abs(onPath)
		}
	}
	return self, nil
}

func launchdPlist(args []string, logPath string) string {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + usbBridgeLabel + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range args {
		b.WriteString("\t\t<string>" + html.EscapeString(a) + "</string>\n")
	}
	b.WriteString(`	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>` + html.EscapeString(logPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + html.EscapeString(logPath) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func systemdUnit(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = strconv.Quote(a)
	}
	return `[Unit]
Description=BenchPod usb-bridge: the pod's cloud link over USB through this computer
After=network-online.target

[Service]
ExecStart=` + strings.Join(quoted, " ") + `
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`
}

// windowsTaskRun is the /TR of the scheduled task: conhost --headless keeps a console window
// from opening at every logon (Windows 10 1809 and later).
func windowsTaskRun(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t") {
			a = `"` + a + `"`
		}
		quoted[i] = a
	}
	return "conhost.exe --headless " + strings.Join(quoted, " ")
}

func homePath(parts ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, parts...)...), nil
}

func installBridgeService(args []string) (string, error) {
	switch serviceOS {
	case "darwin":
		plist, err := homePath("Library", "LaunchAgents", usbBridgeLabel+".plist")
		if err != nil {
			return "", err
		}
		logPath, err := homePath("Library", "Logs", "benchpod-usb-bridge.log")
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
			return "", err
		}
		_, _ = runCommand("launchctl", "bootout", launchdTarget()) // replace an older install
		if err := os.WriteFile(plist, []byte(launchdPlist(args, logPath)), 0o644); err != nil {
			return "", err
		}
		if out, err := runCommand("launchctl", "bootstrap", launchdDomain(), plist); err != nil {
			return "", fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
		}
		return fmt.Sprintf("installed the launchd agent %s (log: %s)", plist, logPath), nil
	case "linux":
		unit, err := homePath(".config", "systemd", "user", usbBridgeUnit)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(unit, []byte(systemdUnit(args)), 0o644); err != nil {
			return "", err
		}
		if out, err := runCommand("systemctl", "--user", "daemon-reload"); err != nil {
			return "", fmt.Errorf("systemctl --user daemon-reload: %v: %s", err, out)
		}
		if out, err := runCommand("systemctl", "--user", "enable", "--now", usbBridgeUnit); err != nil {
			return "", fmt.Errorf("systemctl --user enable --now: %v: %s", err, out)
		}
		return fmt.Sprintf("installed the systemd user unit %s (log: journalctl --user -u %s). "+
			"To keep it running while you are logged out: loginctl enable-linger", unit, usbBridgeUnit), nil
	case "windows":
		if out, err := runCommand("schtasks", "/Create", "/TN", usbBridgeTaskName, "/TR", windowsTaskRun(args),
			"/SC", "ONLOGON", "/RL", "LIMITED", "/F"); err != nil {
			return "", fmt.Errorf("schtasks /Create: %v: %s", err, out)
		}
		if out, err := runCommand("schtasks", "/Run", "/TN", usbBridgeTaskName); err != nil {
			return "", fmt.Errorf("schtasks /Run: %v: %s", err, out)
		}
		return fmt.Sprintf("installed the scheduled task %q (runs at logon). Windows Defender Firewall must let "+
			"the pod in: allow benchpod when it asks, or as administrator: netsh advfirewall firewall add rule "+
			"name=\"BenchPod usb-bridge\" dir=in action=allow protocol=TCP localport=%d remoteip=10.237.0.0/16",
			usbBridgeTaskName, usbBridgeDefaultPort), nil
	}
	return "", fmt.Errorf("usb-bridge install is not supported on %s; run `benchpod usb-bridge --watch` yourself", serviceOS)
}

func uninstallBridgeService() (string, error) {
	switch serviceOS {
	case "darwin":
		plist, err := homePath("Library", "LaunchAgents", usbBridgeLabel+".plist")
		if err != nil {
			return "", err
		}
		_, _ = runCommand("launchctl", "bootout", launchdTarget())
		if err := os.Remove(plist); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "removed the launchd agent", nil
	case "linux":
		unit, err := homePath(".config", "systemd", "user", usbBridgeUnit)
		if err != nil {
			return "", err
		}
		_, _ = runCommand("systemctl", "--user", "disable", "--now", usbBridgeUnit)
		if err := os.Remove(unit); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		_, _ = runCommand("systemctl", "--user", "daemon-reload")
		return "removed the systemd user unit", nil
	case "windows":
		_, _ = runCommand("schtasks", "/End", "/TN", usbBridgeTaskName)
		if out, err := runCommand("schtasks", "/Delete", "/TN", usbBridgeTaskName, "/F"); err != nil &&
			!strings.Contains(strings.ToLower(out), "cannot find") {
			return "", fmt.Errorf("schtasks /Delete: %v: %s", err, out)
		}
		return "removed the scheduled task", nil
	}
	return "", fmt.Errorf("usb-bridge uninstall is not supported on %s", serviceOS)
}

// bridgeServiceStatus says whether the service is installed and running.
func bridgeServiceStatus() (installed, running bool, detail string, err error) {
	switch serviceOS {
	case "darwin":
		plist, err := homePath("Library", "LaunchAgents", usbBridgeLabel+".plist")
		if err != nil {
			return false, false, "", err
		}
		if _, err := os.Stat(plist); err != nil {
			return false, false, "", nil
		}
		out, err := runCommand("launchctl", "print", launchdTarget())
		return true, err == nil && strings.Contains(out, "state = running"), plist, nil
	case "linux":
		unit, err := homePath(".config", "systemd", "user", usbBridgeUnit)
		if err != nil {
			return false, false, "", err
		}
		if _, err := os.Stat(unit); err != nil {
			return false, false, "", nil
		}
		out, _ := runCommand("systemctl", "--user", "is-active", usbBridgeUnit)
		return true, out == "active", unit, nil
	case "windows":
		out, err := runCommand("schtasks", "/Query", "/TN", usbBridgeTaskName, "/FO", "LIST")
		if err != nil {
			return false, false, "", nil
		}
		return true, strings.Contains(out, "Running"), usbBridgeTaskName, nil
	}
	return false, false, "", fmt.Errorf("usb-bridge status is not supported on %s", serviceOS)
}

func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }
func launchdTarget() string { return launchdDomain() + "/" + usbBridgeLabel }

func newUSBBridgeInstallCmd(g *globalFlags) *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Run usb-bridge in the background at every login (this user)",
		Long: "Install `benchpod usb-bridge --watch` as a background service for this user: a launchd\n" +
			"agent on macOS, a systemd user unit on Linux, a scheduled task at logon on Windows.\n" +
			"It waits for the pod on USB, carries its cloud link while it is plugged in, and comes\n" +
			"back after a restart. --connection is passed on (usb:<name> for one of several pods).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if port <= 0 || port > 65535 {
				return fmt.Errorf("usb-bridge install: --port %d is out of range", port)
			}
			exe, err := benchpodExecutable()
			if err != nil {
				return fmt.Errorf("usb-bridge install: %w", err)
			}
			msg, err := installBridgeService(bridgeServiceArgs(exe, strings.TrimSpace(g.connection), port))
			if err != nil {
				return fmt.Errorf("usb-bridge install: %w", err)
			}
			fmt.Fprintln(os.Stderr, "usb-bridge: "+msg)
			return nil
		},
	}
	cmd.Flags().IntVar(&port, "port", usbBridgeDefaultPort, "TCP port of the proxy on this computer's USB address")
	return cmd
}

func newUSBBridgeUninstallCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the usb-bridge background service and clear the pod's cloud proxy",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			msg, err := uninstallBridgeService()
			if err != nil {
				return fmt.Errorf("usb-bridge uninstall: %w", err)
			}
			fmt.Fprintln(os.Stderr, "usb-bridge: "+msg)
			// The stopped service may not have cleared the proxy (the pod unplugged, or killed).
			time.Sleep(time.Second)
			if err := clearBridgeProxy(g); err != nil {
				fmt.Fprintf(os.Stderr, "usb-bridge: could not clear the pod's cloud proxy (%v); "+
					"run `benchpod cloud proxy clear --connection usb` with the pod plugged in\n", err)
			}
			return nil
		},
	}
}

// clearBridgeProxy clears the pod's cloud proxy over its USB network link when it points at this
// computer's end of that link (never the console).
func clearBridgeProxy(g *globalFlags) error {
	links := localUSBLinks()
	if len(links) == 0 {
		return errors.New("no pod on a USB network link")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	link, err := pickUSBLink(ctx, links, g.usbPodName())
	if err != nil {
		return err
	}
	raw, err := bridgePodAPI(ctx, net.JoinHostPort(link.pod, "8080"), map[string]any{"cmd": "cloud_proxy"})
	if err != nil {
		return err
	}
	var cur struct {
		Host string `json:"host"`
	}
	_ = json.Unmarshal(raw, &cur)
	if cur.Host != link.host {
		return nil // not ours
	}
	if err := bridgeSetProxy(link.pod, ""); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "usb-bridge: cleared the pod's cloud proxy")
	return nil
}

func newUSBBridgeStatusCmd(_ *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the usb-bridge background service is installed and running",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			installed, running, detail, err := bridgeServiceStatus()
			if err != nil {
				return fmt.Errorf("usb-bridge status: %w", err)
			}
			switch {
			case !installed:
				fmt.Println("usb-bridge: not installed (`benchpod usb-bridge install` installs it)")
			case running:
				fmt.Printf("usb-bridge: installed and running (%s)\n", detail)
			default:
				fmt.Printf("usb-bridge: installed, not running (%s)\n", detail)
			}
			return nil
		},
	}
}
