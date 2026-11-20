package tcpclient

import (
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/errcatalog"
)

// Every code in the catalog has a kind in codeKinds, and the table has no code the catalog lacks.
func TestCodeKindsCoverTheCatalog(t *testing.T) {
	for _, code := range errcatalog.Codes() {
		if _, ok := CodeKind(code); !ok {
			t.Errorf("%s is in the catalog but not in codeKinds", code)
		}
	}
	for code := range codeKinds {
		if _, ok := errcatalog.Lookup(code); !ok {
			t.Errorf("%s is in codeKinds but not in the catalog", code)
		}
	}
}

// A refusal is classified by its code only; its text is never read.
func TestPodErrorKindByCode(t *testing.T) {
	for _, c := range []struct {
		code, reason string
		want         RefusalKind
	}{
		{"BP0001", "", Refused},
		{"BP0002", "", FirmwareTooOld},
		{"BP0003", "", Busy},
		{"BP0005", "", Busy},
		{"BP0016", "", Busy},
		{"BP0017", "busy", FirmwareTooOld}, // the code wins over the text
		{"BP0025", "", Busy},
		{"BP0026", "", Refused},
		{"BP0040", "", Locked},
		{"BP0045", "", Forbidden},
		{"BP0047", "", Busy},
		{"BP0074", "", Refused},
		{"bp0003", "", Busy},
		// Without a code, or with one this table does not know (newer firmware): Refused.
		{"", "locked: x needs the cloud or USB", Refused},
		{"", "uart busy", Refused},
		{"BP9999", "busy", Refused},
	} {
		if got := (&PodError{Code: c.code, Reason: c.reason}).Kind(); got != c.want {
			t.Errorf("%s %q: kind %v, want %v", c.code, c.reason, got, c.want)
		}
	}
}

func TestIsUnknownCommand(t *testing.T) {
	for _, c := range []struct {
		pe   *PodError
		want bool
	}{
		{&PodError{Code: "BP0002"}, true},
		{&PodError{Reason: "unknown cmd"}, false}, // a text is never matched
		{&PodError{Code: "BP0006", Reason: "missing la"}, false},
	} {
		if got := IsUnknownCommand(c.pe); got != c.want {
			t.Errorf("%+v: got %v", c.pe, got)
		}
	}
}

// A coded reply without a message is shown from the catalog, its args filled in.
func TestCodedReplyRendersFromTheCatalog(t *testing.T) {
	err := RefusalError("", "BP0031", map[string]any{"la": 4.0, "owner": "gpio"})
	pe, ok := AsPodError(err)
	if !ok || pe.Reason != "LA4 is in use by gpio" || pe.Code != "BP0031" {
		t.Fatalf("pe = %+v", pe)
	}
	// Over the cloud channel the message is the code itself.
	pe, _ = AsPodError(RefusalError("BP0047", "BP0047", nil))
	if !strings.HasPrefix(pe.Reason, "The pod is running another long command") {
		t.Fatalf("cloud: %q", pe.Reason)
	}
	// A code this catalog does not know shows as the code.
	pe, _ = AsPodError(RefusalError("", "BP9999", nil))
	if pe.Reason != "BP9999" {
		t.Fatalf("unknown: %q", pe.Reason)
	}
	// A refusal without a code keeps the pod's text.
	pe, _ = AsPodError(RefusalError("unknown waveform", "", nil))
	if pe.Reason != "unknown waveform" {
		t.Fatalf("uncoded: %q", pe.Reason)
	}
}
