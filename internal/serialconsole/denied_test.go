package serialconsole

import (
	"errors"
	"strings"
	"testing"
)

// A console the user may not open (Linux: the dialout group) is named with the fix, not reported
// as "no console found".
func TestDeniedHint(t *testing.T) {
	if !isPermissionDenied(errors.New("open /dev/ttyACM0: permission denied")) || isPermissionDenied(errors.New("busy")) || isPermissionDenied(nil) {
		t.Fatal("isPermissionDenied")
	}
	if deniedHint(nil) != "" {
		t.Fatal("no hint without a refused port")
	}
	h := deniedHint([]string{"/dev/ttyACM0"})
	for _, want := range []string{"/dev/ttyACM0", "permission denied", "dialout", "70-benchpod.rules"} {
		if !strings.Contains(h, want) {
			t.Errorf("hint %q lacks %q", h, want)
		}
	}
}
