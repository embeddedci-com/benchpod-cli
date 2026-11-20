package spiflash

import (
	"errors"
	"fmt"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// isSPIBusy decides whether a failed spi_start was another session's: by its code only.
func TestIsSPIBusyByCode(t *testing.T) {
	for code, want := range map[string]bool{
		"BP0004": true,  // spi_session_open
		"BP0005": true,  // bus_engine_busy
		"BP0050": false, // swd_session_open
		"BP0047": false, // pod_busy
		"BP0025": false, // lease_busy
		"":       false,
	} {
		err := fmt.Errorf("spi_start: %w", tcpclient.RefusalError("spi busy", code, nil))
		if got := isSPIBusy(err); got != want {
			t.Errorf("isSPIBusy(%q) = %v, want %v", code, got, want)
		}
	}
	if isSPIBusy(errors.New("spi busy: send spi_stop first")) {
		t.Error("a text without a code taken for busy")
	}
}
