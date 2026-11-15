package tcpclient

import (
	"strings"
	"testing"
)

func TestStagePSRAMSkipsAcks(t *testing.T) {
	addr, got := mockServer(t, []string{
		`{"status":"ok","data":"ready"}`,
		`{"ack":8192}`,
		`{"status":"ok","data":{"total":12000}}`,
	})
	c := &Client{Addr: addr}
	n, err := c.StagePSRAM(testContext(t), make([]byte, 12000))
	if err != nil {
		t.Fatalf("StagePSRAM: %v", err)
	}
	if n != 12000 {
		t.Fatalf("stored %d, want 12000", n)
	}
	req := <-got
	if req["cmd"] != "load_bin" || req["psram"] != true || req["total"] != float64(12000) {
		t.Fatalf("request %v", req)
	}
}

func TestStagePSRAMRefused(t *testing.T) {
	addr, _ := mockServer(t, []string{`{"status":"error","message":"busy"}`})
	c := &Client{Addr: addr}
	if _, err := c.StagePSRAM(testContext(t), []byte{1, 2, 3}); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("want busy, got %v", err)
	}
}

func TestStagePSRAMEmpty(t *testing.T) {
	c := &Client{Addr: "127.0.0.1:1"}
	if _, err := c.StagePSRAM(testContext(t), nil); err == nil {
		t.Fatal("want an error for no data")
	}
}
