package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/embeddedci-com/benchpod-cli/internal/features"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// USB host mode (firmware 4.0.3+, feature usb_host_mode): the pod hosts a board's USB port for a
// while and reports the device that enumerates on it. `usb-flash` (usbflash.go) flashes that board.
// The report types and the feature check are shared with the other usb_* commands.

const (
	usbHostNeedsFirmware = "needs firmware 4.0.3 or later: update the pod with `benchpod flash-self`"
	usbHostDefaultTime   = 60 * time.Second
)

var usbHostPorts = []string{"usb_a", "usb_c"}

type usbInterface struct {
	Number    int    `json:"number"`
	Alt       int    `json:"alt"`
	Class     int    `json:"class"`
	ClassName string `json:"class_name"`
	Subclass  int    `json:"subclass"`
	Protocol  int    `json:"protocol"`
	Endpoints int    `json:"endpoints"`
}

type usbConfig struct {
	Value          int            `json:"value"`
	MaxPowerMA     int            `json:"max_power_ma"`
	SelfPowered    bool           `json:"self_powered"`
	RemoteWakeup   bool           `json:"remote_wakeup"`
	InterfaceCount int            `json:"interface_count"`
	Truncated      bool           `json:"truncated"`
	Malformed      bool           `json:"malformed"`
	Interfaces     []usbInterface `json:"interfaces"`
}

type usbDevice struct {
	VID          int        `json:"vid"`
	PID          int        `json:"pid"`
	Speed        string     `json:"speed"`
	USB          string     `json:"usb"`
	Release      string     `json:"release"`
	Class        int        `json:"class"`
	Subclass     int        `json:"subclass"`
	Protocol     int        `json:"protocol"`
	Manufacturer string     `json:"manufacturer"`
	Product      string     `json:"product"`
	Serial       string     `json:"serial"`
	DescriptorMs int        `json:"descriptor_ms"`
	MountedMs    int        `json:"mounted_ms"`
	Config       *usbConfig `json:"config"`
}

// usbHostReport is the usb_host reply.
type usbHostReport struct {
	Mode         string     `json:"mode"`
	Port         string     `json:"port"`
	RemainingS   int        `json:"remaining_s"`
	HostMs       int        `json:"host_ms"`
	Enumerations int        `json:"enumerations"`
	Attached     bool       `json:"attached"`
	Enumeration  string     `json:"enumeration"`
	Connected    bool       `json:"connected"`
	LineSpeed    string     `json:"line_speed"`
	Device       *usbDevice `json:"device"`
}

func (d *usbDevice) ids() string { return fmt.Sprintf("%04X:%04X", d.VID, d.PID) }

// hasInterface reports whether the device's first configuration has an interface of the class
// (and, when subclass >= 0, the subclass).
func (d *usbDevice) hasInterface(class, subclass int) bool {
	if d == nil || d.Config == nil {
		return false
	}
	for _, i := range d.Config.Interfaces {
		if i.Class == class && (subclass < 0 || i.Subclass == subclass) {
			return true
		}
	}
	return false
}

// podInfo is what the status reply tells the usb_* commands.
type podInfo struct {
	Version  string `json:"version"`
	BoardRev string `json:"board_rev"`
}

// requireUSBFeatures reads the pod's status and refuses with the firmware hint when it lacks one
// of the features (older firmware answers the usb_* commands as unknown). It returns the status.
func requireUSBFeatures(ctx context.Context, c podCommander, what string, names ...string) (podInfo, error) {
	raw, err := c.Command(ctx, features.StatusRequest())
	if err != nil {
		if pe, ok := tcpclient.AsPodError(err); ok {
			return podInfo{}, refusedError(what, pe)
		}
		return podInfo{}, fmt.Errorf("%s: status: %w", what, err)
	}
	var info podInfo
	_ = json.Unmarshal(raw, &info)
	for _, n := range names {
		if ok, err := features.Has(raw, n); err != nil || !ok {
			ver := ""
			if info.Version != "" {
				ver = " (this pod runs " + info.Version + ")"
			}
			return info, fmt.Errorf("%s: this pod %s%s", what, usbHostNeedsFirmware, ver)
		}
	}
	return info, nil
}

