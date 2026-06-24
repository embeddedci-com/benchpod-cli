package main

import (
	"strings"
	"testing"
)

func TestBuildOpenOCDArgs(t *testing.T) {
	const target = "target/stm32f4x.cfg"
	const file = "fw.elf"

	tests := []struct {
		name              string
		target, file      string
		noVerify, noReset bool
		connectUnderReset bool
		clearResetEvents  bool
		extraConfigs      []string
		extraArgs         []string
		want              []string
		wantErr           bool
	}{
		{
			name:   "target+file default (no reset clearing)",
			target: target, file: file,
			want: []string{"-f", target, "-c", "program fw.elf verify reset exit"},
		},
		{
			name:   "clear reset events injected before program",
			target: target, file: file, clearResetEvents: true,
			want: []string{"-f", target, "-c", clearResetEventsTCL, "-c", "program fw.elf verify reset exit"},
		},
		{
			name:   "connect under reset + clear reset events ordering",
			target: target, file: file, connectUnderReset: true, clearResetEvents: true,
			want: []string{"-f", target, "-c", "reset_config srst_only srst_nogate connect_assert_srst", "-c", clearResetEventsTCL, "-c", "program fw.elf verify reset exit"},
		},
		{
			name:   "clear reset events with no file still injects after target",
			target: target, clearResetEvents: true,
			want: []string{"-f", target, "-c", clearResetEventsTCL},
		},
		{
			name:   "no-verify no-reset",
			target: target, file: file, noVerify: true, noReset: true,
			want: []string{"-f", target, "-c", "program fw.elf exit"},
		},
		{
			name:    "nothing to do",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildOpenOCDArgs(tt.target, tt.file, "", tt.noVerify, tt.noReset, tt.connectUnderReset, tt.clearResetEvents, tt.extraConfigs, tt.extraArgs)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("args = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBuildOpenOCDArgsResetOrder pins the ordering contract that
// connect_assert_srst and the reset-event clearing must precede the program
// command, since program triggers OpenOCD's init/reset and both have to be in
// place before then.
func TestBuildOpenOCDArgsResetOrder(t *testing.T) {
	got, err := buildOpenOCDArgs("target/stm32f4x.cfg", "fw.elf", "", false, false, true, true, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	resetIdx, clearIdx, progIdx := -1, -1, -1
	for i, a := range got {
		if strings.Contains(a, "connect_assert_srst") {
			resetIdx = i
		}
		if a == clearResetEventsTCL {
			clearIdx = i
		}
		if strings.HasPrefix(a, "program ") {
			progIdx = i
		}
	}
	if resetIdx < 0 || clearIdx < 0 || progIdx < 0 {
		t.Fatalf("missing reset (%d), clear (%d) or program (%d) in %v", resetIdx, clearIdx, progIdx, got)
	}
	if resetIdx > progIdx || clearIdx > progIdx {
		t.Errorf("reset_config (%d) and clear-events (%d) must come before program (%d): %v", resetIdx, clearIdx, progIdx, got)
	}
}
