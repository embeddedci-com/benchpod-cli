package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/embeddedci-com/benchpod-cli/internal/fwsign"
	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

// Firmware releases publish a signed manifest, `<asset>.sig`, next to bench_pod_stm32.bin and
// each blob-*.bin (benchpod-firmware docs/design/firmware-signing.md). For now the CLI only
// reports on it: it checks the signature, prints the result and hands the manifest to firmware
// that understands it, but it never refuses a flash or an install because of it.

// readSig reads the .sig that goes with name in src. Without one it returns nil and why, for
// the "none" line.
func readSig(ctx context.Context, src blobSource, name string) ([]byte, string) {
	sigName := name + ".sig"
	if isHTTPURL(src.base) {
		body, err := httpGet(ctx, strings.TrimRight(src.base, "/")+"/"+sigName)
		if err != nil {
			if strings.Contains(err.Error(), "HTTP 404") {
				return nil, "this release is not signed"
			}
			return nil, fmt.Sprintf("could not fetch %s: %v", sigName, err)
		}
		defer body.Close()
		raw, err := io.ReadAll(io.LimitReader(body, 4096))
		if err != nil {
			return nil, fmt.Sprintf("could not fetch %s: %v", sigName, err)
		}
		return raw, ""
	}
	raw, err := os.ReadFile(filepath.Join(src.base, sigName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "no " + sigName + " next to it"
		}
		return nil, fmt.Sprintf("could not read %s: %v", sigName, err)
	}
	return raw, ""
}

// signatureLine is the one line that reports a check: "signature: ok (release-1 key ...)",
// "signature: none (why)" or "warning: signature check failed (why); <anyway>".
func signatureLine(r fwsign.Report, noneWhy, anyway string) string {
	switch r.Result {
	case fwsign.ResultOK:
		return "signature: ok (" + r.Detail() + ")"
	case fwsign.ResultNone:
		return "signature: none (" + noneWhy + ")"
	}
	why := r.Result
	if d := r.Detail(); d != "" {
		why += ", " + d
	}
	return "warning: signature check failed (" + why + "); " + anyway
}

// checkFirmwareSignature checks the image about to be flashed against the .sig next to where it
// came from (a local file or a download URL) and prints the result. Report only.
func checkFirmwareSignature(ctx context.Context, location, file string) fwsign.Report {
	image, err := os.ReadFile(file)
	if err != nil {
		return fwsign.Report{Result: fwsign.ResultNone}
	}
	src, name := sigSourceOf(location)
	sig, why := readSig(ctx, src, name)
	r := fwsign.Check(sig, image, fwsign.TargetFirmware)
	fmt.Fprintln(os.Stderr, "flash-self: "+signatureLine(r, why, "flashing anyway"))
	return r
}

// sigSourceOf splits a firmware location (a path or URL) into its directory and file name.
func sigSourceOf(location string) (blobSource, string) {
	if isHTTPURL(location) {
		i := strings.LastIndex(location, "/")
		return blobSource{base: location[:i]}, location[i+1:]
	}
	return blobSource{base: filepath.Dir(location)}, filepath.Base(location)
}

// checkBlobSignature checks one blob before it is installed and prints the result. It returns
// the manifest to hand to the pod: nil when there is none or this host found it wrong.
func checkBlobSignature(ctx context.Context, src blobSource, e blobEntry, data []byte) []byte {
	target, ok := fwsign.TargetByName(e.Name)
	if !ok {
		return nil
	}
	sig, why := readSig(ctx, src, e.File)
	r := fwsign.Check(sig, data, target)
	line := signatureLine(r, why, "installing anyway")
	if strings.HasPrefix(line, "warning: ") {
		line = "warning: " + e.Name + " " + strings.TrimPrefix(line, "warning: ")
	} else {
		line = e.Name + " " + line
	}
	fmt.Fprintln(os.Stderr, "blobs: "+line)
	if !r.Forwardable() {
		return nil
	}
	return r.Manifest.Raw
}

// podSigLine reports what the pod made of a signed upload, "" when there is nothing to say.
func podSigLine(rep serialconsole.SigReport) string {
	if !rep.Supported || rep.Result == "" {
		return ""
	}
	s := "pod signature check: " + rep.Result
	if rep.KeyID != "" {
		s += " (key " + rep.KeyID + ")"
	}
	return s
}
