// Command benchpod-cli is the EmbeddedCI BenchPod CLI. It talks to a pod over its
// network TCP/JSON API (port 8080), over its USB console, or through embeddedci.com.
// The global --connection flag is the single place that says where and how to
// connect, and the transport is inferred from its value:
//
//	--connection 192.168.1.5[:8080]   network TCP/JSON API.
//	--connection /dev/tty... | COM3   USB console, explicit device.
//	--connection usb                  USB console, auto-detected ("serial" is a legacy alias).
//	--connection embeddedci:<name>    through embeddedci.com (single-reply JSON commands).
//	(omitted)                         the default saved by `benchpod set-connection`.
//
// The firmware itself is unauthenticated; `login` authenticates with
// embeddedci-server (device-login flow) for register, deregister, the cloud
// policy commands and embeddedci: connections (BENCHPOD_API_KEY also works for
// those). Direct firmware commands on the network or USB do not send tokens.
//
// The root command's help says which command works over which connection.
package main

import "os"

func main() {
	os.Exit(Execute())
}
