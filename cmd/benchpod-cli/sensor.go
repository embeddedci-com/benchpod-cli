package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/embeddedci-com/benchpod-cli/internal/benchpodconfig"
	"github.com/embeddedci-com/benchpod-cli/internal/sensors"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// User-defined sensors (firmware with the "sensors" capability): the pod plays a sensor from a
// definition the host sends (the sensor library's format). The CLI reads definitions from files
// or the sensor library, checks their header, uploads them in chunks small enough for a cloud
// command, and drives the instances. The pod keeps a sensor running after the CLI exits, until
// `benchpod sensor stop`, a reboot or a firmware update.

const sensorTimeout = 30 * time.Second

func newSensorCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sensor",
		Short: "Emulate sensors for the DUT from definitions (I2C, SPI, UART, analog)",
		Long: "Emulate sensors for the DUT: the pod plays a sensor from a definition (YAML or JSON,\n" +
			"the format of github.com/embeddedci-com/benchpod-sensors). Use a definition from the\n" +
			"sensor library by name (`benchpod sensor library` lists them) or your own file.\n" +
			"Needs pod firmware with the sensors capability. Over the network and through\n" +
			"embeddedci.com; library, export and an offline validate need no pod.",
	}
	cmd.AddCommand(newSensorLibraryCmd(g), newSensorExportCmd(g), newSensorValidateCmd(g),
		newSensorStartCmd(g), newSensorSetCmd(g), newSensorStimulusCmd(g), newSensorFaultCmd(g),
		newSensorStopCmd(g), newSensorStatusCmd(g), newSensorImageCmd(g))
	return cmd
}

// sensorImages are the gateware images fpga_image takes, by the name the pod reports.
var sensorImages = []string{"loop", "deep", "emulation"}

func newSensorImageCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "image [loop|deep|emulation]",
		Short: "Show or switch the pod's FPGA image (the emulation image runs the I2C, SPI and UART sensors)",
		Long: "Without an argument, show the FPGA image the pod runs. With one, switch to it (2 to 3 s):\n" +
			"loop (the closed-loop DAC), deep (deep DAC replay) or emulation (gateware v54+).\n" +
			"`benchpod sensor start` loads the emulation image by itself, and the pod never switches\n" +
			"back on its own: after `benchpod sensor stop`, `benchpod sensor image loop` brings the\n" +
			"control loop back. The pod refuses to leave the emulation image while I2C, SPI or UART\n" +
			"sensors run. No flags.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := map[string]any{"cmd": "fpga_image"}
			if len(args) == 1 {
				name := strings.ToLower(strings.TrimSpace(args[0]))
				if !containsString(sensorImages, name) {
					return usageErrorf("sensor image: the image is loop, deep or emulation, got %q", args[0])
				}
				req["image"] = name
			}
			ctx, cancel, t, err := g.podClient("sensor image", sensorTimeout)
			if err != nil {
				return err
			}
			defer cancel()
			reply, err := sensorCommand(ctx, t, "sensor image", req)
			if err != nil {
				return err
			}
			name, _ := reply["name"].(string)
			if name == "" {
				if n, ok := reply["image"].(float64); ok && int(n) >= 0 && int(n) < len(sensorImages) {
					name = sensorImages[int(n)]
				}
			}
			verb := "runs"
			if len(args) == 1 {
				verb = "switched to"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "The pod on %s %s the %s FPGA image (image %v, gateware v%v)\n",
				t.label, verb, valueOrUnknown(name), reply["image"], reply["version"])
			return nil
		},
	}
}

func valueOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// ── the library ─────────────────────────────────────────────────────────────

// sensorLibrary opens the library version: --version, else BENCHPOD_SENSOR_LIBRARY, else the
// config's sensor_library, else the pinned one. A fallback (offline) is a warning on stderr.
func sensorLibrary(g *globalFlags, flagVersion string, offline bool) (*sensors.Library, error) {
	saved := ""
	if cfgPath, err := resolveConfigPath(g.configFile); err == nil {
		if cfg, err := benchpodconfig.Load(cfgPath); err == nil {
			saved = cfg.SensorLibrary
		}
	}
	version, err := sensors.RequestedVersion(flagVersion, saved)
	if err != nil {
		return nil, usageErrorf("%v", err)
	}
	lib, err := sensors.Open(sensors.Options{Version: version, Offline: offline})
	if err != nil {
		return nil, err
	}
	if lib.Warning != "" {
		fmt.Fprintln(os.Stderr, "Warning: "+lib.Warning)
	}
	return lib, nil
}

