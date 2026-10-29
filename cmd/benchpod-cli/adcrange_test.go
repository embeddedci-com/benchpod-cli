package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestADCRangeShow(t *testing.T) {
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"range":"normal","supported":true,"fullscale_mv":32856,`+
		`"normal_fullscale_mv":32856,"low_fullscale_mv":5462,"adc_cal_a_uv":65708158,"adc_cal_b_nv":-1002644}}`)
	var out bytes.Buffer
	if err := runADCRange(&globalFlags{connection: addr}, "show", &out); err != nil {
		t.Fatal(err)
	}
	if req := <-got; req != `{"cmd":"adc_range"}` {
		t.Fatalf("request %s", req)
	}
	if s := out.String(); !strings.Contains(s, "normal, ±32.86 V") || !strings.Contains(s, "low ±5.46 V") {
		t.Fatalf("output %q", s)
	}
}

func TestADCRangeSwitch(t *testing.T) {
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"range":"low","supported":true,"fullscale_mv":5462,`+
		`"normal_fullscale_mv":32856,"low_fullscale_mv":5462}}`)
	var out bytes.Buffer
	if err := runADCRange(&globalFlags{connection: addr}, "low", &out); err != nil {
		t.Fatal(err)
	}
	if req := <-got; !strings.Contains(req, `"range":"low"`) || !strings.Contains(req, `"adc_range"`) {
		t.Fatalf("request %s", req)
	}
	if !strings.Contains(out.String(), "low, ±5.46 V") {
		t.Fatalf("output %q", out.String())
	}
}

func TestADCRangeNoLowRange(t *testing.T) {
	addr, _ := fakeLANPod(t, `{"status":"ok","data":{"range":"normal","supported":false,"fullscale_mv":32856,"normal_fullscale_mv":32856}}`)
	var out bytes.Buffer
	if err := runADCRange(&globalFlags{connection: addr}, "show", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "v3r4") {
		t.Fatalf("output %q", out.String())
	}
	addr, _ = fakeLANPod(t, `{"status":"error","message":"range: the low range needs analog module v3r4 or later"}`)
	err := runADCRange(&globalFlags{connection: addr}, "low", &out)
	if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "v3r4") {
		t.Fatalf("err = %v", err)
	}
}

func TestADCRangeSelfTest(t *testing.T) {
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"points":[{"set_mv":1000,"normal_mv":1001,"low_mv":999,"ok":true},`+
		`{"set_mv":3500,"normal_mv":3502,"low_mv":582,"ok":false}],"pass":false,"range":"normal"}}`)
	var out bytes.Buffer
	err := runADCRange(&globalFlags{connection: addr}, "test", &out)
	if req := <-got; !strings.Contains(req, `"test":true`) {
		t.Fatalf("request %s", req)
	}
	if err == nil || !strings.Contains(err.Error(), "disagree") || !strings.Contains(out.String(), "FAIL") {
		t.Fatalf("err = %v, output %q", err, out.String())
	}
}

func TestADCRangeOnOlderFirmware(t *testing.T) {
	addr, _ := fakeLANPod(t, `{"status":"error","message":"unknown cmd"}`)
	err := runADCRange(&globalFlags{connection: addr}, "show", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "3.9.5 or later") {
		t.Fatalf("err = %v", err)
	}
}
