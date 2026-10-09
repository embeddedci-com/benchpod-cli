// Command benchpod-cli is the EmbeddedCI bench pod CLI. It talks to a bench pod
// either over its TCP/JSON API (port 8080) or over its USB CDC-ACM serial
// console. The global --connection flag is the single place that says where and
// how to connect, and the transport is inferred from its value:
//
//	--connection 192.168.1.5[:8080]   TCP/JSON API (an address ⇒ wifi).
//	--connection /dev/tty... | COM3   USB serial console, explicit device.
//	--connection usb                  USB serial console, auto-detected.
//	(omitted)                         the default saved by `benchpod set-connection`.
//
// The firmware itself is unauthenticated; `login` authenticates with
// embeddedci-server (device-login flow) for register, deregister and the cloud
// policy commands. Direct firmware commands do not send tokens.
//
// Over USB, status, la voltage, flash (SWD) and the policy/cloud-link settings
// work; set-wifi, show-network, clear-wifi, identity, install-blobs, bootsel and
// dfu always use the USB console. The other commands need the network. The root
// command's help is the user-facing version of this.
package main

import "os"

func main() {
	os.Exit(Execute())
}