func newSensorLibraryCmd(g *globalFlags) *cobra.Command {
	var version string
	var save, offline bool
	c := &cobra.Command{
		Use:   "library",
		Short: "List the sensor library's definitions and the version in use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return err
			}
			defer closeOut()
			if save {
				if err := saveSensorLibrary(g, version); err != nil {
					return err
				}
			}
			lib, err := sensorLibrary(g, version, offline)
			if err != nil {
				return err
			}
			printLibrary(out, lib)
			return nil
		},
	}
	c.Flags().StringVar(&version, "version", "", "library version: latest, a release such as 0.2.0, or default (the version this CLI pins)")
	c.Flags().BoolVar(&save, "save", false, "save --version as the default for the CLI and the Python SDK (sensor_library in config.json)")
	c.Flags().BoolVar(&offline, "offline", false, "do not download: use the cache or the bundled version")
	return c
}

func saveSensorLibrary(g *globalFlags, version string) error {
	v, err := sensors.NormalizeVersion(version)
	if err != nil {
		return usageErrorf("%v", err)
	}
	cfgPath, err := resolveConfigPath(g.configFile)
	if err != nil {
		return err
	}
	cfg, err := benchpodconfig.Load(cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if cfg == nil {
		cfg = &benchpodconfig.Config{}
	}
	cfg.SensorLibrary = v
	if v == sensors.PinnedVersion && strings.TrimSpace(strings.ToLower(version)) != v {
		cfg.SensorLibrary = "" // "default": follow whatever each tool pins
	}
	if err := benchpodconfig.Save(cfgPath, cfg); err != nil {
		return err
	}
	shown := cfg.SensorLibrary
	if shown == "" {
		shown = "the pinned version"
	}
	fmt.Fprintf(os.Stderr, "Saved the sensor library default: %s (%s)\n", shown, cfgPath)
	return nil
}

func printLibrary(out io.Writer, lib *sensors.Library) {
	fmt.Fprintf(out, "Sensor library %s (%s)\n", lib.Version, lib.Source)
	w := 4
	for _, e := range lib.Entries {
		if len(e.Name) > w {
			w = len(e.Name)
		}
	}
	for _, e := range lib.Entries {
		fmt.Fprintf(out, "  %-*s  %-8s  %s\n", w, e.Name, e.Kind, e.Label)
	}
}

func newSensorExportCmd(g *globalFlags) *cobra.Command {
	var version, outFile string
	var format bool
	c := &cobra.Command{
		Use:   "export <name>",
		Short: "Write a library definition (YAML) to stdout or a file, to copy and edit",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, err := sensorLibrary(g, version, false)
			if err != nil {
				return err
			}
			text := lib.FormatDoc()
			if !format {
				if text, err = lib.Text(args[0]); err != nil {
					return usageErrorf("%v", err)
				}
			}
			if outFile == "" {
				outFile = g.outputFilename
			}
			if outFile == "" || outFile == "-" {
				_, err = io.WriteString(cmd.OutOrStdout(), text)
				return err
			}
			if err := os.WriteFile(outFile, []byte(text), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Wrote %s\n", outFile)
			return nil
		},
	}
	c.Flags().StringVarP(&outFile, "output", "o", "", "write to this file instead of stdout")
	c.Flags().StringVar(&version, "version", "", "library version (default: the pinned one, BENCHPOD_SENSOR_LIBRARY or the saved default)")
	c.Flags().BoolVar(&format, "format-doc", false, "write the library's format reference (docs/format.md) instead; <name> is then ignored")
	return c
}

// sensorDocument reads a definition or stimulus: a file path, "-" for stdin, or a library name.
func sensorDocument(g *globalFlags, src, version string, stdin io.Reader) (sensors.Document, error) {
	if src == "-" {
		raw, err := io.ReadAll(stdin)
		if err != nil {
			return sensors.Document{}, err
		}
		return sensors.ParseText(string(raw))
	}
	if st, err := os.Stat(src); err == nil && !st.IsDir() {
		return sensors.ReadFile(src)
	}
	if !sensors.NameRE.MatchString(src) && !strings.Contains(src, "/") {
		return sensors.Document{}, fmt.Errorf("%s: no such file", src)
	}
	lib, err := sensorLibrary(g, version, false)
	if err != nil {
		return sensors.Document{}, err
	}
	if lib.Find(src) == nil {
		if strings.ContainsAny(src, "/.") {
			return sensors.Document{}, fmt.Errorf("%s: no such file", src)
		}
		_, err := lib.Get(src)
		return sensors.Document{}, fmt.Errorf("%s is neither a file nor a library entry: %w", src, err)
	}
	return lib.Load(src)
}

// ── talking to the pod ──────────────────────────────────────────────────────

// sensorPod opens the pod connection for a sensor command and checks the firmware has sensor
// emulation.
func sensorPod(g *globalFlags, what string) (context.Context, context.CancelFunc, *podTarget, error) {
	ctx, cancel, t, err := g.podClient(what, sensorTimeout)
	if err != nil {
		return nil, func() {}, nil, err
	}
	status, err := t.client.Command(ctx, map[string]any{"cmd": "status"})
	if err != nil {
		cancel()
		return nil, func() {}, nil, fmt.Errorf("%s: status: %w", what, err)
	}
	if !sensors.HasCapability(status, "sensors") {
		cancel()
		return nil, func() {}, nil, fmt.Errorf("%s: %s", what, sensors.TooOld)
	}
	return ctx, cancel, t, nil
}

// sensorCommand sends one command and words a refusal for the user.
func sensorCommand(ctx context.Context, t *podTarget, what string, req map[string]any) (map[string]any, error) {
	raw, err := t.client.Command(ctx, req)
	if err != nil {
		if pe, ok := tcpclient.AsPodError(err); ok {
			return nil, refusedError(what, pe, !t.spec.IsCloud())
		}
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	out := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("%s: unexpected reply %s", what, strings.TrimSpace(string(raw)))
		}
	}
	return out, nil
}

