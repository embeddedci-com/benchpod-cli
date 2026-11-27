package tcpclient

import (
	"context"
	"errors"
	"strings"

	"github.com/embeddedci-com/benchpod-cli/internal/errcatalog"
)

// PodError is the pod refusing a command, as opposed to a transport failure: an error reply
// from the JSON API ({"status":"error","code":"BP0031","args":{...}}, or a refusal without a code,
// {"status":"error","message":"..."}) or, over the USB console, a command's "<cmd> error <why>"
// line. It is the one error type for a refusal across the CLI.
//
// Cmd is the console command a USB refusal answers ("lan-policy"); it is "" for a JSON reply.
type PodError struct {
	Cmd string
	// Reason is the text to show: for a coded refusal the catalog's message filled with Args
	// (firmware 4.0 sends no message with a code), else the pod's own text.
	Reason string
	// Code and Args: the pod's stable error code (BP0001...) and its values
	// (benchpod-firmware errors/catalog.toml); "" / nil for a refusal without a code.
	Code string
	Args map[string]any
	// Addr is the network address (host:port as dialed from) that refused, for a refusal a Client
	// or Session read; "" otherwise (the USB console, embeddedci.com).
	Addr string
}

func (e *PodError) Error() string {
	if e.Cmd == "" {
		return e.Reason
	}
	return e.Cmd + ": " + e.Reason
}

// RefusalKind is what a refusal means for the caller, from its code. The exit code follows it.
type RefusalKind int

const (
	// Refused is any other refusal: a bad argument, a missing feature, a failed operation, and
	// every refusal without a code (the client never reads a refusal's text).
	Refused RefusalKind = iota
	// Locked is the LAN refusing a command: the LAN policy (BP0040), or a computer that is not
	// paired with the pod (BP0081).
	Locked
	// Busy is the pod or one of its engines in use: a lease, the PSRAM bus, another session.
	Busy
	// Forbidden is a cloud tunnel without the role a command needs (BP0045).
	Forbidden
	// FirmwareTooOld is a pod whose firmware or gateware lacks what the command needs: update
	// the pod (BP0002, BP0017, BP0060).
	FirmwareTooOld
	// ClientTooOld is a pod that needs a newer client (BP0030; it arrives as an
	// *UpdateRequiredError).
	ClientTooOld
)

// codeKinds is the one table from a refusal's stable code (benchpod-firmware errors/catalog.toml,
// internal/errcatalog) to its kind, for every code in the catalog; the exit code follows the kind,
// and the CLI's hint for a coded refusal is the catalog's fix.cli. A test keeps it in step with
// the catalog. A code the table does not have (newer firmware) is Refused.
var codeKinds = map[string]RefusalKind{
	"BP0001": Refused,        // la_voltage_not_set
	"BP0002": FirmwareTooOld, // unknown_command
	"BP0003": Busy,           // i2c_session_open
	"BP0004": Busy,           // spi_session_open
	"BP0005": Busy,           // bus_engine_busy
	"BP0006": Refused,        // missing_argument
	"BP0007": Refused,        // wiring_profile_not_found
	"BP0008": Refused,        // wiring_bad_name
	"BP0009": Refused,        // wiring_revision_conflict
	"BP0010": Refused,        // wiring_store_full
	"BP0011": Refused,        // wiring_profile_too_large
	"BP0012": Refused,        // wiring_not_json_object
	"BP0013": Refused,        // wiring_delete_default
	"BP0014": Refused,        // wiring_upload_out_of_order
	"BP0015": Refused,        // wiring_storage_failed
	"BP0016": Busy,           // storage_busy
	"BP0017": FirmwareTooOld, // gateware_too_old
	"BP0018": Refused,        // uart_same_pin
	"BP0019": Refused,        // uart_half_duplex_two_pins
	"BP0020": Refused,        // uart_entry_no_reset_pin
	"BP0021": Refused,        // uart_entry_invalid
	"BP0022": Refused,        // uart_de_pin
	"BP0023": Refused,        // la_no_pull
	"BP0024": Refused,        // la_pull_direction
	"BP0025": Busy,           // lease_busy
	"BP0026": Refused,        // lease_lost
	"BP0027": Refused,        // lease_not_held
	"BP0028": Refused,        // vref_5v_missing
	"BP0029": Refused,        // pull_rail_down
	"BP0030": ClientTooOld,   // client_too_old
	"BP0031": Refused,        // la_pin_in_use
	"BP0032": Refused,        // la_pull_down_engaged
	"BP0033": Refused,        // la_pull_down_conflict
	"BP0034": Refused,        // la_dut_output
	"BP0035": Refused,        // la_driven_pin
	"BP0036": Refused,        // la_not_gpio_output
	"BP0037": Refused,        // la_voltage_pins_in_use
	"BP0038": Refused,        // trigger_timeout
	"BP0039": Refused,        // sensor_trigger_timeout
	"BP0040": Locked,         // lan_locked
	"BP0041": Refused,        // policy_not_from_lan
	"BP0042": Refused,        // setting_cloud_or_usb
	"BP0043": Refused,        // setting_device_only
	"BP0044": Refused,        // policy_loosen_usb_only (retired)
	"BP0045": Forbidden,      // role_required
	"BP0046": Refused,        // no_analog_front_end
	"BP0047": Busy,           // pod_busy
	"BP0048": Busy,           // storage_bus_busy
	"BP0049": Busy,           // uart_session_open
	"BP0050": Busy,           // swd_session_open
	"BP0051": Busy,           // update_in_progress
	"BP0052": Busy,           // cloud_command_busy
	"BP0053": Refused,        // reply_too_large
	"BP0054": Refused,        // cloud_reply_too_large
	"BP0055": Refused,        // sensor_instance_unknown
	"BP0056": Refused,        // no_module_link
	"BP0057": Refused,        // image_switch_sensors_running
	"BP0058": Refused,        // fpga_image_required
	"BP0059": Refused,        // fpga_image_sensors_first
	"BP0060": FirmwareTooOld, // ota_unknown_target
	"BP0061": Refused,        // i2c_needs_pullups
	"BP0062": Refused,        // i2c_session_not_open
	"BP0063": Refused,        // i2c_engine_reset
	"BP0064": Refused,        // loop_sweep_with_map
	"BP0065": Refused,        // output_used_by_sensor
	"BP0066": Busy,           // step_busy
	"BP0067": Refused,        // samples_out_of_range
	"BP0068": Refused,        // la_rate_above_ceiling
	"BP0069": Refused,        // la_rate_too_deep
	"BP0070": Refused,        // capture_dual_too_deep
	"BP0071": Refused,        // capture_overflowed
	"BP0072": Refused,        // psram_too_small
	"BP0073": Refused,        // psram_bandwidth
	"BP0074": Refused,        // signatures_always_required
	"BP0075": Refused,        // lan_plaintext_off
	"BP0076": Refused,        // usb_host_over_usb
	"BP0077": Refused,        // usb_host_port_unavailable
	"BP0078": Refused,        // usb_host_no_crystal
	"BP0079": Refused,        // usb_host_other_port
	"BP0080": Refused,        // dac_pinout_mismatch
	"BP0081": Locked,         // lan_not_paired
	"BP0082": Refused,        // lan_clients_full
	"BP0083": Refused,        // xip_unavailable
	"BP0084": Refused,        // usb_flash_refused
	"BP0085": Refused,        // usb_serial_no_port
	"BP0086": Busy,           // usb_serial_busy
	"BP0087": Refused,        // usb_check_refused
	"BP0088": Refused,        // usb_check_no_efuse_power
	"BP0089": Refused,        // wiring_bad_supply
	"BP0090": Refused,        // power_no_meter
	"BP0091": Refused,        // power_supply_and_efuse
	"BP0092": Refused,        // power_supply_not_on_pod
}