var boardRevRe = regexp.MustCompile(`^v(\d+)(?:r(\d+))?$`)

// hasUSBAHost says the pod has the USB-A host port: board_rev v3r4 or later.
func hasUSBAHost(boardRev string) bool {
	m := boardRevRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(boardRev)))
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	rev, _ := strconv.Atoi(m[2])
	return major > 3 || (major == 3 && rev >= 4)
}

// chooseUSBHostPort picks the port: the flag when given, else usb_a on a pod with the USB-A host
// port and usb_c on an older one.
func chooseUSBHostPort(flag, boardRev string) (string, error) {
	flag = strings.ToLower(strings.TrimSpace(flag))
	if flag != "" {
		for _, p := range usbHostPorts {
			if flag == p {
				if p == "usb_a" && boardRev != "" && !hasUSBAHost(boardRev) {
					return "", usageErrorf("--port usb_a: this pod (board %s) has no USB-A host port; use --port usb_c", boardRev)
				}
				return p, nil
			}
		}
		return "", usageErrorf("--port: use usb_a or usb_c (got %q)", flag)
	}
	if hasUSBAHost(boardRev) {
		return "usb_a", nil
	}
	return "usb_c", nil
}

func usbHostRead(ctx context.Context, c podCommander) (*usbHostReport, json.RawMessage, error) {
	raw, err := c.Command(ctx, map[string]any{"cmd": "usb_host"})
	if err != nil {
		return nil, nil, err
	}
	var r usbHostReport
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, nil, fmt.Errorf("unexpected usb_host reply %s", strings.TrimSpace(string(raw)))
	}
	return &r, raw, nil
}

func usbHostStart(ctx context.Context, c podCommander, port string, timeoutS int) error {
	_, err := c.Command(ctx, map[string]any{"cmd": "usb_host", "enabled": true, "port": port, "timeout_s": timeoutS})
	return err
}

// usbHostWaitMode polls until the pod reports the mode (the switch happens right after the reply).
func usbHostWaitMode(ctx context.Context, c podCommander, want string, poll time.Duration) (*usbHostReport, json.RawMessage, error) {
	for {
		r, raw, err := usbHostRead(ctx, c)
		if err != nil {
			return nil, nil, err
		}
		if r.Mode == want {
			return r, raw, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("the pod did not reach USB %s mode (it is %q): %w", want, r.Mode, ctx.Err())
		case <-time.After(poll):
		}
	}
}

func newUSBHostCmd(g *globalFlags) *cobra.Command {
	var port string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "usb-host [on|off|status]",
		Short: "Turn the pod's USB host mode on or off, or show the USB device it sees",
		Long: "The pod hosts a board's USB port for a while and reports the device that enumerates on it:\n" +
			"IDs, strings, speed, enumeration time and interfaces (firmware 4.0.3 and later).\n" +
			"`benchpod usb-flash` flashes the board over it. Default: status.\n\n" +
			"  on      start host mode (or renew its timeout when it runs on that port)\n" +
			"  off     stop it\n" +
			"  status  show the mode and the device\n\n" +
			"--port is usb_a (the USB-A host port, on pods of board v3r4 and later: the board's own USB\n" +
			"cable goes there; the default) or usb_c (the pod's own USB-C connector through a breakout:\n" +
			"D+, D- and GND to the board, 5 V from a supply into the pod's VBUS; the default on older pods).\n" +
			"The global --timeout is how long `on` keeps host mode (default 60s, 1s to 1h); when it runs out\n" +
			"the pod goes back to being a USB device by itself.\n\n" +
			"While the pod hosts, it is off the computer's USB: no USB console and no USB network adapter.\n" +
			"Use the network (Ethernet, Wi-Fi) or embeddedci.com to start host mode; over the pod's own\n" +
			"USB link it is refused.",
		Example: "  benchpod usb-host on --port usb_a --timeout 2m\n" +
			"  benchpod usb-host\n" +
			"  benchpod usb-host status --json\n" +
			"  benchpod usb-host off",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"on", "off", "status"},
		RunE: func(cmd *cobra.Command, args []string) error {
			action := "status"
			if len(args) == 1 {
				action = strings.ToLower(args[0])
			}
			switch action {
			case "on", "off", "status":
			default:
				return usageErrorf("usb-host: use on, off or status (got %q)", args[0])
			}
			if action != "on" && cmd.Flags().Changed("port") {
				return usageErrorf("usb-host: --port only applies to `on`")
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return err
			}
			defer closeOut()
			ctx, cancel, t, err := g.podClient("usb-host", 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()
			hostTimeout := g.timeout
			if hostTimeout <= 0 {
				hostTimeout = usbHostDefaultTime
			}
			return runUSBHost(ctx, t.client, t.where(), usbHostOpts{action: action, port: port, timeout: hostTimeout, asJSON: asJSON}, out)
		},
	}
	cmd.Flags().StringVar(&port, "port", "", "usb_a or usb_c (default: usb_a on a pod with the USB-A host port, board v3r4 or later; else usb_c)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the pod's reply as JSON")
	return cmd
}

