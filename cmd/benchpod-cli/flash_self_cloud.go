package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/cloudpod"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
)

// flash-self --network over --connection embeddedci:<name> updates the pod through embeddedci.com,
// the path the web app's Update button takes: the CLI uploads the image to
// POST /api/benchpod/devices/{id}/ota (or names a published release there), and the server
// streams it to the pod over the pod's own cloud link, has it verified and committed, and waits
// for the pod to come back. The CLI follows the job with GET .../ota and aborts it on Ctrl-C.
//
// It needs an organization owner or admin (API keys: the benchpod:admin scope), the pod online on
// embeddedci.com and no other session holding its lease. The pod's signature policy applies as on
// any other path: the image's .sig goes with it when there is one.

// Seams for tests: how often the job is read, and how long a dry run's abort may take.
var (
	cloudOTAPoll         = 2 * time.Second
	cloudOTAAbortTimeout = 30 * time.Second
	// resolveCloudPodForFlash finds the pod on embeddedci.com (resolveCloudPod; tests replace it).
	resolveCloudPodForFlash = resolveCloudPod
)

// explicitCloud is the pod name when --connection (or BENCHPOD_CONNECTION) is embeddedci:<name>.
// A saved embeddedci: default does not count, as for the USB-only commands (usbDevice).
func (g *globalFlags) explicitCloud() (string, bool) {
	raw := strings.TrimSpace(g.connection)
	if raw == "" {
		return "", false
	}
	spec, err := parseTarget(raw)
	if err != nil || !spec.IsCloud() {
		return "", false
	}
	return spec.Name, true
}

// runFlashSelfCloud is flash-self through embeddedci.com to the pod called name.
func runFlashSelfCloud(g *globalFlags, f *flashSelfFlags, changed func(string) bool, args []string, name string) error {
	for _, flag := range dfuOnlyFlags {
		if changed(flag) {
			return fmt.Errorf("--%s is for USB DFU and does not apply through embeddedci.com", flag)
		}
	}
	arg := ""
	if len(args) == 1 {
		arg = strings.TrimSpace(args[0])
	}
	// A published release (the default: the latest) is installed by tag: the server downloads it,
	// checks it and installs the blobs that go with it. A file or URL is uploaded as is.
	release := arg == "" && strings.TrimSpace(f.firmwareURL) == ""
	if release && (changed("skip-blobs") || changed("blobs-dir")) {
		return errors.New("--skip-blobs and --blobs-dir do not apply to a release installed through embeddedci.com: " +
			"it installs the release's firmware and blobs together")
	}

	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(flashSelfTimeout))
	defer cancel()
	interrupted, stopSignals := context.WithCancel(ctx)
	defer stopSignals()
	defer installSignalHandler(interrupted, stopSignals)()

	pod, err := resolveCloudPodForFlash(interrupted, name)
	if err != nil {
		return err
	}
	raw, err := pod.Command(interrupted, map[string]any{"cmd": "status"})
	if err != nil {
		return fmt.Errorf("flash-self: %s: %w", pod.Label(), err)
	}
	var st podStatusInfo
	_ = json.Unmarshal(raw, &st)
	fmt.Fprintf(os.Stderr, "flash-self: %s, running firmware %s\n", pod.Label(), valueOr(strings.TrimPrefix(st.Version, "v"), "unknown"))

	req := serverapi.OTARequest{DryRun: f.dryRun}
	if release {
		tag := strings.TrimSpace(f.firmwareVer)
		if tag == "" || tag == "latest" {
			rel, err := pod.API.LatestFirmware(interrupted, pod.Credential)
			if err != nil {
				return fmt.Errorf("flash-self: find the latest release on embeddedci.com: %w", pod.Explain(interrupted, err))
			}
			tag = rel.Tag
		}
		req.Release = tag
		fmt.Fprintf(os.Stderr, "flash-self: embeddedci.com installs release %s (firmware and blobs)\n", tag)
	} else {
		file, location, cleanup, err := f.resolveFirmware(interrupted, arg)
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return err
		}
		image, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("firmware file: %w", err)
		}
		if need := imageNeedsKB(image); st.FlashKB > 0 && need > st.FlashKB {
			return fmt.Errorf("this firmware needs %d KB of flash and the pod has %d KB: it was built for the 2 MB H563; use a newer release", need, st.FlashKB)
		}
		sig := checkFirmwareSignature(interrupted, location, file)
		if err := f.requireSignature(location, sig); err != nil {
			return err
		}
		if sig.Forwardable() {
			req.Signature = sig.Manifest.Raw
		}
		req.Image = image
		fmt.Fprintf(os.Stderr, "flash-self: uploading %d KB to embeddedci.com\n", (len(image)+1023)/1024)
		if !f.skipBlobs {
			fmt.Fprintln(os.Stderr, "flash-self: an uploaded image is the firmware only: the pod keeps its gateware and ESP32-C3 blobs "+
				"(a release installs both: --firmware-version <tag>)")
		}
	}

	job, err := pod.API.StartOTA(interrupted, pod.Credential, pod.DeviceID, req)
	if err != nil {
		return fmt.Errorf("flash-self: embeddedci.com did not start the update: %w", pod.Explain(interrupted, err))
	}
	job, err = followCloudOTA(interrupted, pod, job)
	if err != nil {
		if interrupted.Err() != nil && ctx.Err() == nil {
			return abortCloudOTA(pod, job)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("flash-self: gave up following the update after %s; it may still be running: "+
				"check the pod in the web app: %w", g.effectiveTimeout(flashSelfTimeout), err)
		}
		return err
	}
	reportSignatures(job)
	switch job.Phase {
	case serverapi.OTAPhaseVerified:
		// A dry run ends with the image staged: until it is dropped the pod answers "busy" to
		// captures, so drop it.
		fmt.Fprintln(os.Stderr, "flash-self: dry run: the pod staged the image and its SHA-256 checked out; nothing was written")
		actx, acancel := context.WithTimeout(context.Background(), cloudOTAAbortTimeout)
		defer acancel()
		if err := pod.API.AbortOTA(actx, pod.Credential, pod.DeviceID); err != nil {
			return fmt.Errorf("flash-self: could not drop the staged image (the pod answers busy until it is dropped or restarted): %w",
				pod.Explain(actx, err))
		}
		return nil
	case serverapi.OTAPhaseDone:
		fmt.Fprintf(os.Stderr, "flash-self: success, %s runs firmware %s\n", pod.Name, valueOr(job.VersionAfter, "unknown"))
		return nil
	case serverapi.OTAPhaseAborted:
		return fmt.Errorf("flash-self: the update was aborted%s", errSuffix(job.Error))
	}
	return fmt.Errorf("flash-self: the update failed%s; `benchpod status --connection %s%s` shows the firmware the pod runs",
		errSuffix(job.Error), cloudConnPrefix, pod.Name)
}

