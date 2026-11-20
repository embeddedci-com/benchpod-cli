package tcpclient

import (
	"context"
	"errors"
	"strings"
)

// PodError is the pod refusing a command, as opposed to a transport failure: an error reply
// from the JSON API ({"status":"error","message":...}) or, over the USB console, a command's
// "<cmd> error <why>" line. It is the one error type for a refusal across the CLI; the USB
// console's errors are the same type.
//
// Cmd is the console command a USB refusal answers ("proxy-set", "lan-policy"); it is "" for a
// JSON reply, whose message stands alone.
type PodError struct {
	Cmd    string
	Reason string
	// Code and Args: the pod's stable error code (BP0001...) and its values, from firmware that
	// sends them (benchpod-firmware errors/catalog.toml); "" / nil otherwise.
	Code string
	Args map[string]any
}

func (e *PodError) Error() string {
	if e.Cmd == "" {
		return e.Reason
	}
	return e.Cmd + ": " + e.Reason
}

// RefusalKind classifies a refusal by the firmware's own prefix.
type RefusalKind int

const (
	// Refused is any other refusal: a bad argument, a missing feature, a failed operation.
	Refused RefusalKind = iota
	// Locked is the LAN policy refusing a command on the LAN ("locked: ...").
	Locked
	// Busy is the pod or one of its engines in use: a cloud lease ("busy: a cloud job holds
	// this pod ..."), the PSRAM bus, or an SWD, SPI or UART session ("swd busy", "uart busy").
	Busy
	// Forbidden is a cloud tunnel without the role a command needs ("forbidden: ...").
	Forbidden
)

// codeKinds is the one table from a refusal's stable code (benchpod-firmware errors/catalog.toml,
// internal/errcatalog) to its kind, for every code in the catalog; the exit code follows the kind,
// and the CLI's hint for a coded refusal is the catalog's fix.cli. A test keeps it in step with
// the catalog. A code the table does not have (newer firmware) falls back to the text.
var codeKinds = map[string]RefusalKind{
	"BP0001": Refused, // la_voltage_not_set
	"BP0002": Refused, // unknown_command: also IsUnknownCommand, for "this firmware lacks it" messages
	"BP0003": Busy,    // i2c_session_open
	"BP0004": Busy,    // spi_session_open
	"BP0005": Busy,    // bus_engine_busy
	"BP0006": Refused, // missing_argument
	"BP0007": Refused, // wiring_profile_not_found
	"BP0008": Refused, // wiring_bad_name
	"BP0009": Refused, // wiring_revision_conflict
	"BP0010": Refused, // wiring_store_full
	"BP0011": Refused, // wiring_profile_too_large
	"BP0012": Refused, // wiring_not_json_object
	"BP0013": Refused, // wiring_delete_default
	"BP0014": Refused, // wiring_upload_out_of_order
	"BP0015": Refused, // wiring_storage_failed
	"BP0016": Busy,    // storage_busy
	"BP0017": Refused, // gateware_too_old
	"BP0018": Refused, // uart_same_pin
	"BP0019": Refused, // uart_half_duplex_two_pins
	"BP0020": Refused, // uart_entry_no_reset_pin
	"BP0021": Refused, // uart_entry_invalid
	"BP0022": Refused, // uart_de_pin
	"BP0023": Refused, // la_no_pull
	"BP0024": Refused, // la_pull_direction
	"BP0025": Busy,    // lease_busy: args holder, purpose, left_s
	"BP0026": Refused, // lease_lost: args by (benchpod ci handles it: the run is invalid)
	"BP0027": Refused, // lease_not_held
	"BP0028": Refused, // vref_5v_missing
	"BP0029": Refused, // pull_rail_down
	"BP0030": Refused, // client_too_old: arrives as an *UpdateRequiredError, not a *PodError (RefusalError)
}

// CodeKind is the kind of a refusal code; ok is false for a code the table does not have.
func CodeKind(code string) (kind RefusalKind, ok bool) {
	kind, ok = codeKinds[strings.ToUpper(strings.TrimSpace(code))]
	return kind, ok
}

// Kind classifies the refusal: by its code, else (a reply without one, from older firmware or a
// refusal with no code yet) by its text.
func (e *PodError) Kind() RefusalKind {
	if k, ok := CodeKind(e.Code); ok {
		return k
	}
	return ClassifyRefusal(e.Reason)
}

// ClassifyRefusal classifies a firmware refusal text by its prefix: "locked:", "forbidden:",
// or a first clause ending in "busy" ("busy", "busy: ...", "swd busy", "swd or spi busy: ...").
// It is the fallback for a refusal without a code.
func ClassifyRefusal(msg string) RefusalKind {
	msg = strings.TrimSpace(msg)
	switch {
	case strings.HasPrefix(msg, "locked:"): // no code yet (the LAN policy gate)
		return Locked
	case strings.HasPrefix(msg, "forbidden:"): // no code yet (the cloud tunnel role gate)
		return Forbidden
	}
	// "busy: a cloud job holds ..." / "busy: <holder> holds ...": BP0025 on firmware after
	// 3.10.13; the engine texts ("swd busy", "uart busy", "busy: ... PSRAM bus"): no code yet
	// (BP0003-BP0005 and BP0016 cover the I2C, SPI, bus engine and storage cases).
	head, _, _ := strings.Cut(msg, ":")
	if head == "busy" || strings.HasSuffix(head, " busy") {
		return Busy
	}
	return Refused
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

// podError is an error reply from the JSON API.
func podError(msg string) error { return &PodError{Reason: firmwareMessage(msg)} }

// replyError is an error reply from the JSON API with its code and args.
func replyError(r *reply) error { return RefusalError(r.Message, r.Code, r.Args) }

// RefusalError is a refusal with its text, code and args, from any JSON transport (the LAN, the
// USB network, embeddedci.com's command channel): a *PodError, or for BP0030 (the pod takes no
// client this old) an *UpdateRequiredError.
func RefusalError(msg, code string, args map[string]any) error {
	pe := &PodError{Reason: firmwareMessage(msg), Code: code, Args: args}
	if strings.EqualFold(strings.TrimSpace(code), CodeClientTooOld) {
		return updateRequiredFromRefusal(pe)
	}
	return pe
}
