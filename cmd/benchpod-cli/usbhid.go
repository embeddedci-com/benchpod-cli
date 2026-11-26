package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// `benchpod usb-hid [list|capture]`: the HID interfaces of the board on the pod's USB host port
// and its input reports (firmware 4.0.3+, feature usb_hid): keyboard keys, a mouse, a gamepad, a
// custom HID device. `list` shows the interfaces and report descriptors; `capture` catches reports.

const (
	usbHIDDefaultDuration = 10 * time.Second
	usbHIDReadMax         = 32 // reports per usb_hid read
	usbHIDReadTimeoutMS   = 2000
)

type usbHIDInterface struct {
	Instance      int    `json:"instance"`
	Interface     int    `json:"interface"`
	Protocol      string `json:"protocol"`
	DescriptorLen int    `json:"descriptor_len"`
	Offset        int    `json:"descriptor_offset"`
	Truncated     bool   `json:"truncated"`
	Descriptor    string `json:"descriptor"`
}

type usbHIDList struct {
	Attached   bool              `json:"attached"`
	Interfaces []usbHIDInterface `json:"interfaces"`
}

type usbHIDReport struct {
	MS       int    `json:"ms"`
	Instance int    `json:"instance"`
	Data     string `json:"data"`
}

type usbHIDReadReply struct {
	Reports   []usbHIDReport `json:"reports"`
	Count     int            `json:"count"`
	Dropped   int            `json:"dropped"`
	Capturing bool           `json:"capturing"`
}

type usbHIDOpts struct {
	action     string // list, capture
	instance   int    // -1 = all
	count      int    // capture: stop after this many reports (0 = until the duration)
	duration   time.Duration
	port       string
	asJSON     bool
	hostSecs   int
	pollPeriod time.Duration
}

func newUSBHIDCmd(g *globalFlags) *cobra.Command {
	o := usbHIDOpts{instance: -1}
	cmd := &cobra.Command{
		Use:   "usb-hid [list|capture]",
		Short: "Show the HID interfaces of the board on the pod's USB host port and catch its input reports",
		Long: "The pod hosts the board on its USB port and reads its HID interfaces (the first two of a composite\n" +
			"device): report descriptors, and the input reports a keyboard, mouse, gamepad or custom HID device\n" +
			"sends (firmware 4.0.3 and later). If USB host mode is off, usb-hid starts it (and says so). Default: list.\n\n" +
			"  list     the interfaces with their protocol (keyboard, mouse, none) and report descriptor (hex)\n" +
			"  capture  catch input reports for --duration (default 10s) or until --count reports; each line is the\n" +
			"           time since the capture started in ms, the interface (hid0, hid1) and the report as hex\n" +
			"           (with several report IDs the first byte is the ID). Trigger the reports meanwhile: press a key,\n" +
			"           move the mouse, or let the board's firmware send them.\n\n" +
			"The pod never sends anything to the device. A capture ends with the command; it also ends when the board\n" +
			"detaches (a board that resets needs a new capture). The pod refuses usb-hid while a usb-flash or usb-check\n" +
			"runs. --port is usb_a or usb_c when host mode has to be started (default: usb_a on a pod with the USB-A host\n" +
			"port, board v4 and later; else usb_c). While the pod hosts it is off the computer's USB, so use the\n" +
			"pod's network address (not --connection usb). `benchpod usb-host off` stops host mode.",
		Example: "  benchpod usb-hid\n" +
			"  benchpod usb-hid capture --duration 30s\n" +
			"  benchpod usb-hid capture --count 5 --instance 1 --json",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"list", "capture"},
		RunE: func(cmd *cobra.Command, args []string) error {
			o.action = "list"
			if len(args) == 1 {
				o.action = strings.ToLower(args[0])
			}
			f := cmd.Flags()
			if err := validateUSBHID(&o, f.Changed("instance"), f.Changed("count"), f.Changed("duration")); err != nil {
				return err
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return err
			}
			defer closeOut()
			o.hostSecs = int((o.duration + usbHostExtra*2) / time.Second)
			if o.action == "list" {
				o.hostSecs = int((2 * usbHostExtra) / time.Second)
			}
			if o.hostSecs > 3600 {
				o.hostSecs = 3600
			}
			total := g.effectiveTimeout(time.Minute)
			if o.action == "capture" {
				total += o.duration
			}
			ctx, cancel, t, err := g.podClient("usb-hid", total)
			if err != nil {
				return err
			}
			defer cancel()
			return runUSBHID(ctx, t.client, o, out, cmd.ErrOrStderr())
		},
	}
	f := cmd.Flags()
	f.IntVar(&o.instance, "instance", -1, "only this HID interface, 0 or 1 (default: all)")
	f.IntVar(&o.count, "count", 0, "capture: stop after this many reports (default: run for --duration)")
	f.DurationVar(&o.duration, "duration", usbHIDDefaultDuration, "capture: how long to catch reports, 1s to 1h")
	f.StringVar(&o.port, "port", "", "usb_a or usb_c when host mode has to be started (default: usb_a on a pod with the USB-A host port, board v4 or later; else usb_c)")
	f.BoolVar(&o.asJSON, "json", false, "print the interfaces (list) or the reports (capture) as JSON")
	return cmd
}

