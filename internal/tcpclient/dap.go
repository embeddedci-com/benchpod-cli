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
// The handshake is the dap_start arm-then-switch sequence (ack
// {"status":"ok","data":"dap ready"} in JSON mode, then raw framed packets on the
// same socket), implemented by startRawMode. The caller owns closing the returned
// connection; a zero-length frame or closing the socket returns the pod to JSON
// mode.
//
// Target reset is NOT passed here. Since pod rev3 nRESET is the pod's own
// /NRST_CONTROL pin (J1 pin 22), driven by the firmware behind CMSIS-DAP
// SWJ_PINS — it is no longer an LA channel the host has to name.
func (c *Client) DAPStart(ctx context.Context, swclk, swdio int) (net.Conn, error) {
	return c.DAPStartWith(ctx, swclk, swdio, DAPOptions{})
}

// DAPOptions are the optional dap_start fields. Pod firmware that predates them ignores them.
type DAPOptions struct {
	// PacketSize / PacketCount ask the pod to advertise larger CMSIS-DAP packets and several in
	// flight (DAP_Info); 0 keeps the pod's default of 256 x 1. Each packet is a host + network
	// round trip, so fewer, larger packets flash faster, most of all over the cloud.
	PacketSize  int
	PacketCount int
	// WaitMS makes the pod wait, up to this long, for the target to answer SWD before it acks:
	// for a target whose rail was just switched on (an STM32 NUCLEO needs ~2.1 s).
	WaitMS int
}

// DAPStartWith is DAPStart with the optional dap_start fields.
func (c *Client) DAPStartWith(ctx context.Context, swclk, swdio int, o DAPOptions) (net.Conn, error) {
	extra := map[string]any{}
	if o.PacketSize > 0 {
		extra["packet_size"] = o.PacketSize
	}
	if o.PacketCount > 0 {
		extra["packet_count"] = o.PacketCount
	}
	if o.WaitMS > 0 {
		extra["wait_ms"] = o.WaitMS
	}
	return c.startRawMode(ctx, "dap_start", swclk, swdio, extra)
}
