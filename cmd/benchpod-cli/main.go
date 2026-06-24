// Command benchpod-cli is the EmbeddedCI bench pod CLI. It talks to a bench pod
// either over its TCP/JSON API (port 8080) or over its USB CDC-ACM serial
// console. The global --connection flag is the single place that says where and
// how to connect, and the transport is inferred from its value:
//
//	--connection 192.168.1.5[:8080]   TCP/JSON API (an address ⇒ wifi).
//	--connection /dev/tty... | COM3   USB serial console, explicit device.
//	--connection serial               USB serial console, auto-detected (USB VID 2E8A).
//	(omitted)                         the default saved by `benchpod set-connection`.
//
// The firmware itself is unauthenticated; the `login` subcommand is independent
// of the device path and authenticates with embeddedci-server (device-login
// flow) for the future cloud features. Direct firmware commands do not send
// tokens.
//
// Only `flash` (SWD) is implemented over the serial console today; the other
// TCP/JSON commands reject a serial connection with a clear message. The
// set/show/clear-wifi and bootsel subcommands always use the serial console
// regardless of --connection (a device path still selects the port).
package main

import "os"

func main() {
	os.Exit(Execute())
}
