package main

import "testing"

func TestParseLAPin(t *testing.T) {
	for in, want := range map[string]int{"1": 1, "la1": 1, " LA12 ": 12, "13": 13, "la14": 14} {
		got, err := parseLAPin(in)
		if err != nil || got != want {
			t.Errorf("parseLAPin(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"0", "15", "la15", "x", ""} {
		if _, err := parseLAPin(in); err == nil {
			t.Errorf("parseLAPin(%q) accepted; want an error", in)
		}
	}
}
