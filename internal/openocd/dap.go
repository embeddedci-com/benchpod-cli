package openocd

// CMSIS-DAP-over-TCP bridge. Where Bridge tunnels OpenOCD's per-bit
// remote_bitbang stream verbatim, BridgeDAP drives OpenOCD's `cmsis-dap backend
// tcp` adapter: OpenOCD runs the CMSIS-DAP host stack (batched DAP_Transfer /
// DAP_TransferBlock, posted reads, WAIT retries, flash loaders) and ships whole
// CMSIS-DAP packets, which the pod executes locally on the SWD wire. That turns
// thousands of per-bit round-trips into one round-trip per DAP command.
//
// The only wire difference between the two sides is the per-packet header, so
// the bridge is a small framing translation:
//
//	OpenOCD cmsis_dap_tcp : [sig u32 "DAP"][len u16][type u8][rsv u8][payload]
//	pod PROTO_DAP tunnel  : [len u16][payload]
//
// Both carry a verbatim CMSIS-DAP v1 packet (≤ DAP_PACKET_SIZE). See
// bench-pod-firmware/docs/dap-over-tunnel.md and OpenOCD's cmsis_dap_tcp backend.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
)

const (
	// dapTCPSignature is OpenOCD's cmsis_dap_tcp packet signature: "DAP\0" as a
	// little-endian u32.
	dapTCPSignature = 0x00504144
	// dapTCPHeaderLen is the size of OpenOCD's framing header (signature, length,
	// packet_type, reserved).
	dapTCPHeaderLen = 8
	// dapPktResponse is the packet_type OpenOCD expects on device->host frames.
	dapPktResponse = 0x02
	// dapMaxPacket mirrors DAP_PACKET_SIZE in the firmware (dap.h) and the pod's
	// 2-byte frame limit; OpenOCD learns it from DAP_Info.
	dapMaxPacket = 256
)

// dapStrategy bridges OpenOCD's `cmsis-dap backend tcp` adapter to the pod's
// length-framed PROTO_DAP stream, translating the per-packet header in each
// direction. Leaving DAP mode is handled by runBridge closing the connection
// (the firmware disarms on disconnect), so no in-band leave frame is sent.
func dapStrategy() bridgeStrategy {
	return bridgeStrategy{
		adapterArgs: func(port int) []string {
			return []string{
				"-c", "adapter driver cmsis-dap",
				"-c", "cmsis-dap backend tcp",
				"-c", "cmsis-dap tcp host 127.0.0.1",
				"-c", "cmsis-dap tcp port " + strconv.Itoa(port),
				"-c", "transport select swd",
			}
		},
		copyToPod:   copyOpenOCDToPod,
		copyFromPod: copyPodToOpenOCD,
	}
}

// BridgeDAP runs OpenOCD against podConn, which must already be in PROTO_DAP mode
// (dap_start handshake done). It is identical to Bridge except OpenOCD is pointed
// at its cmsis-dap TCP backend and the byte pumps translate between OpenOCD's
// 8-byte DAP header and the pod's 2-byte length frame. Requires an OpenOCD build
// with the cmsis_dap_tcp backend — see SupportsCMSISDAPTCP.
func BridgeDAP(ctx context.Context, bin string, args []string, podConn io.ReadWriteCloser, stdout, stderr io.Writer) error {
	return runBridge(ctx, bin, dapStrategy(), args, podConn, stdout, stderr)
}

// copyOpenOCDToPod reads OpenOCD cmsis_dap_tcp request frames (8-byte header +
// payload) and rewrites each as the pod's 2-byte length-framed packet. It
// returns when src reaches EOF or either side errors.
func copyOpenOCDToPod(dst io.Writer, src io.Reader) error {
	hdr := make([]byte, dapTCPHeaderLen)
	for {
		if _, err := io.ReadFull(src, hdr); err != nil {
			return err
		}
		if sig := binary.LittleEndian.Uint32(hdr[0:4]); sig != dapTCPSignature {
			return fmt.Errorf("openocd cmsis-dap tcp: bad packet signature 0x%08x", sig)
		}
		n := binary.LittleEndian.Uint16(hdr[4:6])
		// hdr[6] (packet_type) and hdr[7] (reserved) carry no info the pod needs.
		if int(n) > dapMaxPacket {
			return fmt.Errorf("openocd cmsis-dap tcp: request length %d exceeds %d", n, dapMaxPacket)
		}
		frame := make([]byte, 2+int(n))
		frame[0] = byte(n)
		frame[1] = byte(n >> 8)
		if _, err := io.ReadFull(src, frame[2:]); err != nil {
			return err
		}
		if _, err := dst.Write(frame); err != nil {
			return err
		}
	}
}

// copyPodToOpenOCD reads the pod's 2-byte length-framed responses and rewrites
// each as an OpenOCD cmsis_dap_tcp response frame (8-byte header + payload). It
// returns when src reaches EOF or either side errors.
func copyPodToOpenOCD(dst io.Writer, src io.Reader) error {
	lenHdr := make([]byte, 2)
	for {
		if _, err := io.ReadFull(src, lenHdr); err != nil {
			return err
		}
		n := int(lenHdr[0]) | int(lenHdr[1])<<8
		if n > dapMaxPacket {
			return fmt.Errorf("pod dap frame length %d exceeds %d", n, dapMaxPacket)
		}
		out := make([]byte, dapTCPHeaderLen+n)
		binary.LittleEndian.PutUint32(out[0:4], dapTCPSignature)
		binary.LittleEndian.PutUint16(out[4:6], uint16(n))
		out[6] = dapPktResponse
		// out[7] (reserved) stays zero.
		if _, err := io.ReadFull(src, out[dapTCPHeaderLen:]); err != nil {
			return err
		}
		if _, err := dst.Write(out); err != nil {
			return err
		}
	}
}

// SupportsCMSISDAPTCP reports whether bin is an OpenOCD build with the
// cmsis_dap_tcp backend (added post-0.12.0). It runs OpenOCD through the config
// stage with the cmsis-dap TCP backend selected and shuts down before init, so
// no probe or target is touched: a clean exit means the backend exists, a
// non-zero exit means OpenOCD rejected `cmsis-dap backend tcp` (an older build
// reports "invalid argument tcp to cmsis-dap backend"). err is non-nil only when
// the binary could not be run at all.
func SupportsCMSISDAPTCP(ctx context.Context, bin string) (bool, error) {
	cmd := commandContext(ctx, bin,
		"-c", "adapter driver cmsis-dap",
		"-c", "cmsis-dap backend tcp",
		"-c", "cmsis-dap tcp port 4441",
		"-c", "shutdown")
	if _, err := cmd.CombinedOutput(); err != nil {
		// An ExitError just means the backend isn't supported; surface only a
		// genuine "couldn't start the process" failure to the caller.
		if isStartFailure(err) {
			return false, fmt.Errorf("run openocd (%s): %w", bin, err)
		}
		return false, nil
	}
	return true, nil
}

// isStartFailure distinguishes "OpenOCD ran and exited non-zero" (an
// *exec.ExitError, which for the probe just means the backend is unsupported)
// from "OpenOCD could not be started at all" (binary missing/not executable).
func isStartFailure(err error) bool {
	var exitErr *exec.ExitError
	return !errors.As(err, &exitErr)
}