// uploadDocument sends doc as sensor_define (instance set) or sensor_stimulus chunks and returns
// the last reply.
func uploadDocument(ctx context.Context, t *podTarget, what, cmd string, doc sensors.Document, instance string, dryRun bool) (map[string]any, error) {
	data, err := doc.JSON()
	if err != nil {
		return nil, err
	}
	reqs, err := sensors.UploadRequests(cmd, data, instance, dryRun)
	if err != nil {
		return nil, err
	}
	var reply map[string]any
	for _, req := range reqs {
		if reply, err = sensorCommand(ctx, t, what, req); err != nil {
			return nil, err
		}
	}
	return reply, nil
}

func newSensorValidateCmd(g *globalFlags) *cobra.Command {
	var version string
	var offline bool
	c := &cobra.Command{
		Use:   "validate <file|name>",
		Short: "Check a definition: its header here, then a dry-run compile on the pod",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSensorValidate(g, args[0], version, offline, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	c.Flags().StringVar(&version, "version", "", "library version for a library name")
	c.Flags().BoolVar(&offline, "offline", false, "check the header only, even with a pod connection")
	return c
}

func runSensorValidate(g *globalFlags, src, version string, offline bool, stdin io.Reader, out io.Writer) error {
	doc, err := sensorDocument(g, src, version, stdin)
	if err != nil {
		return err
	}
	if err := sensors.CheckDefinition(doc); err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	raw, _ := g.rawConnection()
	if offline || raw == "" {
		fmt.Fprintf(out, "%s: header ok (%s sensor %s); no pod connection, so it was not compiled\n", src, doc.Kind(), doc.Name())
		return nil
	}
	ctx, cancel, t, err := sensorPod(g, "sensor validate")
	if err != nil {
		return err
	}
	defer cancel()
	rep, err := uploadDocument(ctx, t, "sensor validate", "sensor_define", doc, doc.Name(), true)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s: compiles on %s (%s sensor %s, %v program bytes)\n", src, t.label, doc.Kind(), doc.Name(), rep["program_bytes"])
	printParams(out, rep["params"])
	return nil
}

func printParams(out io.Writer, params any) {
	list, _ := params.([]any)
	if len(list) == 0 {
		return
	}
	fmt.Fprintln(out, "Params (benchpod sensor set <instance> key=value):")
	for _, p := range list {
		m, _ := p.(map[string]any)
		unit := ""
		if u, _ := m["unit"].(string); u != "" {
			unit = " " + u
		}
		fmt.Fprintf(out, "  %-16v %v..%v%s (default %v)\n", m["key"], m["min"], m["max"], unit, m["default"])
	}
}

// ── start, set, stop ────────────────────────────────────────────────────────

type sensorStartFlags struct {
	instance, address, version string
	pins, values               []string
	baud                       int
}

func newSensorStartCmd(g *globalFlags) *cobra.Command {
	var f sensorStartFlags
	c := &cobra.Command{
		Use:   "start <file|name>",
		Short: "Upload a definition and start it on the pod's pins",
		Long: "Upload a definition (a file, - for stdin, or a library name) and start it.\n\n" +
			"Pins are role=channel: I2C sda, scl (default: the wiring profile's i2c_sda/i2c_scl)\n" +
			"and drdy; SPI sclk, mosi, miso, cs (default: the profile's spi_*; the emulated SPI\n" +
			"target has no data-ready pin); UART tx (pod to DUT RX) and rx; analog output=dac\n" +
			"(default) or output=current. A channel is 1-14 (or la4); another word is a name in\n" +
			"the wiring profile on embeddedci.com, resolved here. Set the LA voltage and,\n" +
			"for I2C, the pull-ups first (`benchpod la pullup`). Starting an instance that runs\n" +
			"replaces it; the sensor keeps running after this command, until `benchpod sensor stop`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return err
			}
			defer closeOut()
			return runSensorStart(g, args[0], f, cmd.InOrStdin(), out)
		},
	}
	c.Flags().StringVar(&f.instance, "instance", "", "instance name, 1 to 16 characters of a-z, 0-9 and _ (default: the definition's name)")
	c.Flags().StringArrayVar(&f.pins, "pin", nil, "pin as role=channel, e.g. --pin sda=2 --pin scl=1 (repeatable)")
	c.Flags().StringVar(&f.address, "address", "", "I2C address: the definition's address or one of its alt_addresses (e.g. 0x77)")
	c.Flags().StringArrayVar(&f.values, "set", nil, "initial value as key=number, e.g. --set temperature_c=30 (repeatable)")
	c.Flags().IntVar(&f.baud, "baud", 0, "UART: override the definition's baud rate")
	c.Flags().StringVar(&f.version, "version", "", "library version for a library name")
	return c
}