// followCloudOTA polls the job until it ends, printing its progress.
func followCloudOTA(ctx context.Context, pod *cloudpod.Client, job *serverapi.OTAJob) (*serverapi.OTAJob, error) {
	last := ""
	for {
		if line := otaProgress(job); line != last {
			fmt.Fprintln(os.Stderr, "flash-self: "+line)
			last = line
		}
		if job.Terminal() {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return job, ctx.Err()
		case <-time.After(cloudOTAPoll):
		}
		next, err := pod.API.GetOTA(ctx, pod.Credential, pod.DeviceID)
		if err != nil {
			if ctx.Err() != nil {
				return job, ctx.Err()
			}
			// A blip while the pod restarts or the server is busy: keep following.
			fmt.Fprintf(os.Stderr, "flash-self: reading the update's progress: %v (retrying)\n", pod.Explain(ctx, err))
			continue
		}
		job = next
	}
}

// otaProgress is one progress line: the phase, the part, and the percent in tens.
func otaProgress(j *serverapi.OTAJob) string {
	line := j.Phase
	if j.Part != "" && j.Part != "firmware" {
		line += " " + j.Part
	}
	if j.Phase == serverapi.OTAPhaseStreaming {
		line += fmt.Sprintf(" %d%%", j.Percent/10*10)
	}
	if j.Phase == serverapi.OTAPhaseCommitting {
		line += " (the pod writes its flash and restarts; do not power it off)"
	}
	if j.Step != "" && j.Live {
		line += ": " + j.Step
	}
	return line
}

// abortCloudOTA is Ctrl-C during an update: the pod drops what it staged, unless it is already
// writing its flash, which cannot be stopped.
func abortCloudOTA(pod *cloudpod.Client, job *serverapi.OTAJob) error {
	if job != nil && job.Phase == serverapi.OTAPhaseCommitting {
		return errors.New("flash-self: interrupted while the pod writes its flash; that cannot be stopped. " +
			"It restarts into the new firmware when done; check it in the web app")
	}
	fmt.Fprintln(os.Stderr, "flash-self: interrupted; aborting the update")
	ctx, cancel := context.WithTimeout(context.Background(), cloudOTAAbortTimeout)
	defer cancel()
	if err := pod.API.AbortOTA(ctx, pod.Credential, pod.DeviceID); err != nil {
		return fmt.Errorf("flash-self: interrupted, and the abort failed (check the pod in the web app): %w", pod.Explain(ctx, err))
	}
	return errors.New("flash-self: interrupted; the update was aborted and the pod keeps its firmware")
}

// reportSignatures prints the server's and the pod's signature checks of what was installed.
func reportSignatures(j *serverapi.OTAJob) {
	for _, s := range j.Signatures {
		line := fmt.Sprintf("flash-self: %s signature: %s on embeddedci.com", valueOr(s.Part, "firmware"), valueOr(s.Server, "none"))
		if s.KeyID != "" {
			line += " (key " + s.KeyID + ")"
		}
		if s.Device != "" {
			line += ", " + s.Device + " on the pod"
		}
		fmt.Fprintln(os.Stderr, line)
	}
}

func errSuffix(msg string) string {
	if strings.TrimSpace(msg) == "" {
		return ""
	}
	return ": " + strings.TrimSpace(msg)
}
