package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/fwsign"
	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

// sigVectors loads the shared vectors (internal/fwsign/testdata) and makes their signing key
// this host's developer key, so the good cases check "ok" under the name "dev".
func sigVectors(t *testing.T) (images map[string][]byte, sigs map[string][]byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "fwsign", "testdata", "fwsign_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Images map[string]string `json:"images"`
		Cases  []struct{ Name, Sig string }
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	images, sigs = map[string][]byte{}, map[string][]byte{}
	for k, h := range v.Images {
		images[k], _ = hex.DecodeString(h)
	}
	for _, c := range v.Cases {
		sigs[c.Name], _ = hex.DecodeString(c.Sig)
	}
	dir := t.TempDir()
	orig := fwsign.DevKeyPath
	t.Cleanup(func() { fwsign.DevKeyPath = orig })
	fwsign.DevKeyPath = func() string { return filepath.Join(dir, "fw-signing-dev.key") }
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	if err := os.WriteFile(fwsign.DevKeyPath(), []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		t.Fatal(err)
	}
	return images, sigs
}

// captureStderr runs fn and returns what it wrote to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	return <-done
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFirmwareSignatureLocal(t *testing.T) {
	images, sigs := sigVectors(t)
	dir := t.TempDir()
	fw := filepath.Join(dir, "bench_pod_stm32.bin")
	writeFile(t, fw, images["firmware"])
	ctx := context.Background()

	out := captureStderr(t, func() { checkFirmwareSignature(ctx, fw, fw) })
	if want := "flash-self: signature: none (no bench_pod_stm32.bin.sig next to it)\n"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}

	writeFile(t, fw+".sig", sigs["good firmware"])
	var r fwsign.Report
	out = captureStderr(t, func() { r = checkFirmwareSignature(ctx, fw, fw) })
	if r.Result != "ok" || !strings.HasPrefix(out, "flash-self: signature: ok (dev key ed4242ead4ac6948, release 3.6.0)") {
		t.Fatalf("%+v %q", r, out)
	}

	writeFile(t, fw, images["firmware_changed"])
	out = captureStderr(t, func() { r = checkFirmwareSignature(ctx, fw, fw) })
	if r.Result != "image" || !strings.Contains(out, "warning: signature check failed (image, dev key ed4242ead4ac6948, release 3.6.0); flashing anyway") {
		t.Fatalf("%+v %q", r, out)
	}
}

// A release download looks for <url>.sig; a 404 means the release is not signed.
func TestFirmwareSignatureRelease(t *testing.T) {
	images, sigs := sigVectors(t)
	serveSig := []byte(nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download/bench_pod_stm32.bin.sig" && serveSig != nil {
			_, _ = w.Write(serveSig)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	orig := flashSelfHTTPClient
	flashSelfHTTPClient = ts.Client()
	t.Cleanup(func() { flashSelfHTTPClient = orig })

	file := filepath.Join(t.TempDir(), "downloaded.bin")
	writeFile(t, file, images["firmware"])
	url := ts.URL + "/download/bench_pod_stm32.bin"
	ctx := context.Background()

	out := captureStderr(t, func() { checkFirmwareSignature(ctx, url, file) })
	if want := "flash-self: signature: none (this release is not signed)\n"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	serveSig = sigs["signature byte flipped"]
	out = captureStderr(t, func() { checkFirmwareSignature(ctx, url, file) })
	if !strings.Contains(out, "flash-self: warning: signature check failed (signature, key ed4242ead4ac6948, release 3.6.0); flashing anyway") {
		t.Fatalf("%q", out)
	}
	serveSig = sigs["good firmware"]
	out = captureStderr(t, func() { checkFirmwareSignature(ctx, url, file) })
	if !strings.Contains(out, "flash-self: signature: ok (dev key") {
		t.Fatalf("%q", out)
	}
}

// A blob's manifest goes to the pod when it checks out here (or only its key is unknown), not
// when this host already found it wrong.
func TestBlobSignature(t *testing.T) {
	images, sigs := sigVectors(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "blob-gw1.bin"), images["blob"])
	src := blobSource{base: dir}
	e := blobEntry{Name: "gw1", File: "blob-gw1.bin", Size: len(images["blob"])}
	ctx := context.Background()

	var m []byte
	out := captureStderr(t, func() { m = checkBlobSignature(ctx, src, e, images["blob"]) })
	if m != nil || out != "blobs: gw1 signature: none (no blob-gw1.bin.sig next to it)\n" {
		t.Fatalf("%x %q", m, out)
	}

	writeFile(t, filepath.Join(dir, "blob-gw1.bin.sig"), sigs["good gateware blob"])
	out = captureStderr(t, func() { m = checkBlobSignature(ctx, src, e, images["blob"]) })
	if !bytes.Equal(m, sigs["good gateware blob"]) || !strings.HasPrefix(out, "blobs: gw1 signature: ok (dev key") {
		t.Fatalf("%x %q", m, out)
	}

	// The same manifest on the gw0 slot is for the wrong target.
	e0 := blobEntry{Name: "gw0", File: "blob-gw1.bin"}
	out = captureStderr(t, func() { m = checkBlobSignature(ctx, src, e0, images["blob"]) })
	if m != nil || !strings.HasPrefix(out, "blobs: warning: gw0 signature check failed (target, dev key") ||
		!strings.Contains(out, "installing anyway") {
		t.Fatalf("%x %q", m, out)
	}

	writeFile(t, filepath.Join(dir, "blob-gw1.bin.sig"), sigs["other key"])
	out = captureStderr(t, func() {
		m = checkBlobSignature(ctx, src, blobEntry{Name: "firmware", File: "blob-gw1.bin"}, images["blob"])
	})
	if m == nil || !strings.Contains(out, "unknown-key") {
		t.Fatalf("an unknown-key manifest should still go to the pod: %x %q", m, out)
	}
}

func TestPodSigLine(t *testing.T) {
	if s := podSigLine(serialconsole.SigReport{}); s != "" {
		t.Fatalf("%q", s)
	}
	if s := podSigLine(serialconsole.SigReport{Supported: true, Sent: true, Result: "ok", KeyID: "ed4242ead4ac6948"}); s != "pod signature check: ok (key ed4242ead4ac6948)" {
		t.Fatalf("%q", s)
	}
	if s := podSigLine(serialconsole.SigReport{Supported: true, Result: "none"}); s != "pod signature check: none" {
		t.Fatalf("%q", s)
	}
}
