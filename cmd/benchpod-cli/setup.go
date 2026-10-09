package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/embeddedci-com/benchpod-cli/internal/benchpodconfig"
	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

// ── setup ────────────────────────────────────────────────────────────────────
//
// setup walks the getting-started steps (https://www.embeddedci.com/docs/benchpod-getting-started)
// in order and skips each one that is already done: find the pod, get it on the network, set the
// board I/O voltage, register it with embeddedci.com, save the connection. Every step reuses the
// command that does it on its own (discover, set-wifi, la voltage, login, register,
// set-connection); setup only decides what is left to do and asks.

const gettingStartedURL = "https://www.embeddedci.com/docs/benchpod-getting-started"

func newSetupCmd(g *globalFlags) *cobra.Command {
	var (
		laVoltage     string
		ssid          string
		passwordStdin bool
		noRegister    bool
		yes           bool
	)
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Guided first-time setup: find the pod, network, I/O voltage, registration, saved connection",
		Long: "Walk through the getting-started steps for a BenchPod, skipping what is already done:\n\n" +
			"  1. Find the pod over USB and on the LAN (like `benchpod discover`).\n" +
			"  2. Network: when the pod has no address, choose Ethernet (setup waits for its\n" +
			"     DHCP lease) or Wi-Fi (SSID and password, like `benchpod set-wifi`).\n" +
			"  3. Board I/O voltage: 1.8 V or 3.3 V, the DUT's logic level (like\n" +
			"     `benchpod la voltage`). Already set is kept unless --la-voltage is given.\n" +
			"     The pod forgets it on a restart, so when you are signed in and the pod is\n" +
			"     on your account, setup also saves it to the pod's wiring profile on\n" +
			"     embeddedci.com, which the server applies on every connect.\n" +
			"  4. embeddedci.com: an unregistered pod is registered to your account (signing\n" +
			"     in first when needed), and its wiring profile gets the I/O voltage. A pod\n" +
			"     on another account is explained, not touched.\n" +
			"  5. Save the pod's address as the default connection (like `discover --save`).\n\n" +
			"It then prints the next steps: wiring SWD, flashing and running tests.\n\n" +
			"Pass --connection to set up a pod at a known address or USB device instead of\n" +
			"discovering it. With --yes setup never prompts: it fails with a message naming\n" +
			"the flag to pass when it needs an answer. Docs: " + gettingStartedURL,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			opts := setupOpts{ssid: strings.TrimSpace(ssid), passwordStdin: passwordStdin,
				noRegister: noRegister, yes: yes, ethernetWait: 2 * time.Minute}
			if laVoltage != "" {
				mv, err := parseLAVoltage(laVoltage)
				if err != nil {
					return &usageError{err: fmt.Errorf("--la-voltage: %w", err)}
				}
				opts.laVoltageMV = mv
			}
			if passwordStdin && opts.ssid == "" {
				return usageErrorf("--password-stdin needs --ssid")
			}
			ui := &setupUI{in: bufio.NewReader(os.Stdin), out: os.Stdout, yes: yes}
			return runSetup(&realSetupEnv{g: g}, ui, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&laVoltage, "la-voltage", "", "board I/O voltage to set: 1.8V or 3.3V (default: keep the current one, or ask)")
	f.StringVar(&ssid, "ssid", "", "join this Wi-Fi network when the pod has no network address")
	f.BoolVar(&passwordStdin, "password-stdin", false, "read the Wi-Fi password from the first line of stdin (with --ssid)")
	f.BoolVar(&noRegister, "no-register", false, "do not register the pod with embeddedci.com")
	f.BoolVar(&yes, "yes", false, "never prompt: register without asking, and fail with a message when an answer is needed")
	return cmd
}

type setupOpts struct {
	laVoltageMV   int
	ssid          string
	passwordStdin bool
	noRegister    bool
	yes           bool
	ethernetWait  time.Duration
}

// setupPod is one physical pod as setup sees it.
type setupPod struct {
	device   string // USB console device, "" when not seen over USB
	addr     string // network host:port, "" when it has no address
	firmware string
	cloud    cloudState // known=false when the pod could not say
}