func runSensorStart(g *globalFlags, src string, f sensorStartFlags, stdin io.Reader, out io.Writer) error {
	doc, err := sensorDocument(g, src, f.version, stdin)
	if err != nil {
		return err
	}
	if err := sensors.CheckDefinition(doc); err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	instance := f.instance
	if instance == "" {
		instance = doc.Name()
		if !sensors.InstanceRE.MatchString(instance) {
			return usageErrorf("sensor start: the definition's name %q is longer than an instance name (at most 16 characters): pass --instance", instance)
		}
	}
	if !sensors.InstanceRE.MatchString(instance) {
		return usageErrorf("sensor start: --instance is 1 to 16 characters of a-z, 0-9 and _, got %q", instance)
	}
	req := map[string]any{"cmd": "sensor_start", "instance": instance}
	pins, err := sensorPins(g, doc.Kind(), f.pins)
	if err != nil {
		return usageErrorf("sensor start: %v", err)
	}
	req["pins"] = pins
	if f.address != "" {
		a, err := sensors.Address(f.address)
		if err != nil {
			return usageErrorf("sensor start: %v", err)
		}
		req["address"] = a
	}
	if f.baud != 0 {
		if f.baud < 300 || f.baud > 4_000_000 {
			return usageErrorf("sensor start: --baud must be 300-4000000, got %d", f.baud)
		}
		req["baud"] = f.baud
	}
	if len(f.values) > 0 {
		vals, err := sensors.Values(f.values)
		if err != nil {
			return usageErrorf("sensor start: %v", err)
		}
		req["values"] = vals
	}
	ctx, cancel, t, err := sensorPod(g, "sensor start")
	if err != nil {
		return err
	}
	defer cancel()
	info, err := uploadDocument(ctx, t, "sensor start", "sensor_define", doc, instance, false)
	if err != nil {
		return err
	}
	started, err := sensorCommand(ctx, t, "sensor start", req)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Started %s (%s sensor %s) on %s, %s", instance, doc.Kind(), doc.Name(), t.label, formatPins(pins))
	if slot, _ := started["slot"].(string); slot != "" {
		fmt.Fprintf(out, ", slot %s", slot)
	}
	fmt.Fprintln(out)
	if loaded, _ := started["image_loaded"].(bool); loaded {
		fmt.Fprintf(out, "The pod loaded its %v FPGA image; unavailable meanwhile: %v\n", started["image"], started["unavailable"])
	}
	printParams(out, info["params"])
	return nil
}

