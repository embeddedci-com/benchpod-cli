package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

// withFWReports points the reporter at a fake server and a temporary queue.
func withFWReports(t *testing.T, post func(fwEvent) error) (sent *[]fwEvent) {
	t.Helper()
	oldPost, oldPath := fwReportPost, fwReportQueuePath
	t.Cleanup(func() { fwReportPost, fwReportQueuePath = oldPost, oldPath })
	dir := t.TempDir()
	fwReportQueuePath = func() string { return filepath.Join(dir, "firmware-reports.jsonl") }
	var got []fwEvent
	fwReportPost = func(_ context.Context, ev fwEvent) error {
		got = append(got, ev)
		return post(ev)
	}
	t.Setenv("BENCHPOD_NO_FIRMWARE_REPORT", "")
	return &got
}

func TestFirmwareReportSendsWhenSignedIn(t *testing.T) {
	sent := withFWReports(t, func(fwEvent) error { return nil })
	(&globalFlags{}).reportFirmware(fwEvent{PublicKey: "pk", Kind: "firmware", Method: "dfu", OldVersion: "3.8.1", NewVersion: "3.9.0"})
	if len(*sent) != 1 || (*sent)[0].CLIVersion != version || (*sent)[0].At == "" || len(readFWQueue()) != 0 {
		t.Fatalf("sent %+v, queue %d", *sent, len(readFWQueue()))
	}
}

func TestFirmwareReportOptOut(t *testing.T) {
	sent := withFWReports(t, func(fwEvent) error { return nil })
	(&globalFlags{noFirmwareReport: true}).reportFirmware(fwEvent{PublicKey: "pk", Kind: "firmware"})
	t.Setenv("BENCHPOD_NO_FIRMWARE_REPORT", "1")
	(&globalFlags{}).reportFirmware(fwEvent{PublicKey: "pk", Kind: "firmware"})
	if len(*sent) != 0 {
		t.Fatalf("sent despite the opt-out: %+v", *sent)
	}
}

func TestFirmwareReportSilentWhenNotSignedIn(t *testing.T) {
	sent := withFWReports(t, func(fwEvent) error { return errNoCloudCredential })
	(&globalFlags{}).reportFirmware(fwEvent{PublicKey: "pk", Kind: "firmware"})
	if len(*sent) != 1 || len(readFWQueue()) != 0 {
		t.Fatalf("not signed in: queued %d", len(readFWQueue()))
	}
}

// Offline: the report waits for one more try on a later run, then is dropped.
func TestFirmwareReportRetriesOnceLater(t *testing.T) {
	fail := true
	sent := withFWReports(t, func(fwEvent) error {
		if fail {
			return errors.New("offline")
		}
		return nil
	})
	g := &globalFlags{}
	g.reportFirmware(fwEvent{PublicKey: "a", Kind: "firmware"})
	if q := readFWQueue(); len(q) != 1 || q[0].Tries != 1 {
		t.Fatalf("queue after a failed send: %+v", q)
	}
	g.reportFirmware(fwEvent{PublicKey: "b", Kind: "firmware"}) // still offline: a is dropped, b kept
	if q := readFWQueue(); len(q) != 1 || q[0].Event.PublicKey != "b" {
		t.Fatalf("queue after the second failure: %+v", q)
	}
	fail = false
	*sent = nil
	g.reportFirmware(fwEvent{PublicKey: "c", Kind: "blobs"})
	if len(*sent) != 2 || (*sent)[0].PublicKey != "b" || (*sent)[1].PublicKey != "c" || len(readFWQueue()) != 0 {
		t.Fatalf("online again: sent %+v, queue %d", *sent, len(readFWQueue()))
	}
}

func TestFirmwareEventFromBeforeAndAfter(t *testing.T) {
	before := podVersions{PublicKey: "pk", ShortID: "baea06", Version: "3.8.1", Gateware: 48}
	ev := firmwareEvent(before, podVersions{}, "v3.9.0", []blobEntry{{Name: "gw1", Version: 49}})
	if ev.PublicKey != "pk" || ev.Name != "benchpod-baea06" || ev.OldVersion != "3.8.1" || ev.NewVersion != "3.9.0" ||
		ev.Method != "dfu" || ev.Kind != "firmware" || len(ev.Blobs) != 1 {
		t.Fatalf("%+v", ev)
	}
	ev = firmwareEvent(podVersions{}, podVersions{PublicKey: "pk2", ShortID: "aaaaaa", Version: "3.9.0", Gateware: 49}, "", nil)
	if ev.PublicKey != "pk2" || ev.NewVersion != "3.9.0" || ev.NewGateware != 49 || ev.OldVersion != "" {
		t.Fatalf("after only: %+v", ev)
	}
	if gatewareOf([]serialconsole.BlobSlot{{Name: "gw0", Version: 47}, {Name: "gw1", Version: 48}, {Name: "esp", Version: 3}}) != 48 {
		t.Fatal("gatewareOf")
	}
}

// A report the server refuses (4xx) is dropped, not kept for a retry.
func TestFirmwareReportRejectedIsDropped(t *testing.T) {
	withFWReports(t, func(fwEvent) error { return errFWReportRejected })
	(&globalFlags{}).reportFirmware(fwEvent{PublicKey: "pk", Kind: "firmware"})
	if q := readFWQueue(); len(q) != 0 {
		t.Fatalf("queued a rejected report: %+v", q)
	}
}
