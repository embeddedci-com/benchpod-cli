package main

import (
	"strings"
	"testing"
)

func TestDfuUtilDownloadArgs(t *testing.T) {
	tests := []struct {
		name    string
		address string
		leave   bool
		file    string
		want    string
	}{
		{
			name:    "default address with leave",
			address: "0x08000000",
			leave:   true,
			file:    "fw.bin",
			want:    "-a 0 --dfuse-address 0x08000000:leave -D fw.bin",
		},
		{
			name:    "no leave stays in DFU",
			address: "0x08000000",
			leave:   false,
			file:    "build/bench_pod_stm32.bin",
			want:    "-a 0 --dfuse-address 0x08000000 -D build/bench_pod_stm32.bin",
		},
		{
			name:    "custom address",
			address: "0x08020000",
			leave:   true,
			file:    "app.bin",
			want:    "-a 0 --dfuse-address 0x08020000:leave -D app.bin",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(dfuUtilDownloadArgs(tt.address, tt.leave, tt.file), " ")
			if got != tt.want {
				t.Errorf("dfuUtilDownloadArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFirmwareReleaseURL(t *testing.T) {
	base := "https://github.com/embeddedci-com/benchpod-firmware/releases"
	tests := []struct {
		version string
		want    string
	}{
		{"", base + "/latest/download/bench_pod_stm32.bin"},
		{"latest", base + "/latest/download/bench_pod_stm32.bin"},
		{"v1.2.3", base + "/download/v1.2.3/bench_pod_stm32.bin"},
		{"  v0.0.5  ", base + "/download/v0.0.5/bench_pod_stm32.bin"},
	}
	for _, tt := range tests {
		if got := firmwareReleaseURL(tt.version); got != tt.want {
			t.Errorf("firmwareReleaseURL(%q) = %q, want %q", tt.version, got, tt.want)
		}
	}
}

func TestIsHTTPURL(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"https://example.com/fw.bin", true},
		{"http://example.com/fw.bin", true},
		{"fw.bin", false},
		{"./build/bench_pod_stm32.bin", false},
		{"/abs/path/fw.bin", false},
		{"httpsfoo", false},
	}
	for _, tt := range tests {
		if got := isHTTPURL(tt.in); got != tt.want {
			t.Errorf("isHTTPURL(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