// sensorPins parses --pin role=channel flags for kind, filling I2C and SPI pins left out from
// the wiring profile on embeddedci.com when there is one.
func sensorPins(g *globalFlags, kind string, flags []string) (map[string]any, error) {
	pins := map[string]any{}
	for _, p := range flags {
		role, val, ok := strings.Cut(p, "=")
		role = strings.ToLower(strings.TrimSpace(role))
		if !ok || role == "" {
			return nil, fmt.Errorf("--pin is role=channel (e.g. sda=2), got %q", p)
		}
		if _, dup := pins[role]; dup {
			return nil, fmt.Errorf("--pin %s given twice", role)
		}
		v, err := sensors.PinValue(role, val)
		if err != nil {
			return nil, err
		}
		pins[role] = v
	}
	if kind == "analog" {
		if _, ok := pins["output"]; !ok {
			pins["output"] = "dac"
		}
	}
	defaults := map[string]map[string]func(*podWiring) *int{
		"i2c": {"sda": func(w *podWiring) *int { return w.I2cSda }, "scl": func(w *podWiring) *int { return w.I2cScl }},
		"spi": {"sclk": func(w *podWiring) *int { return w.SpiSclk }, "mosi": func(w *podWiring) *int { return w.SpiMosi },
			"miso": func(w *podWiring) *int { return w.SpiMiso }, "cs": func(w *podWiring) *int { return w.SpiCs }},
	}[kind]
	var missing []string
	for role := range defaults {
		if _, ok := pins[role]; !ok {
			missing = append(missing, role)
		}
	}
	named := false
	for role, v := range pins {
		if _, ok := v.(string); ok && role != "output" {
			named = true
		}
	}
	var w *podWiring
	if len(missing) > 0 || named {
		w = loadPodWiring(g, os.Stderr)
	}
	if named {
		if err := sensors.ResolvePins(pins, w.channel); err != nil {
			return nil, err
		}
	}
	if len(missing) > 0 {
		if w != nil {
			sort.Strings(missing)
			for _, role := range missing {
				if la := defaults[role](w); la != nil && *la >= 1 && *la <= laPinCount {
					pins[role] = *la
					fmt.Fprintf(os.Stderr, "sensor start: %s=LA%d from %s\n", role, *la, wiringSource)
				}
			}
		}
	}
	return pins, sensors.Pins(kind, pins)
}

func formatPins(pins map[string]any) string {
	roles := make([]string, 0, len(pins))
	for r := range pins {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	parts := make([]string, 0, len(roles))
	for _, r := range roles {
		if n, ok := pins[r].(int); ok {
			parts = append(parts, fmt.Sprintf("%s LA%d", r, n))
		} else {
			parts = append(parts, fmt.Sprintf("%s %v", r, pins[r]))
		}
	}
	return strings.Join(parts, ", ")
}

func newSensorSetCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "set <instance> <key=value>...",
		Short: "Change what a running sensor reports",
		Long: "Change a running sensor's readings, e.g. `benchpod sensor set env temperature_c=31.5`.\n" +
			"The pod refuses a value outside its param's range and names the range. No flags.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			vals, err := sensors.Values(args[1:])
			if err != nil {
				return usageErrorf("sensor set: %v", err)
			}
			return sensorSimple(g, cmd.OutOrStdout(), "sensor set",
				map[string]any{"cmd": "sensor_set", "instance": args[0], "values": vals},
				"Set "+strings.Join(args[1:], " ")+" on "+args[0])
		},
	}
}

