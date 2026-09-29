package tcpclient

import (
	"testing"
)

func TestDAPStartOK(t *testing.T) {
	addr, gotReq := mockServer(t, []string{`{"status":"ok","data":"dap ready"}`})
	c := &Client{Addr: addr}

	conn, err := c.DAPStart(testContext(t), 2, 3)
	if err != nil {
		t.Fatalf("DAPStart: %v", err)
	}
	defer conn.Close()

	req := <-gotReq
	if req["cmd"] != "dap_start" {
		t.Errorf("cmd = %v, want dap_start", req["cmd"])
	}
	// JSON numbers decode as float64.
	if req["swclk"] != float64(2) || req["swdio"] != float64(3) {
		t.Errorf("pins = swclk:%v swdio:%v, want 2/3", req["swclk"], req["swdio"])
	}
}

// nRESET is the pod's own pin since rev3, so dap_start must never carry an
// nreset field — a pod that still honoured one would take an LA channel out of
// service behind the user's back.
func TestDAPStartOmitsNreset(t *testing.T) {
	addr, gotReq := mockServer(t, []string{`{"status":"ok","data":"dap ready"}`})
	c := &Client{Addr: addr}

	conn, err := c.DAPStart(testContext(t), 2, 3)
	if err != nil {
		t.Fatalf("DAPStart: %v", err)
	}
	defer conn.Close()

	req := <-gotReq
	if _, ok := req["nreset"]; ok {
		t.Errorf("nreset present in request, want omitted: %v", req)
	}
}

func TestDAPStartError(t *testing.T) {
	addr, _ := mockServer(t, []string{`{"status":"error","message":"swd busy"}`})
	c := &Client{Addr: addr}

	conn, err := c.DAPStart(testContext(t), 2, 3)
	if err == nil {
		conn.Close()
		t.Fatal("DAPStart: expected error, got nil")
	}
	if err.Error() != "swd busy" {
		t.Errorf("error = %q, want %q", err.Error(), "swd busy")
	}
}

func TestDAPStartWithSendsOptions(t *testing.T) {
	addr, gotReq := mockServer(t, []string{`{"status":"ok","data":"dap ready"}`})
	c := &Client{Addr: addr}

	conn, err := c.DAPStartWith(testContext(t), 2, 3, DAPOptions{PacketSize: 1024, PacketCount: 4, WaitMS: 5000})
	if err != nil {
		t.Fatalf("DAPStartWith: %v", err)
	}
	defer conn.Close()

	req := <-gotReq
	if req["packet_size"] != float64(1024) || req["packet_count"] != float64(4) || req["wait_ms"] != float64(5000) {
		t.Errorf("options = %v", req)
	}
}

// DAPStart without options must not send them: the pod then keeps its 256 x 1 default.
func TestDAPStartSendsNoOptions(t *testing.T) {
	addr, gotReq := mockServer(t, []string{`{"status":"ok","data":"dap ready"}`})
	c := &Client{Addr: addr}

	conn, err := c.DAPStart(testContext(t), 2, 3)
	if err != nil {
		t.Fatalf("DAPStart: %v", err)
	}
	defer conn.Close()

	req := <-gotReq
	for _, k := range []string{"packet_size", "packet_count", "wait_ms"} {
		if _, ok := req[k]; ok {
			t.Errorf("%s present in request, want omitted: %v", k, req)
		}
	}
}
