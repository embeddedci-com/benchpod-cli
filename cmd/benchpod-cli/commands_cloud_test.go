package main

import "testing"

// TestDefaultDeviceNameFromAddr verifies the default device name is always URL-safe
// ([A-Za-z0-9._-], no colons) so the server's name validation accepts it.
func TestDefaultDeviceNameFromAddr(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"192.168.1.214:8080", "192.168.1.214"}, // the reported case: port stripped
		{"192.168.1.214:9000", "192.168.1.214"},
		{"192.168.1.214", "192.168.1.214"}, // no port
		{"benchpod.local:8080", "benchpod.local"},
		{"[fe80::1]:8080", "fe80--1"}, // IPv6: colons sanitized to dashes
		{":8080", "benchpod"},         // no host -> fallback
		{"", "benchpod"},
	}
	for _, c := range cases {
		got := defaultDeviceNameFromAddr(c.addr)
		if got != c.want {
			t.Errorf("defaultDeviceNameFromAddr(%q) = %q, want %q", c.addr, got, c.want)
		}
		// Defense: the result must never contain a colon or space.
		for _, r := range got {
			if r == ':' || r == ' ' {
				t.Errorf("defaultDeviceNameFromAddr(%q) = %q contains an invalid char", c.addr, got)
			}
		}
	}
}