// sensorSimple sends one command after the capability check and prints done.
func sensorSimple(g *globalFlags, out io.Writer, what string, req map[string]any, done string) error {
	_, err := sensorSend(g, what, req)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, done)
	return nil
}

// sensorSend checks the instance name, then sends one command after the capability check.
func sensorSend(g *globalFlags, what string, req map[string]any) (map[string]any, error) {
	if inst, ok := req["instance"].(string); ok && !sensors.InstanceRE.MatchString(inst) {
		return nil, usageErrorf("%s: an instance name is 1 to 16 characters of a-z, 0-9 and _, got %q", what, inst)
	}
	ctx, cancel, t, err := sensorPod(g, what)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return sensorCommand(ctx, t, what, req)
}

func newSensorStopCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "stop [instance]",
		Short: "Stop one emulated sensor, or all of them",
		Long: "Stop an instance (or every one when none is named): the pod releases its pins and forgets the\n" +
			"definition. The emulation FPGA image stays loaded until `benchpod sensor image` switches it. No flags.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := map[string]any{"cmd": "sensor_stop", "all": true}
			if len(args) == 1 {
				req = map[string]any{"cmd": "sensor_stop", "instance": args[0]}
			}
			reply, err := sensorSend(g, "sensor stop", req)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), stoppedLine(reply))
			return nil
		},
	}
}

// ── stimulus and faults ─────────────────────────────────────────────────────

func newSensorStimulusCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stimulus",
		Short: "Load, start and stop a stimulus: sensor values (and faults) over time",
	}
	var at time.Duration
	start := &cobra.Command{
		Use:   "start",
		Short: "Play the loaded stimulus",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req := map[string]any{"cmd": "sensor_stimulus_start"}
			if at < 0 {
				return usageErrorf("sensor stimulus start: --at must not be negative")
			}
			if at > 0 {
				req["at_s"] = at.Seconds()
			}
			return sensorSimple(g, cmd.OutOrStdout(), "sensor stimulus start", req, "Stimulus started")
		},
	}
	start.Flags().DurationVar(&at, "at", 0, "start this far into the stimulus, e.g. 12.5s")
	load := &cobra.Command{
		Use:   "load <file>",
		Short: "Upload a stimulus (YAML or JSON; data file: CSVs are inlined)",
		Long: "Upload a stimulus, replacing the loaded one. Its signals are params (temperature_c,\n" +
			"or env.temperature_c with several sensors); a data segment's file: (CSV t,value,\n" +
			"relative to the stimulus) is sent inline. `sensor stimulus start` plays it. No flags.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStimulusLoad(g, args[0], cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop the stimulus; values stay where it left them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return sensorSimple(g, cmd.OutOrStdout(), "sensor stimulus stop",
				map[string]any{"cmd": "sensor_stimulus_stop"}, "Stimulus stopped")
		},
	}
	cmd.AddCommand(load, start, stop)
	return cmd
}

func runStimulusLoad(g *globalFlags, src string, stdin io.Reader, out io.Writer) error {
	doc, err := sensorDocument(g, src, "", stdin)
	if err != nil {
		return err
	}
	if err := sensors.CheckStimulus(doc); err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	ctx, cancel, t, err := sensorPod(g, "sensor stimulus load")
	if err != nil {
		return err
	}
	defer cancel()
	if _, err := uploadDocument(ctx, t, "sensor stimulus load", "sensor_stimulus", doc, "", false); err != nil {
		return err
	}
	fmt.Fprintf(out, "Loaded stimulus %s on %s; play it with `benchpod sensor stimulus start`\n", doc.Name(), t.label)
	return nil
}

// sensorFaultTypes are the fault types of the format.
var sensorFaultTypes = []string{"stuck", "offset", "gain", "no_response", "corrupt", "bus_stuck",
	"garbage", "drop", "baud_error", "open", "short"}

