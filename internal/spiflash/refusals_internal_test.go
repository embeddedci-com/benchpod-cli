package spiflash

import (
	"errors"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/fwrefusals"
)

// isSPIBusy decides whether a failed spi_start was another session's: it must match the
// firmware's SPI refusals and nothing else.
func TestIsSPIBusyMatchesTheFirmwareTexts(t *testing.T) {
	for id, want := range map[string]bool{
		"spi_busy":             true,
		"swd_or_spi_busy":      true,
		"swd_busy_spi_session": false,
		"swd_busy":             false,
		"heavy_busy":           false,
		"psram_bus_busy":       false,
		"lease_busy":           false,
	} {
		if got := isSPIBusy(errors.New(fwrefusals.Example(id))); got != want {
			t.Errorf("isSPIBusy(%s: %q) = %v, want %v", id, fwrefusals.Example(id), got, want)
		}
	}
}
