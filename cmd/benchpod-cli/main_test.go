package main

import (
	"bytes"
	"testing"
)

func TestWriteSamplesJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSamples(&buf, []int{1, 2, 3}, "json"); err != nil {
		t.Fatalf("writeSamples: %v", err)
	}
	if got, want := buf.String(), "[1,2,3]\n"; got != want {
		t.Fatalf("json output = %q, want %q", got, want)
	}
}

func TestWriteSamplesCSV(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSamples(&buf, []int{10, 20}, "csv"); err != nil {
		t.Fatalf("writeSamples: %v", err)
	}
	want := "index,value\n0,10\n1,20\n"
	if got := buf.String(); got != want {
		t.Fatalf("csv output = %q, want %q", got, want)
	}
}

func TestWriteSamplesNDJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := writeSamples(&buf, []int{142, 139}, "ndjson"); err != nil {
		t.Fatalf("writeSamples: %v", err)
	}
	want := "{\"index\":0,\"value\":142}\n{\"index\":1,\"value\":139}\n"
	if got := buf.String(); got != want {
		t.Fatalf("ndjson output = %q, want %q", got, want)
	}
}

func TestWriteSamplesEmpty(t *testing.T) {
	for _, format := range []string{"json", "csv", "ndjson"} {
		var buf bytes.Buffer
		if err := writeSamples(&buf, nil, format); err != nil {
			t.Fatalf("writeSamples(%s): %v", format, err)
		}
		switch format {
		case "json":
			if got := buf.String(); got != "null\n" {
				t.Fatalf("json empty = %q, want %q", got, "null\n")
			}
		case "csv":
			if got := buf.String(); got != "index,value\n" {
				t.Fatalf("csv empty = %q, want header only", got)
			}
		case "ndjson":
			if got := buf.String(); got != "" {
				t.Fatalf("ndjson empty = %q, want empty", got)
			}
		}
	}
}

func TestValidOutput(t *testing.T) {
	for _, ok := range []string{"json", "csv", "ndjson"} {
		if !validOutput(ok) {
			t.Errorf("validOutput(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "xml", "yaml", "JSON"} {
		if validOutput(bad) {
			t.Errorf("validOutput(%q) = true, want false", bad)
		}
	}
}