func validateUSBHID(o *usbHIDOpts, hasInstance, hasCount, hasDuration bool) error {
	switch o.action {
	case "list":
		if hasCount || hasDuration {
			return usageErrorf("usb-hid list: --count and --duration are for capture")
		}
	case "capture":
		if o.count < 0 {
			return usageErrorf("usb-hid: --count must be positive (got %d)", o.count)
		}
		if o.duration < time.Second || o.duration > time.Hour {
			return usageErrorf("usb-hid: --duration must be 1s to 1h (got %s)", o.duration)
		}
	default:
		return usageErrorf("usb-hid: use list or capture (got %q)", o.action)
	}
	if hasInstance && (o.instance < 0 || o.instance > 1) {
		return usageErrorf("usb-hid: --instance must be 0 or 1 (got %d)", o.instance)
	}
	return validUSBPort(o.port)
}

func usbHIDErr(what string, err error) error { return usbHostErr(what, err) }

func runUSBHID(ctx context.Context, pod podCommander, o usbHIDOpts, out, progress io.Writer) error {
	const what = "usb-hid"
	poll := o.pollPeriod
	if poll <= 0 {
		poll = usbHostPoll
	}
	say := func(format string, a ...any) { fmt.Fprintf(progress, "usb-hid: "+format+"\n", a...) }
	info, err := requireUSBFeatures(ctx, pod, what, "usb_host_mode", "usb_hid")
	if err != nil {
		return err
	}
	host, err := usbEnsureHost(ctx, pod, what, info, o.port, o.hostSecs, int((o.duration+2*usbHostExtra)/time.Second), poll, say)
	if err != nil {
		return err
	}
	if _, err := usbWaitDevice(ctx, pod, host, poll, say); err != nil {
		return usbHIDErr(what, err)
	}
	if o.action == "capture" {
		return usbHIDCapture(ctx, pod, o, out, say)
	}
	ifs, err := usbHIDInterfaces(ctx, pod, o.instance)
	if err != nil {
		return usbHIDErr(what, err)
	}
	if o.asJSON {
		b, _ := json.Marshal(usbHIDList{Attached: true, Interfaces: ifs})
		fmt.Fprintln(out, string(b))
		return nil
	}
	if len(ifs) == 0 {
		fmt.Fprintln(out, "The board has no HID interface.")
		return nil
	}
	for _, i := range ifs {
		fmt.Fprintf(out, "hid%d: USB interface %d, protocol %s, report descriptor %d bytes\n", i.Instance, i.Interface, i.Protocol, i.DescriptorLen)
		if i.Descriptor != "" {
			fmt.Fprintf(out, "  %s\n", i.Descriptor)
		}
		if i.Truncated || len(i.Descriptor)/2 < i.DescriptorLen {
			fmt.Fprintln(out, "  (descriptor cut short)")
		}
	}
	return nil
}

