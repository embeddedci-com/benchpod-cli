package tcpclient

import "testing"

// Messages name a LAN pod by its host until a connection shows TLS 8443 or plaintext 8080.
func TestShownAddr(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.164:8080":  "192.168.1.164",
		"192.168.1.164:8443":  "192.168.1.164:8443",
		"192.168.1.164:9000":  "192.168.1.164:9000",
		"10.237.210.109:8080": "10.237.210.109:8080", // the USB link is plaintext 8080
		"benchpod.local:8080": "benchpod.local",
	} {
		if got := ShownAddr(in); got != want {
			t.Errorf("ShownAddr(%q) = %q, want %q", in, got, want)
		}
	}
	c := &Client{Addr: "192.168.1.164:8080"}
	if got := c.Shown(); got != "192.168.1.164" {
		t.Errorf("before a connection: %q", got)
	}
	c.lastTLS, c.lastAddr = true, "192.168.1.164:8443"
	if got := c.Shown(); got != "192.168.1.164:8443" {
		t.Errorf("after a TLS connection: %q", got)
	}
}