type usbHostOpts struct {
	action  string // on, off, status
	port    string
	timeout time.Duration
	asJSON  bool
}

// usbHostPoll is how often the poll loops ask the pod (tests shorten it).
var usbHostPoll = 250 * time.Millisecond

func runUSBHost(ctx context.Context, c podCommander, where string, o usbHostOpts, out io.Writer) error {
	const what = "usb-host"
	info, err := requireUSBFeatures(ctx, c, what, "usb_host_mode")
	if err != nil {
		return err
	}
	switch o.action {
	case "on":
		port, err := chooseUSBHostPort(o.port, info.BoardRev)
		if err != nil {
			return err
		}
		secs := int((o.timeout + time.Second - 1) / time.Second)
		if secs < 1 || secs > 3600 {
			return usageErrorf("usb-host: --timeout must be 1s to 1h for host mode (got %s)", o.timeout)
		}
		if err := usbHostStart(ctx, c, port, secs); err != nil {
			return usbHostErr(what, err)
		}
	case "off":
		if _, err := c.Command(ctx, map[string]any{"cmd": "usb_host", "enabled": false}); err != nil {
			return usbHostErr(what, err)
		}
	}
	r, raw, err := usbHostRead(ctx, c)
	if err != nil {
		return usbHostErr(what, err)
	}
	if o.action == "on" && r.Mode != "host" {
		if r, raw, err = usbHostWaitMode(ctx, c, "host", usbHostPoll); err != nil {
			return usbHostErr(what, err)
		}
	}
	if o.asJSON {
		fmt.Fprintln(out, strings.TrimSpace(string(raw)))
		return nil
	}
	printUSBHost(out, where, r)
	return nil
}

// usbHostErr renders a pod refusal (the catalog's fix) or passes other errors through.
func usbHostErr(what string, err error) error {
	if isUnknownCommand(err) {
		return fmt.Errorf("%s: this pod %s", what, usbHostNeedsFirmware)
	}
	if pe, ok := tcpclient.AsPodError(err); ok {
		return refusedError(what, pe)
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return err
	}
	return fmt.Errorf("%s: %w", what, err)
}