func newSensorFaultCmd(g *globalFlags) *cobra.Command {
	var duration time.Duration
	var param, value, register, mask, rate string
	c := &cobra.Command{
		Use:   "fault <instance> <type|clear>",
		Short: "Inject a fault into a running sensor now, or clear its faults",
		Long: "Make a sensor misbehave: stuck, offset, gain, no_response, corrupt, bus_stuck,\n" +
			"garbage, drop, baud_error, open or short (see the format's faults section); clear\n" +
			"ends its faults. Without --duration a fault lasts until cleared.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			instance, typ := args[0], strings.ToLower(args[1])
			if typ == "clear" {
				return sensorSimple(g, cmd.OutOrStdout(), "sensor fault",
					map[string]any{"cmd": "sensor_fault_clear", "instance": instance}, "Cleared the faults of "+instance)
			}
			if !containsString(sensorFaultTypes, typ) {
				return usageErrorf("sensor fault: the type is clear or one of %s, got %q", strings.Join(sensorFaultTypes, ", "), args[1])
			}
			req := map[string]any{"cmd": "sensor_fault", "instance": instance, "type": typ}
			if duration < 0 {
				return usageErrorf("sensor fault: --duration must not be negative")
			}
			if duration > 0 {
				req["duration_s"] = duration.Seconds()
			}
			if param != "" {
				req["param"] = param
			}
			for _, n := range []struct {
				flag, key, val string
				integer        bool
			}{{"value", "value", value, false}, {"register", "at", register, true}, {"mask", "mask", mask, true}, {"rate", "rate_hz", rate, false}} {
				if n.val == "" {
					continue
				}
				if n.integer {
					v, err := strconv.ParseInt(n.val, 0, 32)
					if err != nil || v < 0 {
						return usageErrorf("sensor fault: --%s: not a number: %q", n.flag, n.val)
					}
					req[n.key] = v
					continue
				}
				v, err := strconv.ParseFloat(n.val, 64)
				if err != nil {
					return usageErrorf("sensor fault: --%s: not a number: %q", n.flag, n.val)
				}
				req[n.key] = v
			}
			return sensorSimple(g, cmd.OutOrStdout(), "sensor fault", req, "Injected "+typ+" into "+instance)
		},
	}
	c.Flags().DurationVar(&duration, "duration", 0, "how long the fault lasts, e.g. 2s (default: until cleared)")
	c.Flags().StringVar(&param, "param", "", "the param a stuck, offset or gain fault applies to")
	c.Flags().StringVar(&value, "value", "", "offset or gain amount, drop probability, baud error percent, or a stuck/open/short value")
	c.Flags().StringVar(&register, "register", "", "corrupt: the register (e.g. 0xF9)")
	c.Flags().StringVar(&mask, "mask", "", "corrupt: the XOR mask (e.g. 0x01)")
	c.Flags().StringVar(&rate, "rate", "", "garbage: bytes per second (pod default 100)")
	return c
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ── status ──────────────────────────────────────────────────────────────────

func newSensorStatusCmd(g *globalFlags) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show the running sensors, their values and bus activity, and the stimulus",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return err
			}
			defer closeOut()
			ctx, cancel, t, err := sensorPod(g, "sensor status")
			if err != nil {
				return err
			}
			defer cancel()
			st, err := sensorStatusAll(ctx, t)
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := json.MarshalIndent(st, "", "  ")
				_, err := fmt.Fprintln(out, string(b))
				return err
			}
			printSensorStatus(out, t.label, st)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print the pod's reply as JSON")
	return c
}

// stoppedLine reports a sensor_stop reply ({"stopped":["env"]}).
func stoppedLine(reply map[string]any) string {
	list, _ := reply["stopped"].([]any)
	if len(list) == 0 {
		return "No sensor was running"
	}
	names := make([]string, 0, len(list))
	for _, n := range list {
		names = append(names, fmt.Sprint(n))
	}
	return "Stopped " + strings.Join(names, ", ")
}

