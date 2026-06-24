package main

import (
	"log"
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
			"API; a device path (/dev/tty..., COM3) or the keyword `serial` uses the USB\n" +
			"serial console. Omit it to use the default saved by `benchpod set-connection`.\n" +
			"Today only `flash` works over serial; the wifi-* and bootsel commands always\n" +
			"use the serial console regardless.",
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
		`how to reach the pod: an address (192.168.1.5[:8080]), a device path (/dev/tty..., COM3), or "serial" to auto-detect USB (default: the saved set-connection target)`)
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
		newRegisterCmd(g),
		newSetConnectionCmd(g),
		newPingCmd(g),
		newStatusCmd(g),
		newGenerateCmd(g),
		newCaptureCmd(g),
		newStreamCmd(g),
		newMeasureCmd(g),
		newTestCmd(g),
		newSetGPIOCmd(g),
		newStepGPIOCmd(g),
		newFlashCmd(g),
		newSetWifiCmd(g),
		newShowWifiCmd(g),
		newClearWifiCmd(g),
		newBootselCmd(g),
	)
	return root
}

// Execute builds and runs the root command, translating any error into a
// process exit code. Commands log their own diagnostics (and Cobra's output is
// silenced), so a non-nil error here just needs a final line and code 1.
func Execute() int {
	log.SetFlags(0)
	log.SetPrefix("[benchpod] ")
	if err := newRootCmd().Execute(); err != nil {
		log.Printf("%v", err)
		return 1
	}
	return 0
}