// CodeKind is the kind of a refusal code; ok is false for a code the table does not have.
func CodeKind(code string) (kind RefusalKind, ok bool) {
	kind, ok = codeKinds[strings.ToUpper(strings.TrimSpace(code))]
	return kind, ok
}

// Kind classifies the refusal by its code; a refusal without one (or with a code this table
// does not know) is Refused. The text is never read.
func (e *PodError) Kind() RefusalKind {
	k, _ := CodeKind(e.Code)
	return k
}

// HasCode says err carries a refusal with this code.
func HasCode(err error, code string) bool {
	pe, ok := AsPodError(err)
	return ok && strings.EqualFold(strings.TrimSpace(pe.Code), code)
}

// AsPodError returns the refusal in err's chain, if any.
func AsPodError(err error) (*PodError, bool) {
	var pe *PodError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// ErrUnreachable matches (errors.Is) a failure to reach the pod at all: nothing answered on
// its address within the connect budget, or (serialconsole) no pod on USB.
var ErrUnreachable = errors.New("pod unreachable")

// dialError is a failed connect. Its text is the one the CLI always printed; it also matches
// ErrUnreachable and unwraps to the underlying dial error.
type dialError struct {
	addr string
	err  error
}

func (e *dialError) Error() string { return "connect to pod at " + e.addr + ": " + e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }
func (e *dialError) Is(target error) bool {
	// An interrupted connect (Ctrl+C) is not an unreachable pod.
	return target == ErrUnreachable && !errors.Is(e.err, context.Canceled)
}

func firmwareMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "pod returned an error"
	}
	return msg
}

// codedReason is the text of a coded refusal: the catalog's message filled with args. The pod
// sends no message with a code (firmware 4.0); over the cloud channel the message is the code
// itself. A code this CLI's catalog does not know shows as the code.
func codedReason(code string, args map[string]any) string {
	if e, ok := errcatalog.Lookup(code); ok {
		return errcatalog.Fill(e.Message, args)
	}
	return strings.ToUpper(strings.TrimSpace(code))
}

// replyError is an error reply from the JSON API with its code and args.
func replyError(r *reply) error { return RefusalError(r.Message, r.Code, r.Args) }

// replyErrorAt is replyError for a reply from the pod at addr.
func replyErrorAt(r *reply, addr string) error {
	err := replyError(r)
	if pe, ok := err.(*PodError); ok {
		pe.Addr = strings.TrimSpace(addr)
	}
	return err
}

// RefusalError is a refusal with its text, code and args, from any JSON transport (the LAN, the
// USB network, embeddedci.com's command channel): a *PodError, or for BP0030 (the pod takes no
// client this old) an *UpdateRequiredError. A coded refusal's text comes from the catalog, never
// from the pod (only BP0030 still carries a message, for clients older than the codes).
func RefusalError(msg, code string, args map[string]any) error {
	code = strings.TrimSpace(code)
	pe := &PodError{Reason: firmwareMessage(msg), Code: code, Args: args}
	if code != "" {
		pe.Reason = codedReason(code, args)
	}
	if strings.EqualFold(code, CodeClientTooOld) {
		return updateRequiredFromRefusal(pe)
	}
	return pe
}