// sensorStatusAll reads every page of sensor_status (the pod ends a page with "next" when the
// next instance would not fit the cloud reply cap) into one reply.
func sensorStatusAll(ctx context.Context, t *podTarget) (map[string]any, error) {
	st, err := sensorCommand(ctx, t, "sensor status", map[string]any{"cmd": "sensor_status"})
	if err != nil {
		return nil, err
	}
	list, _ := st["instances"].([]any)
	seen := map[int]bool{0: true}
	for {
		n, ok := st["next"].(float64)
		first := int(n)
		if !ok || first <= 0 || seen[first] {
			break
		}
		seen[first] = true
		page, err := sensorCommand(ctx, t, "sensor status", map[string]any{"cmd": "sensor_status", "first": first})
		if err != nil {
			return nil, err
		}
		more, _ := page["instances"].([]any)
		list = append(list, more...)
		delete(st, "next")
		if v, ok := page["next"]; ok {
			st["next"] = v
		}
	}
	delete(st, "next")
	st["instances"] = list
	return st, nil
}

// faultText is one sensor_status fault: {"type":"no_response","left_s":1.25}.
func faultText(f any) string {
	m, ok := f.(map[string]any)
	if !ok {
		return fmt.Sprint(f)
	}
	parts := []string{fmt.Sprint(m["type"])}
	for _, k := range []string{"param", "value", "at", "mask", "rate_hz"} {
		if v, ok := m[k]; ok && v != nil {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	if m["stimulus"] == true {
		parts = append(parts, "from the stimulus")
	}
	if left, ok := m["left_s"].(float64); ok {
		parts = append(parts, fmt.Sprintf("%.2f s left", left))
	} else {
		parts = append(parts, "until cleared")
	}
	return strings.Join(parts, " ")
}

func printSensorStatus(out io.Writer, via string, st map[string]any) {
	fmt.Fprintf(out, "Sensors on %s (FPGA image %v)\n", via, st["image"])
	if stim, ok := st["stimulus"].(map[string]any); ok && stim["loaded"] == true {
		state := "loaded"
		if stim["running"] == true {
			state = fmt.Sprintf("running, t = %v s", stim["t_s"])
		}
		fmt.Fprintf(out, "Stimulus: %s\n", state)
	}
	list, _ := st["instances"].([]any)
	if len(list) == 0 {
		fmt.Fprintln(out, "No sensor runs.")
		return
	}
	for _, item := range list {
		inst, _ := item.(map[string]any)
		state := "stopped"
		if inst["running"] == true {
			state = "running"
		}
		pins := map[string]any{}
		if p, ok := inst["pins"].(map[string]any); ok {
			for k, v := range p {
				if f, ok := v.(float64); ok && f == float64(int(f)) {
					pins[k] = int(f)
				} else {
					pins[k] = v
				}
			}
		}
		fmt.Fprintf(out, "%v: %v %v, %s, %s", inst["instance"], inst["kind"], inst["name"], state, formatPins(pins))
		if slot, _ := inst["slot"].(string); slot != "" {
			fmt.Fprintf(out, ", slot %s", slot)
		}
		if a, ok := inst["address"]; ok {
			fmt.Fprintf(out, ", address %v", a)
		}
		fmt.Fprintln(out)
		if vals, ok := inst["values"].(map[string]any); ok && len(vals) > 0 {
			keys := make([]string, 0, len(vals))
			for k := range vals {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s=%v", k, vals[k]))
			}
			fmt.Fprintf(out, "  values: %s\n", strings.Join(parts, " "))
		}
		if faults, ok := inst["faults"].([]any); ok && len(faults) > 0 {
			parts := make([]string, 0, len(faults))
			for _, f := range faults {
				parts = append(parts, faultText(f))
			}
			fmt.Fprintf(out, "  faults: %s\n", strings.Join(parts, "; "))
		}
		if v, ok := inst["output"]; ok && v != nil {
			fmt.Fprintf(out, "  output: %v\n", v)
		}
		if bus, ok := inst["bus"].(map[string]any); ok && len(bus) > 0 {
			// transfers is always null (the gateware counts written bytes, not transfers).
			if v, ok := bus["transfers"]; ok && v == nil {
				bus = maps.Clone(bus)
				delete(bus, "transfers")
			}
			b, _ := json.Marshal(bus)
			fmt.Fprintf(out, "  bus: %s\n", b)
		}
	}
}
