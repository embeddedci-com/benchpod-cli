package tcpclient

import (
	"context"
	"net"
)

// DAPStart sends a dap_start command and, on success, hands back the live TCP
// connection now switched to the firmware's length-framed CMSIS-DAP packet
// protocol (PROTO_DAP).
//
// This is the fast flash/debug path. Instead of tunnelling per-bit OpenOCD
// remote_bitbang (one byte per SWCLK edge, blocking on every ACK sample), the
// pod runs a real CMSIS-DAP command processor locally and the host batches whole
// DAP transfers (DAP_Transfer / DAP_TransferBlock). The caller bridges this
// connection to OpenOCD's `cmsis-dap backend tcp` adapter via
// internal/openocd.BridgeDAP, which translates between OpenOCD's framing and the
// pod's 2-byte length frames. See bench-pod-firmware/docs/dap-over-tunnel.md.
//
// The handshake mirrors SWDStart exactly (ack {"status":"ok","data":"dap ready"}
// in JSON mode, then raw framed packets on the same socket), so it shares
// startRawMode. The caller owns closing the returned connection; a zero-length
// frame or closing the socket returns the pod to JSON mode.
//
// nreset is the optional target-reset LA channel; pass nil when the pod does not
// own target reset.
func (c *Client) DAPStart(ctx context.Context, swclk, swdio int, nreset *int) (net.Conn, error) {
	return c.startRawMode(ctx, "dap_start", swclk, swdio, nreset)
}
