package tcpclient

import (
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

// A coded refusal is classified by its code, whatever its text.
func TestPodErrorKindByCode(t *testing.T) {
	for _, c := range []struct {
		code, reason string
		want         RefusalKind
	}{
		{"BP0001", "la voltage not set", Refused},
		{"BP0002", "unknown cmd", Refused},
		{"BP0003", "", Busy},
		{"BP0004", "", Busy},
		{"BP0005", "swd or spi busy: ...", Busy},
		{"BP0016", "pod returned an error", Busy},
		{"BP0025", "busy: a cloud job holds this pod (ci, 3 s left)", Busy},
		{"BP0025", "pod returned an error", Busy}, // a build without texts
		{"BP0026", "lease lost", Refused},
		{"BP0027", "", Refused},
		{"BP0017", "busy", Refused}, // the code wins over the text
		{"bp0003", "", Busy},
	} {
		if got := (&PodError{Code: c.code, Reason: c.reason}).Kind(); got != c.want {
			t.Errorf("%s %q: kind %v, want %v", c.code, c.reason, got, c.want)
		}
	}
	// Without a code, or with one this table does not know (newer firmware), the text decides.
	for _, c := range []struct {
		code, reason string
		want         RefusalKind
	}{
		{"", "locked: x needs the cloud or USB", Locked},
		{"", "forbidden: x needs an organization owner or admin", Forbidden},
		{"", "uart busy", Busy},
		{"", "unknown cmd", Refused},
		{"BP9999", "busy", Busy},
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
		{&PodError{Code: "BP0002", Reason: "pod returned an error"}, true},
		{&PodError{Reason: "unknown cmd"}, true},
		{&PodError{Reason: "unknown command 'x'"}, true},
		{&PodError{Code: "BP0006", Reason: "missing la"}, false},
		{&PodError{Reason: "uart busy"}, false},
	} {
		if got := IsUnknownCommand(c.pe); got != c.want {
			t.Errorf("%+v: got %v", c.pe, got)
		}
	}
}
