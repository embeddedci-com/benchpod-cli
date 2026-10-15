package main

import (
	"encoding/json"
	"testing"
)

// generate --amplitude-v/--amplitude-ma: the calibrated 16-bit form (firmware 3.9.1+). The CLI
// asks the pod for its caps first, because older firmware would ignore the volts and play its
// default full-scale 8-bit waveform.
func TestGenerateCalibrated(t *testing.T) {
	t.Setenv("BENCHPOD_CONNECTION", "")
	t.Setenv("BENCHPOD_TIMEOUT", "")
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"caps":["dac","generate16"],"bits":16}}`)
	sent := func() map[string]any {
		t.Helper()
		var req map[string]any
		if err := json.Unmarshal([]byte(<-got), &req); err != nil {
			t.Fatal(err)
		}
		return req
	}

	if code := run([]string{"generate", "--waveform", "sine", "--freq", "1kHz", "--path", "3v3",
		"--amplitude-v", "1.48", "--offset-v", "1.65", "--connection", addr}); code != exitOK {
		t.Fatalf("volts: exit %d", code)
	}
	if st := sent(); st["cmd"] != "status" {
		t.Fatalf("first request = %v, want status", st)
	}
	req := sent()
	if req["path"] != "3v3" || req["amplitude_v"] != 1.48 || req["offset_v"] != 1.65 {
		t.Fatalf("volts request = %v", req)
	}
	if _, ok := req["amplitude"]; ok {
		t.Fatalf("8-bit amplitude sent with volts: %v", req)
	}

	if code := run([]string{"generate", "--waveform", "square", "--amplitude-ma", "4", "--offset-ma", "12",
		"--connection", addr}); code != exitOK {
		t.Fatalf("mA: exit %d", code)
	}
	sent()
	if req := sent(); req["amplitude_ua"] != 4000.0 || req["offset_ua"] != 12000.0 || req["path"] != nil {
		t.Fatalf("mA request = %v", req)
	}
}

func TestGenerateCalibratedNeedsFirmware391(t *testing.T) {
	t.Setenv("BENCHPOD_CONNECTION", "")
	t.Setenv("BENCHPOD_TIMEOUT", "")
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"caps":["dac"]}}`)
	if code := run([]string{"generate", "--waveform", "sine", "--path", "5v", "--amplitude-v", "0.1",
		"--offset-v", "2.5", "--connection", addr}); code == exitOK {
		t.Fatal("generate with volts accepted on a pod without generate16")
	}
	if st := <-got; st != `{"cmd":"status"}` {
		t.Fatalf("request = %s", st)
	}
	select {
	case req := <-got:
		t.Fatalf("sent %s to a pod without generate16", req)
	default:
	}
}

func TestGenerateCalibratedFlagErrors(t *testing.T) {
	t.Setenv("BENCHPOD_CONNECTION", "")
	for _, args := range [][]string{
		{"--path", "3v3", "--amplitude-v", "1"},                                       // no offset
		{"--path", "ext", "--amplitude-v", "1", "--offset-v", "1"},                    // bad path
		{"--amplitude-v", "1", "--offset-v", "1"},                                     // no path
		{"--path", "3v3", "--amplitude-v", "1", "--offset-v", "1", "--amplitude", "5"}, // mixed with levels
		{"--path", "3v3", "--amplitude-v", "1", "--offset-v", "1", "--amplitude-ma", "4", "--offset-ma", "12"},
		{"--amplitude-ma", "0", "--offset-ma", "12"},
	} {
		if code := run(append([]string{"generate", "--waveform", "sine", "--connection", "127.0.0.1:1"}, args...)); code == exitOK {
			t.Errorf("%v accepted", args)
		}
	}
}
