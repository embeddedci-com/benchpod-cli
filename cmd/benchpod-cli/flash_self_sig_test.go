package main

import (
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/fwsign"
)

// A download must be signed (unless --allow-unsigned); a local file is only reported on.
func TestRequireSignature(t *testing.T) {
	url := "https://github.com/x/releases/download/v1/bench_pod_stm32.bin"
	f := &flashSelfFlags{}
	if err := f.requireSignature(url, fwsign.Report{Result: fwsign.ResultOK}); err != nil {
		t.Errorf("signed download: %v", err)
	}
	if err := f.requireSignature(url, fwsign.Report{Result: fwsign.ResultNone}); err == nil {
		t.Error("unsigned download: want an error")
	}
	if err := f.requireSignature("/tmp/fw.bin", fwsign.Report{Result: fwsign.ResultNone}); err != nil {
		t.Errorf("local file: %v", err)
	}
	f.allowUnsigned = true
	if err := f.requireSignature(url, fwsign.Report{Result: fwsign.ResultNone}); err != nil {
		t.Errorf("--allow-unsigned: %v", err)
	}
	for u, want := range map[string]bool{"http://localhost:8000/fw.bin": true, "http://127.0.0.1/fw.bin": true, "http://[::1]:9/x": true, "http://example.com/fw.bin": false, "http://localhost.evil.com/x": false} {
		if got := loopbackURL(u); got != want {
			t.Errorf("loopbackURL(%q) = %v, want %v", u, got, want)
		}
	}
}