// setupEnv is everything setup does to the pod, the account and the config; tests fake it.
type setupEnv interface {
	find() ([]setupPod, error)
	waitForAddress(device string, wait time.Duration) (string, error)
	setWifi(device, ssid, password string) (string, error)
	readPassword() (string, error)
	laVoltage(target string, mv int) (int, error)
	saveLaMV(deviceID string, mv int) (changed bool, err error)
	cloudStatus(addr string) cloudState
	signedIn() string
	login() error
	accountDevices() map[string]string
	register(addr string) error
	save(target string) error
	// latestFirmware is the latest firmware release tag, "" when it could not be read.
	latestFirmware() string
}

// errNeedsAnswer is returned under --yes when a step needs an answer that no flag gave.
var errNeedsAnswer = errors.New("setup needs an answer")

// setupUI asks the questions. Under --yes it never reads: it fails with the flag to pass.
type setupUI struct {
	in  *bufio.Reader
	out io.Writer
	yes bool
}

func (u *setupUI) readLine() (string, error) {
	line, err := u.in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read answer: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// choose asks for one of options (1-based) and returns its index; flagHint names the flag that
// answers it under --yes.
func (u *setupUI) choose(question, flagHint string, options []string) (int, error) {
	if u.yes {
		return 0, fmt.Errorf("%w: %s", errNeedsAnswer, flagHint)
	}
	fmt.Fprintln(u.out, question)
	for i, o := range options {
		fmt.Fprintf(u.out, "  %d) %s\n", i+1, o)
	}
	for tries := 0; tries < 3; tries++ {
		fmt.Fprintf(u.out, "Choose 1-%d: ", len(options))
		ans, err := u.readLine()
		if err != nil {
			return 0, err
		}
		for i := range options {
			if ans == fmt.Sprint(i+1) {
				return i, nil
			}
		}
	}
	return 0, errors.New("no valid answer")
}

// confirm asks a yes/no question; Enter takes def. Under --yes the answer is yes.
func (u *setupUI) confirm(question string, def bool) (bool, error) {
	if u.yes {
		return true, nil
	}
	hint := "[Y/n]"
	if !def {
		hint = "[y/N]"
	}
	fmt.Fprintf(u.out, "%s %s ", question, hint)
	ans, err := u.readLine()
	if err != nil {
		return false, err
	}
	switch strings.ToLower(ans) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// ask reads a free-text answer.
func (u *setupUI) ask(prompt, flagHint string) (string, error) {
	if u.yes {
		return "", fmt.Errorf("%w: %s", errNeedsAnswer, flagHint)
	}
	fmt.Fprint(u.out, prompt)
	return u.readLine()
}

// runSetup is the step logic.
func runSetup(env setupEnv, ui *setupUI, o setupOpts) error {
	out := ui.out
	step := func(n int, title string) { fmt.Fprintf(out, "\n%d/5  %s\n", n, title) }

	// 1. Find the pod.
	step(1, "Find the pod")
	pods, err := env.find()
	if err != nil {
		return err
	}
	switch {
	case len(pods) == 0:
		return errors.New("no BenchPod found over USB or on this LAN. Check the pod is powered and the USB " +
			"cable carries data (many are charge-only), then run setup again; `benchpod discover` " +
			"explains what it sees. A pod you know the address of: `benchpod setup --connection <address>`")
	case len(pods) > 1:
		return fmt.Errorf("found %d BenchPods; set up one at a time with `benchpod setup --connection <address|device>` "+
			"(`benchpod discover` lists them)", len(pods))
	}
	p := pods[0]
	fmt.Fprintf(out, "     Found %s.\n", describeSetupPod(p))
	if latest := env.latestFirmware(); latest != "" && p.firmware != "" && versionLess(p.firmware, latest) {
		// A nudge, not a step: flashing over USB DFU needs the pod replugged afterwards, so the
		// user runs it when it suits them.
		fmt.Fprintf(out, "     Firmware %s is out of date: %s is the latest. Update it with the pod on USB:\n"+
			"       benchpod flash-self --enter-dfu\n"+
			"     (unplug and replug USB-C when it finishes), or later from the web app's BenchPod page.\n",
			strings.TrimPrefix(p.firmware, "v"), strings.TrimPrefix(latest, "v"))
	}

	// 2. Network.
	step(2, "Network")
	if p.addr != "" {
		fmt.Fprintf(out, "     Done: the pod is on the network at %s.\n", p.addr)
	} else {
		if p.device == "" {
			return errors.New("the pod has no network address and is not on USB; connect it over USB and run setup again")
		}
		addr, err := setupNetwork(env, ui, o, p.device)
		if err != nil {
			return err
		}
		p.addr = addr
		fmt.Fprintf(out, "     The pod is on the network at %s.\n", p.addr)
	}

	// 3. Board I/O voltage.
	step(3, "Board I/O voltage")
	if c := env.cloudStatus(p.addr); c.known {
		p.cloud = c
	}
	mv, err := setupLAVoltage(env, ui, o, p.addr, setupProfileDevice(env, p.cloud))
	if err != nil {
		return err
	}

	// 4. embeddedci.com.
	step(4, "embeddedci.com")
	name, err := setupCloud(env, ui, o, p, mv)
	if err != nil {
		return err
	}

	// 5. Save the connection.
	step(5, "Save the connection")
	if err := env.save(p.addr); err != nil {
		return err
	}

	printSetupNextSteps(out, p.addr, name)
	return nil
}

func describeSetupPod(p setupPod) string {
	var parts []string
	if p.device != "" {
		parts = append(parts, "USB "+p.device)
	}
	if p.addr != "" {
		parts = append(parts, "network "+p.addr)
	} else {
		parts = append(parts, "no network address yet")
	}
	if p.firmware != "" {
		parts = append(parts, "firmware "+p.firmware)
	}
	return "a BenchPod: " + strings.Join(parts, ", ")
}

// setupNetwork gets a pod that is only on USB onto the network and returns its host:port.
func setupNetwork(env setupEnv, ui *setupUI, o setupOpts, device string) (string, error) {
	out := ui.out
	useWifi := o.ssid != ""
	if !useWifi {
		i, err := ui.choose("     The pod has no network address yet. How should it connect?",
			"the pod has no network address: plug in Ethernet and run setup again, or pass --ssid <ssid> (and --password-stdin) for Wi-Fi",
			[]string{"Ethernet (plug in a cable; the pod takes a DHCP lease on its own)", "Wi-Fi"})
		if err != nil {
			return "", err
		}
		useWifi = i == 1
	}
	if !useWifi {
		fmt.Fprintf(out, "     Plug the pod's Ethernet port into your network. Waiting up to %s for a DHCP lease...\n", o.ethernetWait)
		addr, err := env.waitForAddress(device, o.ethernetWait)
		if err != nil {
			return "", fmt.Errorf("no DHCP lease within %s: check the cable and that the network hands out addresses, "+
				"then run setup again (or use Wi-Fi: `benchpod setup --ssid <ssid>`): %w", o.ethernetWait, err)
		}
		return addr, nil
	}

	ssid := o.ssid
	if ssid == "" {
		var err error
		if ssid, err = ui.ask("     Wi-Fi network name (SSID): ", "pass --ssid"); err != nil {
			return "", err
		}
		if ssid == "" {
			return "", errors.New("no SSID given")
		}
	}
	var pw string
	var err error
	if o.passwordStdin {
		pw, err = ui.readLine()
		if err == nil && pw == "" {
			err = errors.New("empty password on stdin")
		}
	} else {
		if ui.yes {
			return "", fmt.Errorf("%w: pass the Wi-Fi password with --password-stdin", errNeedsAnswer)
		}
		pw, err = env.readPassword()
	}
	if err != nil {
		return "", err
	}
	addr, err := env.setWifi(device, ssid, pw)
	if err != nil {
		return "", err
	}
	if addr == "" {
		fmt.Fprintln(out, "     Waiting for the pod to report its Wi-Fi address...")
		if addr, err = env.waitForAddress(device, 30*time.Second); err != nil {
			return "", fmt.Errorf("the pod did not get an address on %q: check the password and that the network is 2.4 GHz, "+
				"then run setup again (`benchpod show-network` shows what it sees): %w", ssid, err)
		}
	}
	return addr, nil
}

// setupProfileDevice returns the pod's cloud device id when this machine is signed in and the pod
// is registered to that account, so its wiring profile on embeddedci.com can be written; "" otherwise.
func setupProfileDevice(env setupEnv, c cloudState) string {
	id := strings.TrimSpace(c.DeviceID)
	if !c.known || !c.Configured || id == "" || env.signedIn() == "" {
		return ""
	}
	if _, ok := env.accountDevices()[id]; !ok {
		return ""
	}
	return id
}

// setupLAVoltage keeps a voltage that is already set unless --la-voltage asks for one, and returns
// the pod's voltage. The pod forgets it on a restart, so when profileDevice is set (signed in, pod on
// the account) it is also saved to the pod's wiring profile on embeddedci.com: the server applies it
// on every connect, and the web app's setup checklist ticks. A failed save only warns.
func setupLAVoltage(env setupEnv, ui *setupUI, o setupOpts, target, profileDevice string) (int, error) {
	out := ui.out
	mv := o.laVoltageMV
	if mv == 0 {
		cur, err := env.laVoltage(target, 0)
		if err != nil {
			return 0, err
		}
		if cur != 0 {
			var saveErr error
			done := fmt.Sprintf("     Done: already %s.", formatMV(cur))
			if profileDevice != "" {
				var changed bool
				if changed, saveErr = env.saveLaMV(profileDevice, cur); saveErr == nil && changed {
					done = fmt.Sprintf("     Done: already %s on the pod. Saved it to its wiring profile on embeddedci.com.", formatMV(cur))
				}
			}
			fmt.Fprintln(out, done)
			if saveErr != nil {
				warnProfileSave(out, cur, saveErr)
			}
			fmt.Fprintln(out, "     Change it with `benchpod la voltage 1.8V|3.3V` if your DUT differs.")
			return cur, nil
		}
		i, err := ui.choose("     Which I/O voltage does your target board (DUT) use? The pod's LA pins, UART and SWD use it.",
			"the board I/O voltage is not set: pass --la-voltage 3.3V or --la-voltage 1.8V (the DUT's logic level)",
			[]string{"3.3 V (most boards, including the NUCLEO examples)", "1.8 V"})
		if err != nil {
			return 0, err
		}
		mv = []int{3300, 1800}[i]
	}
	got, err := env.laVoltage(target, mv)
	if err != nil {
		return 0, err
	}
	if profileDevice == "" {
		fmt.Fprintf(out, "     Set to %s on the pod.\n", formatMV(got))
		return got, nil
	}
	changed, err := env.saveLaMV(profileDevice, got)
	switch {
	case err != nil:
		fmt.Fprintf(out, "     Set to %s on the pod.\n", formatMV(got))
		warnProfileSave(out, got, err)
	case changed:
		fmt.Fprintf(out, "     Set to %s on the pod and saved to its wiring profile on embeddedci.com.\n", formatMV(got))
	default:
		fmt.Fprintf(out, "     Set to %s on the pod; its wiring profile on embeddedci.com already has it.\n", formatMV(got))
	}
	return got, nil
}

// warnProfileSave says the voltage is on the pod but not in its stored profile.
func warnProfileSave(out io.Writer, mv int, err error) {
	fmt.Fprintf(out, "     Warning: could not save it to the pod's wiring profile on embeddedci.com: %v\n", err)
	fmt.Fprintf(out, "     The pod keeps %s until it restarts; set it on the web app's Wiring tab to keep it.\n", formatMV(mv))
}

// setupCloud registers an unregistered pod (unless --no-register) and says whose account a
// registered one is on. It returns the pod's device name on the signed-in account, "" if unknown.
//
// mv is the pod's I/O voltage from step 3: a pod registered here gets it saved to its new wiring
// profile on embeddedci.com.
func setupCloud(env setupEnv, ui *setupUI, o setupOpts, p setupPod, mv int) (string, error) {
	out := ui.out
	c := p.cloud
	if !c.known {
		fmt.Fprintln(out, "     Skipped: this firmware does not report its registration. Update it (`benchpod flash-self`) and run setup again.")
		return "", nil
	}
	if c.Configured {
		who := env.signedIn()
		if who == "" {
			fmt.Fprintln(out, "     Done: the pod is registered to an embeddedci.com account. If it was set up for you, sign in")
			fmt.Fprintln(out, "     at https://www.embeddedci.com (or `benchpod login`) with the address your activation email")
			fmt.Fprintln(out, "     was sent to, and it is on your BenchPod page.")
			return "", nil
		}
		if name, ok := env.accountDevices()[strings.TrimSpace(c.DeviceID)]; ok {
			fmt.Fprintf(out, "     Done: registered to your account (%s) as %s.\n", who, name)
			return name, nil
		}
		fmt.Fprintf(out, "     The pod is registered to a different embeddedci.com account than %s, so setup leaves it alone.\n", who)
		fmt.Fprintln(out, "     If it was set up for you, sign in with the address your activation email was sent to:")
		fmt.Fprintln(out, "       benchpod login --force")
		fmt.Fprintln(out, "     A new account does not get the pod: it stays on the account it is registered to.")
		return "", nil
	}

	if o.noRegister {
		fmt.Fprintln(out, "     Skipped (--no-register): the pod works over the LAN without an account.")
		fmt.Fprintln(out, "     Register it later with `benchpod login` and `benchpod register`.")
		return "", nil
	}
	ok, err := ui.confirm("     The pod is not registered. Register it to your embeddedci.com account (web app, CI, remote use)?", true)
	if err != nil {
		return "", err
	}
	if !ok {
		fmt.Fprintln(out, "     Skipped. Register it later with `benchpod register`.")
		return "", nil
	}
	if env.signedIn() == "" {
		fmt.Fprintln(out, "     Signing in to embeddedci.com first.")
		if err := env.login(); err != nil {
			return "", err
		}
	}
	if err := env.register(p.addr); err != nil {
		return "", err
	}
	if c := env.cloudStatus(p.addr); c.known && c.DeviceID != "" {
		id := strings.TrimSpace(c.DeviceID)
		if name, ok := env.accountDevices()[id]; ok {
			if mv != 0 {
				if _, err := env.saveLaMV(id, mv); err != nil {
					warnProfileSave(out, mv, err)
				} else {
					fmt.Fprintf(out, "     Saved the I/O voltage (%s) to its wiring profile on embeddedci.com.\n", formatMV(mv))
				}
			}
			return name, nil
		}
	}
	return "", nil
}

func printSetupNextSteps(out io.Writer, addr, name string) {
	addr = strings.TrimSuffix(addr, ":8080") // the SDK's default port
	fmt.Fprintln(out, "\nThe pod is set up. Next steps:")
	fmt.Fprintln(out, "  • Wire SWD to your target: SWCLK on LA11, SWDIO on LA12, and GND. The DUT's reset")
	fmt.Fprintln(out, "    line goes to the pod's reset pin.")
	fmt.Fprintln(out, "  • Flash it with OpenOCD (the pod is the CMSIS-DAP probe), e.g. for a NUCLEO-F446RE:")
	fmt.Fprintln(out, "      benchpod flash --swclk 11 --swdio 12 --nreset --target target/stm32f4x.cfg --file firmware.elf")
	if name != "" {
		fmt.Fprintf(out, "  • Run tests: `pytest --benchpod-connection=%s` (or embeddedci:%s through embeddedci.com).\n", addr, name)
	} else {
		fmt.Fprintf(out, "  • Run tests: `pytest --benchpod-connection=%s`.\n", addr)
	}
	fmt.Fprintf(out, "  • Guide: %s\n", gettingStartedURL)
}

// ── the real environment ─────────────────────────────────────────────────────

type realSetupEnv struct {
	g *globalFlags
	// account caches accountDevices for one run (login and register clear it).
	account    map[string]string
	accountSet bool
}

// with returns a copy of the global flags aimed at target.
func (e *realSetupEnv) with(target string) *globalFlags {
	g := *e.g
	g.connection = target
	return &g
}

func (e *realSetupEnv) find() ([]setupPod, error) {
	if raw := strings.TrimSpace(e.g.connection); raw != "" {
		spec, err := classifyConnection(raw)
		if err != nil {
			return nil, err
		}
		if spec.IsNetwork() {
			dp := discoveredPod{addr: spec.Addr}
			checkPod(&dp)
			if !dp.reachable {
				return nil, fmt.Errorf("no BenchPod answers at %s: %s", spec.Addr, firstLine(dp.healthErr))
			}
			return []setupPod{{addr: spec.Addr, cloud: dp.cloud}}, nil
		}
		serialPods, _, err := serialconsole.ProbeSerial(spec.Device, 2*time.Second)
		if err != nil {
			return nil, err
		}
		var pods []setupPod
		for _, sp := range serialPods {
			if spec.Device == "" || sp.Device == spec.Device {
				pods = append(pods, setupPodFromSerial(sp))
			}
		}
		return pods, nil
	}

	fmt.Println("     Looking over USB and on the LAN (mDNS)...")
	serialPods, _, serialErr := serialconsole.ProbeSerial("", 2*time.Second)
	netPods, netErr := discoverPods(3 * time.Second)
	if serialErr != nil && netErr != nil {
		return nil, fmt.Errorf("could not look for a pod: USB: %v; LAN: %v", serialErr, netErr)
	}
	for i := range netPods {
		checkPod(&netPods[i])
	}
	correlate(netPods, serialPods)
	return mergeSetupPods(serialPods, netPods), nil
}

// mergeSetupPods turns discover's two lists into one entry per physical pod. A network pod that
// does not answer its API is left out (a stale mDNS record is not a pod to set up).
func mergeSetupPods(serialPods []serialconsole.SerialPod, netPods []discoveredPod) []setupPod {
	var pods []setupPod
	for _, sp := range serialPods {
		p := setupPodFromSerial(sp)
		for _, np := range netPods {
			if np.sameAs == sp.Device && np.reachable {
				p.addr = np.addr
				if np.cloud.known {
					p.cloud = np.cloud
				}
			}
		}
		pods = append(pods, p)
	}
	for _, np := range netPods {
		if np.sameAs == "" && np.reachable {
			pods = append(pods, setupPod{addr: np.addr, cloud: np.cloud})
		}
	}
	return pods
}

func setupPodFromSerial(sp serialconsole.SerialPod) setupPod {
	p := setupPod{device: sp.Device, firmware: sp.Firmware}
	if sp.Addressed() {
		p.addr = benchpodconfig.EnsurePort(strings.TrimSpace(sp.IP))
	}
	if sp.CloudKnown {
		p.cloud = cloudState{known: true, Configured: sp.Registered, State: sp.CloudState, DeviceID: sp.DeviceID}
	}
	return p
}

func (e *realSetupEnv) waitForAddress(device string, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		pods, _, err := serialconsole.ProbeSerial(device, 2*time.Second)
		if err == nil {
			for _, sp := range pods {
				if sp.Device == device && sp.Addressed() {
					return benchpodconfig.EnsurePort(strings.TrimSpace(sp.IP)), nil
				}
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return "", err
			}
			return "", errors.New("the pod still reports no address")
		}
		time.Sleep(3 * time.Second)
	}
}

func (e *realSetupEnv) setWifi(device, ssid, password string) (string, error) {
	ip, err := runSetWifi(e.with(device), ssid, password, false)
	if err != nil || ip == "" {
		return "", err
	}
	return benchpodconfig.EnsurePort(ip), nil
}

func (e *realSetupEnv) readPassword() (string, error) { return resolveWifiPassword("", false) }

func (e *realSetupEnv) laVoltage(target string, mv int) (int, error) {
	_, rep, err := laVoltageDo(e.with(target), mv)
	return rep.MV, err
}

func (e *realSetupEnv) cloudStatus(addr string) cloudState {
	dp := discoveredPod{addr: addr}
	checkPod(&dp)
	return dp.cloud
}

func (e *realSetupEnv) signedIn() string {
	who, _ := currentSession("", "")
	return who
}

func (e *realSetupEnv) login() error {
	e.account, e.accountSet = nil, false
	return runLogin(defaultServerURL, "", false)
}

func (e *realSetupEnv) accountDevices() map[string]string {
	if !e.accountSet {
		e.account, e.accountSet = accountDevices(), true
	}
	return e.account
}

func (e *realSetupEnv) register(addr string) error {
	e.account, e.accountSet = nil, false
	return runRegister(e.with(addr), registerOptions{serverURL: defaultServerURL, wait: 30 * time.Second})
}

func (e *realSetupEnv) saveLaMV(deviceID string, mv int) (bool, error) {
	return saveLaMVToProfile(deviceID, mv)
}

func (e *realSetupEnv) save(target string) error { return runSetConnection(e.g, target) }

func (e *realSetupEnv) latestFirmware() string { return latestFirmwareRelease() }