// usbHIDInterfaces lists the interfaces and reads each report descriptor to its end (a long
// one comes cut and continues from an offset).
func usbHIDInterfaces(ctx context.Context, pod podCommander, instance int) ([]usbHIDInterface, error) {
	req := map[string]any{"cmd": "usb_hid"}
	if instance >= 0 {
		req["instance"] = instance
	}
	raw, err := pod.Command(ctx, req)
	if err != nil {
		return nil, err
	}
	var l usbHIDList
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("unexpected usb_hid reply %s", strings.TrimSpace(string(raw)))
	}
	for k := range l.Interfaces {
		it := &l.Interfaces[k]
		for guard := 0; it.Truncated && guard < 64; guard++ {
			have := it.Offset + len(it.Descriptor)/2
			raw, err := pod.Command(ctx, map[string]any{"cmd": "usb_hid", "instance": it.Instance, "offset": have})
			if err != nil {
				return nil, err
			}
			var more usbHIDList
			if json.Unmarshal(raw, &more) != nil || len(more.Interfaces) != 1 || more.Interfaces[0].Descriptor == "" {
				break
			}
			it.Descriptor += more.Interfaces[0].Descriptor
			it.Truncated = more.Interfaces[0].Truncated
		}
		it.Offset = 0
		it.Truncated = it.Truncated || len(it.Descriptor)/2 < it.DescriptorLen
	}
	return l.Interfaces, nil
}

// usbHIDCapture catches reports until the count or the duration, and always stops the capture.
func usbHIDCapture(ctx context.Context, pod podCommander, o usbHIDOpts, out io.Writer, say func(string, ...any)) error {
	const what = "usb-hid"
	if _, err := pod.Command(ctx, map[string]any{"cmd": "usb_hid", "capture": true}); err != nil {
		return usbHIDErr(what, err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = pod.Command(sctx, map[string]any{"cmd": "usb_hid", "capture": false})
		cancel()
	}()
	say("capturing reports for %s%s...", o.duration, yesNo(o.count > 0, fmt.Sprintf(" or until %d reports", o.count), ""))
	deadline := time.Now().Add(o.duration)
	var all []usbHIDReport
	dropped, ended := 0, false
	for !ended && ctx.Err() == nil {
		wait := time.Until(deadline)
		if wait <= 0 || (o.count > 0 && len(all) >= o.count) {
			break
		}
		n := usbHIDReadMax
		if o.count > 0 && o.count-len(all) < n {
			n = o.count - len(all)
		}
		ms := usbHIDReadTimeoutMS
		if int(wait/time.Millisecond) < ms {
			ms = int(wait/time.Millisecond) + 1
		}
		req := map[string]any{"cmd": "usb_hid", "read": true, "count": n, "timeout_ms": ms}
		if o.instance >= 0 {
			req["instance"] = o.instance
		}
		raw, err := pod.Command(ctx, req)
		if err != nil {
			return usbHIDErr(what, err)
		}
		var r usbHIDReadReply
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("%s: unexpected usb_hid reply %s", what, strings.TrimSpace(string(raw)))
		}
		dropped = r.Dropped
		ended = !r.Capturing
		for _, rep := range r.Reports {
			all = append(all, rep)
			if !o.asJSON {
				fmt.Fprintf(out, "%7d ms  hid%d  %s\n", rep.MS, rep.Instance, rep.Data)
			}
		}
	}
	if o.asJSON {
		if all == nil {
			all = []usbHIDReport{}
		}
		b, _ := json.Marshal(usbHIDReadReply{Reports: all, Count: len(all), Dropped: dropped, Capturing: !ended})
		fmt.Fprintln(out, string(b))
	}
	if ended {
		say("the capture ended early: the board detached or host mode ended")
	}
	if dropped > 0 {
		say("%d report(s) were dropped because the pod's ring was full", dropped)
	}
	if !o.asJSON && len(all) == 0 {
		fmt.Fprintln(out, "No reports arrived.")
	}
	return ctx.Err()
}
