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

// Kind classifies the refusal.
func (e *PodError) Kind() RefusalKind { return ClassifyRefusal(e.Reason) }

// ClassifyRefusal classifies a firmware refusal text by its prefix: "locked:", "forbidden:",
// or a first clause ending in "busy" ("busy", "busy: ...", "swd busy", "swd or spi busy: ...").
func ClassifyRefusal(msg string) RefusalKind {
	msg = strings.TrimSpace(msg)
	switch {
	case strings.HasPrefix(msg, "locked:"):
		return Locked
	case strings.HasPrefix(msg, "forbidden:"):
		return Forbidden
	}
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
