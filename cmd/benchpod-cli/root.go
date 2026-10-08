package main

import (
	"log"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// globalFlags holds the persistent (global) flags shared by every subcommand.
// They are bound through Viper so BENCHPOD_* environment variables also work
// (e.g. BENCHPOD_CONNECTION=serial), with precedence flag > env > default.
type globalFlags struct {
	connection     string
	configFile     string
	outputFilename string
	timeout        time.Duration
}

// effectiveTimeout returns the explicit --timeout when set (> 0), else the
// command's own default. Each command knows a sensible default deadline; the
// global flag is an override.
func (g *globalFlags) effectiveTimeout(def time.Duration) time.Duration {
	if g.timeout > 0 {
		return g.timeout
	}
	return def
}

// version is the CLI version. It defaults to "dev" for local builds and is
// overridden at release time via -ldflags "-X main.version=<tag>" (see the
// GoReleaser config and the release workflow).
var version = "dev"

// newRootCmd builds the benchpod root command, wires the persistent flags
// through Viper, and registers all subcommands.
func newRootCmd() *cobra.Command {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:     "benchpod",
		Version: version,
		Short:   "EmbeddedCI bench pod CLI",
		Long: "EmbeddedCI bench pod CLI.\n\n" +
			"--connection says where and how to reach the bench pod, and the transport\n" +
			"is inferred from its value: an address (192.168.1.5[:8080]) uses the TCP/JSON\n" +
			"API; a device path (/dev/tty..., COM3) or the keyword `usb` uses the pod's\n" +
			"USB console. Omit it to use the default saved by `benchpod set-connection`.\n" +
			"`embeddedci:<name>` names a pod on embeddedci.com: lan-policy, sig-policy and\n" +
			"deregister take it; the other commands need the network or USB.\n" +
			"Over USB, `flash` (SWD), `status`, `la voltage`, `lan-policy`, `sig-policy`,\n" +
			"`cloud ca` and `cloud proxy` work; the other network commands refuse a USB\n" +
			"connection. `identity`, `install-blobs`, `set-wifi`, `show-network`,\n" +
			"`clear-wifi`, `bootsel` and `dfu` always use the USB console. `flash-self`\n" +
			"reflashes the pod's own firmware over USB DFU (STM32) via dfu-util, independent\n" +
			"of --connection. `lan-policy` and `sig-policy` also work over embeddedci.com\n" +
			"with --device-name.\n\n" +
			exitCodesHelp,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Apply Viper precedence (flag > env > default) into g before any RunE.
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			g.connection = viper.GetString("connection")
			g.configFile = viper.GetString("config-file")
			g.outputFilename = viper.GetString("output-filename")
			g.timeout = viper.GetDuration("timeout")
			return nil
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&g.connection, "connection", "",
		`how to reach the pod: an address (192.168.1.5[:8080]), a device path (/dev/tty..., COM3), "usb" to auto-detect the pod over USB, or embeddedci:<name> for a pod on embeddedci.com where a command supports it (default: the saved set-connection target)`)
	pf.StringVar(&g.configFile, "config-file", "", "path to config file")
	pf.StringVar(&g.outputFilename, "output-filename", "", "write command output to this file instead of stdout")
	pf.DurationVar(&g.timeout, "timeout", 0, "overall command deadline (0 = per-command default)")

	viper.SetEnvPrefix("BENCHPOD")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()
	for _, name := range []string{"connection", "config-file", "output-filename", "timeout"} {
		_ = viper.BindPFlag(name, pf.Lookup(name))
	}

	root.AddCommand(
		newLoginCmd(g),
		newLogoutCmd(g),
		newRegisterCmd(g),
		newDeregisterCmd(g),
		newSetConnectionCmd(g),
		newDiscoverCmd(g),
		newPingCmd(g),
		newStatusCmd(g),
		newGenerateCmd(g),
		newCaptureCmd(g),
		newStreamCmd(g),
		newMeasureCmd(g),
		newTestCmd(g),
		newLACmd(g),
		newFlashCmd(g),
		newFlashSelfCmd(g),
		newInstallBlobsCmd(g),
		newSPIFlashCmd(g),
		newSetWifiCmd(g),
		newShowNetworkCmd(g),
		newClearWifiCmd(g),
		newBootselCmd(g),
		newDfuCmd(g),
		newLanPolicyCmd(g),
		newSigPolicyCmd(g),
		newIdentityCmd(g),
		newCloudCmd(g),
	)
	return root
}

// Execute builds and runs the root command, translating any error into a
// process exit code (exitCode). Commands log their own diagnostics (and Cobra's
// output is silenced), so a non-nil error here just needs a final line.
func Execute() int {
	log.SetFlags(0)
	log.SetPrefix("[benchpod] ")
	return run(os.Args[1:])
}

// run runs the CLI with args and returns its exit code.
func run(args []string) int {
	root := newRootCmd()
	markUsageErrors(root)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		log.Printf("%v", withRefusalHint(err))
		return exitCode(err)
	}
	return exitOK
}