func printUSBHost(out io.Writer, where string, r *usbHostReport) {
	switch r.Mode {
	case "host":
		fmt.Fprintf(out, "USB host mode on the pod at %s: on, port %s, %d s left\n", where, r.Port, r.RemainingS)
	case "starting", "stopping":
		fmt.Fprintf(out, "USB host mode on the pod at %s: %s\n", where, r.Mode)
	default:
		fmt.Fprintf(out, "USB host mode on the pod at %s: off (the pod is a normal USB device)\n", where)
		return
	}
	line := r.LineSpeed
	if line == "" {
		line = "none"
	}
	fmt.Fprintf(out, "  Line:        %s speed, %s, enumeration %s, %d device(s) seen\n", line, yesNo(r.Connected, "device connected", "nothing connected"), orDash(r.Enumeration), r.Enumerations)
	if hint := usbLineHint(r); hint != "" {
		fmt.Fprintf(out, "  Hint:        %s\n", hint)
	}
	d := r.Device
	if d == nil {
		fmt.Fprintln(out, "  Device:      none yet")
		return
	}
	state := "attached"
	if !r.Attached {
		state = "detached (last device)"
	}
	fmt.Fprintf(out, "  Device:      %s %s, %s\n", d.ids(), usbNames(d), state)
	if d.Serial != "" {
		fmt.Fprintf(out, "  Serial:      %s\n", d.Serial)
	}
	fmt.Fprintf(out, "  USB:         %s, release %s, %s speed, class %02x/%02x/%02x, enumerated in %d ms\n", d.USB, d.Release, d.Speed, d.Class, d.Subclass, d.Protocol, d.MountedMs)
	if c := d.Config; c != nil {
		power := "bus powered"
		if c.SelfPowered {
			power = "self powered"
		}
		fmt.Fprintf(out, "  Config:      %d, declares %d mA, %s%s\n", c.Value, c.MaxPowerMA, power, yesNo(c.Truncated || c.Malformed, " (descriptor cut short)", ""))
		for _, i := range c.Interfaces {
			name := i.ClassName
			if name == "" {
				name = fmt.Sprintf("class %02x", i.Class)
			}
			fmt.Fprintf(out, "  Interface %d: %s (class %02x/%02x/%02x), %d endpoint(s)\n", i.Number, name, i.Class, i.Subclass, i.Protocol, i.Endpoints)
		}
	}
}

// usbLineHint explains a line state that points to wiring.
func usbLineHint(r *usbHostReport) string {
	switch {
	case r.Enumeration == "stuck":
		return "the device started to enumerate and stopped: its configuration descriptor may be longer than the pod reads (1024 bytes)"
	case r.LineSpeed == "low":
		return "a low-speed line from a full-speed board means D+ and D- are swapped"
	case !r.Connected && !r.Attached && r.Mode == "host":
		return "no device pull-up on the port: check the cable and that D+ and D- reach the pod (and that the board is powered)"
	}
	return ""
}

func usbNames(d *usbDevice) string {
	s := strings.TrimSpace(d.Manufacturer + " " + d.Product)
	if s == "" {
		return "(no strings)"
	}
	return s
}

func yesNo(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// usbEnsureHost makes sure USB host mode is on and returns its state: when it is off it starts it
// on the port (flag, else by board; it stays on for hostSecs, or defSecs when that is 0) and says
// so; when it runs with less than a minute left it renews it. Errors carry the command's name.
func usbEnsureHost(ctx context.Context, pod podCommander, what string, info podInfo, portFlag string, hostSecs, defSecs int, poll time.Duration, say func(string, ...any)) (*usbHostReport, error) {
	host, _, err := usbHostRead(ctx, pod)
	if err != nil {
		return nil, usbHostErr(what, err)
	}
	switch host.Mode {
	case "host":
		if host.RemainingS < int(usbHostExtra/time.Second) && hostSecs > 0 {
			// Renew so it does not run out under the command.
			if err := usbHostStart(ctx, pod, host.Port, hostSecs); err != nil {
				return nil, usbHostErr(what, err)
			}
		}
	case "starting":
	default:
		port, err := chooseUSBHostPort(portFlag, info.BoardRev)
		if err != nil {
			return nil, err
		}
		secs := hostSecs
		if secs <= 0 {
			secs = defSecs
		}
		say("USB host mode was off: starting it on %s (it stays on for %d s, or until `benchpod usb-host off`)", port, secs)
		if err := usbHostStart(ctx, pod, port, secs); err != nil {
			return nil, usbHostErr(what, err)
		}
	}
	host, _, err = usbHostWaitMode(ctx, pod, "host", poll)
	if err != nil {
		return nil, usbHostErr(what, err)
	}
	return host, nil
}
